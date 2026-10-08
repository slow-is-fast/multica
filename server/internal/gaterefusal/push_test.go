package gaterefusal

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// receiver 是一个最小接收端：只记录收到的请求体，不做任何断言。
type receiver struct {
	mu      sync.Mutex
	bodies  []map[string]any
	failAll bool
	status  int
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	r.mu.Lock()
	r.bodies = append(r.bodies, payload)
	r.mu.Unlock()
	if r.failAll {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *receiver) last() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		return nil
	}
	return r.bodies[len(r.bodies)-1]
}

func newTestPusher(t *testing.T, url string, mutate func(*PushConfig)) (*Pusher, *receiver) {
	t.Helper()
	rec := &receiver{status: http.StatusOK}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	if url == "" {
		url = srv.URL
	}
	cfg := PushConfig{
		URL:            url,
		Process:        "test",
		MaxAttempts:    3,
		InitialBackoff: time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
		Timeout:        time.Second,
		ShutdownGrace:  100 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewPusher(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))), rec
}

func intField(t *testing.T, payload map[string]any, path ...string) int {
	t.Helper()
	cur := any(payload)
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("expected a map at %q in %v", key, path)
		}
		cur = m[key]
	}
	f, ok := cur.(float64)
	if !ok {
		t.Fatalf("field %v is %T, want a number", path, cur)
	}
	return int(f)
}

// TestRecordFeedsThePushChannel 是那条接缝本身：闸门记一次拒绝，就必须有一条
// 进到推送队列。变异校验的落点也在这里——把 Record 里那次 Enqueue 删掉，本测试变红。
func TestRecordFeedsThePushChannel(t *testing.T) {
	Reset()
	p, rec := newTestPusher(t, "", nil)
	installPusher(p)
	t.Cleanup(func() { installPusher(nil); Reset() })

	depth, limit := 12, 10
	Record(Notice{
		Dimension:  DimensionDepth,
		Path:       "comment",
		IssueID:    "issue-1",
		Depth:      &depth,
		DepthLimit: &limit,
	})

	p.deliverDue(context.Background())

	if rec.count() != 1 {
		t.Fatalf("receiver got %d requests, want 1", rec.count())
	}
	payload := rec.last()
	if payload["event"] != "gate_refusal" {
		t.Fatalf("event = %v, want gate_refusal", payload["event"])
	}
	if payload["process"] != "test" {
		t.Fatalf("process = %v, want test", payload["process"])
	}
	if got := intField(t, payload, "notice", "count"); got != 1 {
		t.Fatalf("notice.count = %d, want 1", got)
	}
	if got := intField(t, payload, "notice", "depth"); got != depth {
		t.Fatalf("notice.depth = %d, want %d", got, depth)
	}
	if p.Status().Delivered != 1 {
		t.Fatalf("delivered = %d, want 1", p.Status().Delivered)
	}
}

// TestUndeliveredRefusalsCoalesce 守的是「还没告诉过人家的事，不该告诉两遍」。
//
// 一次密集的委派链重试会在去重窗口内撞出好几次拒绝，回执合并成一条（#33）。推送如果
// 每次都发，噪音会把通道自己废掉；所以尚未投递的条目按 (Issue, 维度) 覆盖合并。
func TestUndeliveredRefusalsCoalesce(t *testing.T) {
	Reset()
	p, rec := newTestPusher(t, "", nil)
	installPusher(p)
	t.Cleanup(func() { installPusher(nil); Reset() })

	spent, budget := 2.0, 1.7654
	priced, unpriced := 2, 0
	for i := 0; i < 3; i++ {
		Record(Notice{
			Dimension:    DimensionBudget,
			Path:         "comment",
			IssueID:      "issue-2",
			SpentUSD:     &spent,
			BudgetUSD:    &budget,
			PricedRows:   &priced,
			UnpricedRows: &unpriced,
		})
	}
	if got := p.Status().Pending; got != 1 {
		t.Fatalf("pending = %d, want 1 (three refusals coalesced into one)", got)
	}

	p.deliverDue(context.Background())

	if rec.count() != 1 {
		t.Fatalf("receiver got %d requests, want 1", rec.count())
	}
	if got := intField(t, rec.last(), "notice", "count"); got != 3 {
		t.Fatalf("notice.count = %d, want 3 — the receiver must see the burst, not just its first hit", got)
	}
}

// TestAlreadyDeliveredRefusalsDoNotCoalesce 守的是合并的另一半：投出去之后再来
// 一次拒绝，是一次新的拒绝，接收方有权知道。
func TestAlreadyDeliveredRefusalsDoNotCoalesce(t *testing.T) {
	Reset()
	p, rec := newTestPusher(t, "", nil)
	installPusher(p)
	t.Cleanup(func() { installPusher(nil); Reset() })

	Record(Notice{Dimension: DimensionDepth, Path: "comment", IssueID: "issue-3"})
	p.deliverDue(context.Background())
	Record(Notice{Dimension: DimensionDepth, Path: "comment", IssueID: "issue-3"})
	p.deliverDue(context.Background())

	if rec.count() != 2 {
		t.Fatalf("receiver got %d requests, want 2", rec.count())
	}
}

// TestPushGivesUpAfterMaxAttempts 守的是「不许无限重试」：一个配错的 URL 不能
// 变成永久的背景流量。
func TestPushGivesUpAfterMaxAttempts(t *testing.T) {
	Reset()
	p, rec := newTestPusher(t, "", func(cfg *PushConfig) { cfg.MaxAttempts = 2 })
	rec.failAll = true
	installPusher(p)
	t.Cleanup(func() { installPusher(nil); Reset() })

	Record(Notice{Dimension: DimensionDepth, Path: "comment", IssueID: "issue-4"})

	for i := 0; i < 6 && p.Status().Pending > 0; i++ {
		p.deliverDue(context.Background())
		time.Sleep(5 * time.Millisecond)
	}

	st := p.Status()
	if st.Delivered != 0 {
		t.Fatalf("delivered = %d, want 0", st.Delivered)
	}
	if st.Dropped != 1 {
		t.Fatalf("dropped = %d, want 1", st.Dropped)
	}
	if st.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (MaxAttempts)", st.Attempts)
	}
	if st.LastError == "" {
		t.Fatal("last_error must be populated so `multica gate-refusals` can show why")
	}
	if !strings.Contains(st.LastError, "500") {
		t.Fatalf("last_error = %q, want it to name the status", st.LastError)
	}
}

// TestUnconfiguredPushMeansNoTraffic 守的是关闭态：不配 URL 时，行为必须和
// #36 之前完全一致——没有 goroutine、没有流量、回执端点照常。
func TestUnconfiguredPushMeansNoTraffic(t *testing.T) {
	Reset()
	p, rec := newTestPusher(t, "", func(cfg *PushConfig) { cfg.URL = "" })
	installPusher(p)
	t.Cleanup(func() { installPusher(nil); Reset() })

	Record(Notice{Dimension: DimensionDepth, Path: "comment", IssueID: "issue-5"})
	p.deliverDue(context.Background())
	p.Run(context.Background())

	if rec.count() != 0 {
		t.Fatalf("unconfigured pusher sent %d requests, want 0", rec.count())
	}
	if p.Status().Configured {
		t.Fatal("Configured must be false when no URL is set")
	}
	if len(Snapshot()) != 1 {
		t.Fatalf("receipt must still work without a push channel; got %d notices", len(Snapshot()))
	}
}

// TestRunDeliversWithoutAnyoneAsking 是这条 Issue 的验收判据本身：配好 webhook
// 之后，拒绝会在**不查任何端点**的情况下被推到目标地址。
func TestRunDeliversWithoutAnyoneAsking(t *testing.T) {
	Reset()
	p, rec := newTestPusher(t, "", nil)
	installPusher(p)
	t.Cleanup(func() { installPusher(nil); Reset() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	Record(Notice{Dimension: DimensionBudget, Path: "wakeup", IssueID: "issue-6"})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && rec.count() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if rec.count() == 0 {
		t.Fatal("Run never delivered the refusal to the webhook")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
}

// TestShutdownDrainsWhatItCan 守的是退出路径：剩下的条目要补投一次，且这条路径
// 不被一个坏掉的 URL 拖住。
func TestShutdownDrainsWhatItCan(t *testing.T) {
	Reset()
	p, rec := newTestPusher(t, "", nil)
	installPusher(p)
	t.Cleanup(func() { installPusher(nil); Reset() })

	// 不入队即关闭：worker 退出时把这两条补投出去。
	Record(Notice{Dimension: DimensionDepth, Path: "comment", IssueID: "issue-7"})
	Record(Notice{Dimension: DimensionDepth, Path: "comment", IssueID: "issue-8"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻让 Run 走退出分支
	p.Run(ctx)

	if rec.count() != 2 {
		t.Fatalf("drained %d requests, want 2", rec.count())
	}
	if got := p.Status().Delivered; got != 2 {
		t.Fatalf("delivered = %d, want 2", got)
	}
}

func TestPushStatusReportsUnconfiguredWhenNoPusherIsInstalled(t *testing.T) {
	installPusher(nil)
	st := PushStatusSnapshot()
	if st.Configured {
		t.Fatal("no pusher installed: Configured must be false")
	}
	if st.LastError != "" || st.Delivered != 0 {
		t.Fatalf("unconfigured status must be empty, got %+v", st)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	cfg := PushConfig{InitialBackoff: time.Second, MaxBackoff: 5 * time.Second}
	got := []time.Duration{
		backoff(cfg, 1),
		backoff(cfg, 2),
		backoff(cfg, 3),
		backoff(cfg, 10),
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff(attempt %d) = %s, want %s", i+1, got[i], want[i])
		}
	}
}
