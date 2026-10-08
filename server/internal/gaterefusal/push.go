package gaterefusal

// Ruel 新增：闸门拒绝的**推送**通道（#36）。
//
// #33 把拒绝记成了事务外的回执，但回执是**拉模型**：得有人想起来去查。而闸门拒绝恰恰
// 发生在没人的时候——委派链可以半夜自己转手十几次。本文件补的是推的那一半。
//
// ## 为什么仍然必须在事务外
//
// 和 #33 同一个根因：拒绝的方式是返回 error，调用方随即回滚。在闸门里直接发 HTTP 请求
// 有两个坏处：一是把一个外部请求的延迟塞进一笔正在回滚的事务；二是请求成功了、事务回滚
// 了，就变成一条指向不存在的拒绝的通知。
//
// 所以链路是：闸门里往内存队列丢一条，事务外的 goroutine 去投。队列在进程内，回滚带不走
// 它。
//
// ## 未投递的合并
//
// 同一 (Issue, 维度) 的拒绝，如果上一条**还没投出去**，就用新的覆盖它，而不是再排一条。
// 语义很简单：还没告诉过人家的事，不该告诉两遍。已经投出去的不再合并——那是一次新的拒绝，
// 接收方有权知道。
//
// ## 不做什么
//
// 不持久化。进程重启会丢掉没投出去的拒绝，这是有意的取舍：要持久化就得有表、有重放、有
// 幂等键，那是另一个量级的改动。收据端点（#33）仍然是「那一瞬间有人能问到」的那一层。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// pushEventName 是推送体里的事件类型，接收方靠它分流。
	pushEventName = "gate_refusal"

	// pushQueueCap 是内存队列上限。满了就丢最旧的一条：这是一个「刚刚发生了什么」的
	// 通道，不是审计日志。丢掉的会在 Dropped 里计数，不会静默消失。
	pushQueueCap = 512
)

// PushConfig 是推送通道的配置。零值即关闭。
type PushConfig struct {
	// URL 为空就是关闭。整个推送通道的开关只有这一个。
	URL   string
	Token string
	// Process 标注这条推送来自哪个进程（"api" / "daemon"）。两个进程各自起自己的
	// 投递器，接收方需要能分清是谁在说话。
	Process string

	MaxAttempts   int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Timeout        time.Duration
	// ShutdownGrace 是退出时补投队列里剩余条目的时间预算。
	ShutdownGrace time.Duration
}

// PushConfigFromEnv 读环境变量。与仓库既有做法一致（.dev-env）：配一个 URL 就开，
// 不配就完全不发生——没有 goroutine，没有流量。
func PushConfigFromEnv(process string) PushConfig {
	cfg := PushConfig{
		URL:            strings.TrimSpace(os.Getenv("MULTICA_GATE_REFUSAL_WEBHOOK_URL")),
		Token:          strings.TrimSpace(os.Getenv("MULTICA_GATE_REFUSAL_WEBHOOK_TOKEN")),
		Process:        process,
		MaxAttempts:    envPositiveInt("MULTICA_GATE_REFUSAL_WEBHOOK_MAX_ATTEMPTS", 4),
		InitialBackoff: envPositiveDuration("MULTICA_GATE_REFUSAL_WEBHOOK_BACKOFF", 500*time.Millisecond),
		MaxBackoff:     envPositiveDuration("MULTICA_GATE_REFUSAL_WEBHOOK_MAX_BACKOFF", 30*time.Second),
		Timeout:        envPositiveDuration("MULTICA_GATE_REFUSAL_WEBHOOK_TIMEOUT", 5*time.Second),
		ShutdownGrace:  envPositiveDuration("MULTICA_GATE_REFUSAL_WEBHOOK_SHUTDOWN_GRACE", 2*time.Second),
	}
	if cfg.MaxBackoff < cfg.InitialBackoff {
		cfg.MaxBackoff = cfg.InitialBackoff
	}
	return cfg
}

// Enabled 报告这个配置要不要真的投。
func (c PushConfig) Enabled() bool { return c.URL != "" }

// PushStatus 是推送通道的自检面。配错 URL 时，这是唯一的发现手段——一个静默
// 失败的推送通道比没有通道更糟，因为它让人以为有人在看着。
type PushStatus struct {
	Configured bool   `json:"configured"`
	URL        string `json:"url,omitempty"`
	Process    string `json:"process,omitempty"`
	Pending    int    `json:"pending"`
	Delivered  int64  `json:"delivered"`
	Dropped    int64  `json:"dropped"`
	Attempts   int64  `json:"attempts"`
	LastError  string `json:"last_error,omitempty"`
	LastAt     string `json:"last_at,omitempty"`
}

// pendingItem 是队列里的一条。Attempts 决定退避多久，NextAt 决定什么时候再试。
type pendingItem struct {
	notice   Notice
	attempts int
	nextAt   time.Time
}

// Pusher 把闸门拒绝投到一个 webhook。零配置时 Enqueue 是空操作。
type Pusher struct {
	cfg    PushConfig
	client *http.Client
	logger *slog.Logger

	mu     sync.Mutex
	notify chan struct{}
	queue  []*pendingItem
	closed bool

	delivered int64
	dropped   int64
	attempts  int64
	lastErr   string
	lastAt    time.Time
}

// NewPusher 建一个投递器。未配置时返回的实例也是可用的，只是什么都不做。
func NewPusher(cfg PushConfig, logger *slog.Logger) *Pusher {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Pusher{
		cfg:    cfg,
		logger: logger,
		client: &http.Client{Timeout: cfg.Timeout},
		notify: make(chan struct{}, 1),
	}
	return p
}

// signal 唤醒 worker。非阻塞：队列非空时 worker 本来就要跑一次，多一次唤醒无害，
// 而阻塞在这里会把闸门的入队路径拖住。
func (p *Pusher) signal() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// Enqueue 把一次拒绝排进队列。
//
// 它在闸门里被调用，因此必须快、必须不阻塞、必须不碰数据库：唯一做的事是往内存
// 里塞一条并唤醒 worker。
func (p *Pusher) Enqueue(n Notice) {
	if p == nil || !p.cfg.Enabled() {
		return
	}
	key := n.Dimension + "\x00" + n.IssueID
	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	for i, item := range p.queue {
		if item.attempts > 0 {
			// 已经投过一次了——这是一次新的拒绝，不再合并。
			continue
		}
		if item.notice.Dimension+"\x00"+item.notice.IssueID != key {
			continue
		}
		p.queue[i].notice = n
		p.signal()
		return
	}
	p.queue = append(p.queue, &pendingItem{notice: n, nextAt: now})
	if len(p.queue) > pushQueueCap {
		p.queue = p.queue[1:]
		p.dropped++
	}
	p.signal()
}

// Status 返回当前状态。Configured 为 false 时其余字段无意义。
func (p *Pusher) Status() PushStatus {
	if p == nil {
		return PushStatus{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := PushStatus{
		Configured: p.cfg.Enabled(),
		URL:        p.cfg.URL,
		Process:    p.cfg.Process,
		Pending:    len(p.queue),
		Delivered:  p.delivered,
		Dropped:    p.dropped,
		Attempts:   p.attempts,
		LastError:  p.lastErr,
	}
	if !p.lastAt.IsZero() {
		st.LastAt = p.lastAt.UTC().Format(time.RFC3339)
	}
	return st
}

// Run 是 worker 循环，阻塞直到 ctx 结束。退出前尽 ShutdownGrace 的预算补投剩下的。
func (p *Pusher) Run(ctx context.Context) {
	if p == nil || !p.cfg.Enabled() {
		return
	}
	p.logger.Info("gate refusal webhook push enabled", "url", p.cfg.URL, "process", p.cfg.Process)

	for {
		wait := p.nextWakeup()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			p.drainBestEffort()
			return
		case <-timer.C:
		case <-p.notify:
			timer.Stop()
		}
		p.deliverDue(ctx)
	}
}

// nextWakeup 返回下次该醒来的间隔。队列空时返回一个很长的间隔，靠入队唤醒。
func (p *Pusher) nextWakeup() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	const idleWait = time.Hour
	if len(p.queue) == 0 {
		return idleWait
	}
	next := p.queue[0].nextAt
	for _, item := range p.queue[1:] {
		if item.nextAt.Before(next) {
			next = item.nextAt
		}
	}
	wait := time.Until(next)
	if wait < 0 {
		return 0
	}
	return wait
}

// deliverDue 把所有到期的条目串行投出去。
func (p *Pusher) deliverDue(ctx context.Context) {
	for {
		item := p.takeDue()
		if item == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		attempt := item.attempts + 1
		p.attempts++
		p.lastAt = time.Now().UTC()
		err := p.post(ctx, item.notice, attempt)
		if err == nil {
			p.mu.Lock()
			p.delivered++
			p.lastErr = ""
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		p.lastErr = err.Error()
		if attempt >= p.cfg.MaxAttempts {
			// 到上限就丢，并计数。一个配错的 URL 不该变成永久的背景流量。
			p.dropped++
			p.mu.Unlock()
			p.logger.Warn("gate refusal webhook push abandoned after max attempts",
				"attempts", attempt, "error", err)
			continue
		}
		item.attempts = attempt
		item.nextAt = time.Now().Add(backoff(p.cfg, attempt))
		p.queue = append(p.queue, item)
		p.mu.Unlock()
		p.logger.Warn("gate refusal webhook push failed; will retry",
			"attempt", attempt, "retry_in", backoff(p.cfg, attempt), "error", err)
	}
}

// takeDue 弹出一条到期未投的条目。
func (p *Pusher) takeDue() *pendingItem {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for i, item := range p.queue {
		if item.nextAt.After(now) {
			continue
		}
		p.queue = append(p.queue[:i], p.queue[i+1:]...)
		return item
	}
	return nil
}

// drainBestEffort 在退出前把队列里剩下的补投一次，不再重试。
//
// 「尽量」是有界的：退出路径不该被一个配错的 URL 拖住，所以给一个时间预算，超时
// 就停，剩下的在 Dropped 里体现。
func (p *Pusher) drainBestEffort() {
	p.mu.Lock()
	remaining := len(p.queue)
	p.closed = true
	p.mu.Unlock()
	if remaining == 0 {
		return
	}
	p.logger.Info("draining gate refusal push queue before shutdown", "pending", remaining)

	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.ShutdownGrace)
	defer cancel()
	for {
		item := p.takeDue()
		if item == nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if err := p.post(ctx, item.notice, item.attempts+1); err != nil {
			p.mu.Lock()
			p.dropped++
			p.lastErr = err.Error()
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		p.delivered++
		p.mu.Unlock()
	}
}

func (p *Pusher) post(ctx context.Context, n Notice, attempt int) error {
	body, err := json.Marshal(map[string]any{
		"event":   pushEventName,
		"process": p.cfg.Process,
		"sent_at": time.Now().UTC(),
		"attempt": attempt,
		"notice":  n,
	})
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "multica-gate-refusal/1")
	if p.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", p.cfg.URL, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("post %s: unexpected status %d", p.cfg.URL, resp.StatusCode)
	}
	return nil
}

// backoff 是第 attempt 次失败后要等多久。指数退避，有上限。
func backoff(cfg PushConfig, attempt int) time.Duration {
	d := cfg.InitialBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= cfg.MaxBackoff {
			return cfg.MaxBackoff
		}
	}
	if d > cfg.MaxBackoff {
		return cfg.MaxBackoff
	}
	return d
}

// ---------------- 进程级单例 ----------------

var (
	pushMu  sync.Mutex
	active  *Pusher
	activeC context.CancelFunc
)

// StartPushFromEnv 按环境变量启动本进程的推送通道，返回停止函数。
//
// 未配置时返回的函数是空操作，并且**不会**起 goroutine。
func StartPushFromEnv(ctx context.Context, process string, logger *slog.Logger) func() {
	cfg := PushConfigFromEnv(process)
	if !cfg.Enabled() {
		return func() {}
	}
	p := NewPusher(cfg, logger)
	runCtx, cancel := context.WithCancel(ctx)

	pushMu.Lock()
	active = p
	activeC = cancel
	pushMu.Unlock()

	go p.Run(runCtx)
	return func() {
		pushMu.Lock()
		active = nil
		activeC = nil
		pushMu.Unlock()
		cancel()
	}
}

// PushStatusSnapshot 返回本进程推送通道的状态。未配置时只有 Configured=false。
func PushStatusSnapshot() PushStatus {
	pushMu.Lock()
	p := active
	pushMu.Unlock()
	if p == nil {
		return PushStatus{}
	}
	return p.Status()
}

// currentPusher 是 Record 的下游。未配置时返回 nil，Record 因此什么都不做。
func currentPusher() *Pusher {
	pushMu.Lock()
	defer pushMu.Unlock()
	return active
}

// installPusher 只给测试用：把某个投递器设为 Record 的下游。
func installPusher(p *Pusher) {
	pushMu.Lock()
	defer pushMu.Unlock()
	active = p
}

func envPositiveInt(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func envPositiveDuration(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
