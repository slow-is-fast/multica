// Package gaterefusal 记录委派链闸门的拒绝，让它在**事务之外**仍然可见（#33）。
//
// ## 为什么必须放在事务外
//
// 两条闸门都挂在入队之前的共同收口上，拒绝的方式是**返回 error**，调用方随即回滚整笔
// 事务。任何写在事务里的通知——活动流、评论、timeline——都会跟着一起消失。所以「人看得见」
// 这件事，从根上就不能依赖那笔事务。
//
// 本包把回执记在**进程内**，因此与事务无关：无论调用方最后是 commit 还是 rollback，回执
// 都已经在了。
//
// ## 为什么先是「回执」，推送是后来的事
//
// 拒绝发生时我们处在一笔即将回滚的事务中间，既不能写库，也不该在事务里发外部请求
// （请求成功、事务回滚，就变成一条指向不存在的拒绝的通知）。所以 #33 先做的是**可查询
// 的回执**：人问一句就有，而不是主动弹到面前。
//
// 推送那条事务外的投递通道由 #36 补上（见 push.go）：配一个 webhook，Record 顺手往内存
// 队列丢一条，事务外的 goroutine 去投。两层是分开的——回执回答「刚才发生了什么」，推送
// 回答「没人问的时候也要让人知道」。
//
// ## 一个必须说清的边界
//
// 累计是**进程内**的：拒绝发生在哪个进程，回执就留在哪个进程。委派链的入口有六条
// （见 #25），评论触发的那几条在 API 进程，wakeup 触发的在 daemon 进程。两个进程各自暴露
// 自己的那份，重启即清零——这是有意的取舍，理由同上：要持久化就得在事务外写库，而那需要
// 一条投递通道；在它存在之前，进程内回执至少保证了「拒绝发生过的那一刻，有人能问到」。
package gaterefusal

import (
	"sort"
	"sync"
	"time"
)

// 两个维度。写成常量而不是直接塞字符串，是因为接收方要按维度分流出路——深度耗尽可以换
// 一条链重来，预算是 per_issue 累计、不会归零。
const (
	// DimensionDepth 是「转手次数」维度：封的是代数，换一条链就归零。
	DimensionDepth = "delegation_depth_exceeded"
	// DimensionBudget 是「钱」维度：封的是总量，per_issue 累计不会归零，撞上一次就是
	// 永久的，要人先解释那笔钱花在哪。
	DimensionBudget = "delegation_budget_exceeded"
)

const (
	// DedupeWindow 是这个窗口内的**同一 (Issue, 维度)** 拒绝合并成一条。
	//
	// 为什么必须合并：委派链可以在短时间内密集触发，每次拒绝都发一条，通知自己就成了
	// 噪音——而噪音的下场是被忽略，等于没有通知。
	DedupeWindow = 2 * time.Second

	// maxNotices 是回执条数上限。满了丢最旧的：这是一个「最近发生了什么」的窗口，不是
	// 审计日志。要审计的话该扫库，不该靠这里的内存。
	maxNotices = 200
)

// Notice 是一次闸门拒绝的回执。
//
// 两个维度的字段分开存（指针，nil 表示本条不适用），而不是塞进一个 map：维度决定了哪些
// 数字有意义，而**没有意义的数字显示为 0 会被当成事实**。深度那条没有「花了多少」，预算
// 那条没有「转了几次手」——用 nil 让它们干脆不出现。
type Notice struct {
	Dimension    string    `json:"dimension"`
	Path         string    `json:"path"`
	IssueID      string    `json:"issue_id"`
	AgentID      string    `json:"agent_id"`
	ParentTaskID string    `json:"parent_task_id"`

	// 深度维度
	Depth      *int `json:"depth,omitempty"`
	DepthLimit *int `json:"depth_limit,omitempty"`

	// 预算维度
	SpentUSD     *float64 `json:"spent_usd,omitempty"`
	BudgetUSD    *float64 `json:"budget_usd,omitempty"`
	PricedRows   *int      `json:"priced_rows,omitempty"`
	UnpricedRows *int      `json:"unpriced_rows,omitempty"`

	FirstAt time.Time `json:"first_at"`
	LastAt  time.Time `json:"last_at"`
	// Count 是这个 (Issue, 维度) 在去重窗口内被合并的次数。
	Count int `json:"count"`
}

var (
	mu      sync.Mutex
	notices []*Notice
)

// Record 记一次拒绝。
//
// 同一个 (Issue, 维度) 在 DedupeWindow 内重复出现时合并进已有那一条：计数 +1、时间戳前进、
// 数字刷新为**最后一次**看到的（最近的一次最能说明现状）。其余字段保留第一次的——尤其是
// Path，第一次被拒的入口才是排查时要看的那个。
func Record(n Notice) {
	now := time.Now().UTC()
	mu.Lock()
	defer mu.Unlock()
	if n.FirstAt.IsZero() {
		n.FirstAt = now
	}
	n.LastAt = now
	if n.Count == 0 {
		n.Count = 1
	}
	// 倒序遍历：要看的是**最近**那一条同 key 的回执，不是第一条。正序会先撞到历史上的
	// 旧回执，它必然超出窗口，于是 `break` 掉而漏掉后面那条还在窗口内的。
	for i := len(notices) - 1; i >= 0; i-- {
		existing := notices[i]
		if existing.Dimension != n.Dimension || existing.IssueID != n.IssueID {
			continue
		}
		if now.Sub(existing.LastAt) > DedupeWindow {
			break
		}
		existing.LastAt = now
		existing.Count++
		// 只刷新数字，不刷新 Path：入口变了本身是个信号，但第一次被拒的入口才是那条链
		// 跑歪的起点，不该被后面的覆盖掉。
		if n.Depth != nil {
			existing.Depth = n.Depth
		}
		if n.SpentUSD != nil {
			existing.SpentUSD = n.SpentUSD
		}
		if n.PricedRows != nil {
			existing.PricedRows = n.PricedRows
		}
		if n.UnpricedRows != nil {
			existing.UnpricedRows = n.UnpricedRows
		}
		// 合并进来的那一次也是一次拒绝，推的是**合并后**的状态（Count 已 +1），
		// 接收方因此看得到「这一小段时间里撞了几回」。
		notifyPush(*existing)
		return
	}
	notices = append(notices, &n)
	if len(notices) > maxNotices {
		notices = notices[len(notices)-maxNotices:]
	}
	notifyPush(n)
}

// notifyPush 是 Record 与推送通道之间唯一的接缝（#36）。
//
// 它必须在**持有 notices 的锁、事务之外**被调用：闸门拒绝后调用方会回滚，而队列在
// 进程内存里，回滚带不走它。反过来，这里绝不能碰数据库或发网络请求——它会跑在闸门
// 的返回路径上。
func notifyPush(n Notice) {
	if p := currentPusher(); p != nil {
		p.Enqueue(n)
	}
}

// Snapshot 返回当前回执的副本，按最近发生排序（最新的在前）。
//
// 返回副本而不是切片，是为了让调用方（HTTP handler）不能改动内部状态——这个包的正确性
// 依赖于所有写入都经过 Record。
func Snapshot() []Notice {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Notice, 0, len(notices))
	for _, n := range notices {
		out = append(out, *n)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LastAt.After(out[j].LastAt)
	})
	return out
}

// Reset 清空回执，只给测试用。
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	notices = nil
}
