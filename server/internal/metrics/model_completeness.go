package metrics

// Ruel 新增：用量行的**模型名采集完整性**指标（#34）。
//
// 它是怎么来的：#32 修掉的那条漏采（codex 的用量行模型名恒为字面量 `unknown`）连续
// 污染了本机每一条 codex 用量，但在此期间**没有任何信号指向它**——最后是有人在算账单
// 时才撞见的。这件事的偶然性才是本文件要修的东西：下一次犯同类错误，未必有人正好在
// 算账单。
//
// #32 修的是那个具体的 bug；本文件修的是「这类 bug 能静默存活多久」。
//
// 三条刻意的边界：
//
//  1. **在行落地时计数，不事后扫库**。事后扫库意味着又要有人想起去扫，那就又回到
//     「靠人撞见」的老路。
//  2. **分母是有用量记录的行**。没有用量记录的行属于另一类问题（Run 根本没报），
//     混进来会让这个指标既不像「采没采到」也不像「报没报」。
//  3. **不设阈值**。占比在样本极小时不可信（1/1 = 100%），而本机样本本来就小。先做到
//     可查询、可展示，阈值留给自然流量积累后再定——理由同 6.8.2，不要在没有依据的
//     时候造一个新基准。

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelPlaceholderValues 是已知的「模型名没采到」的占位写法。
//
// 为什么不做成一句 `model == ""`：各 adapter 的写法不统一，#32 里 codex 写的就是字面量
// `unknown`。这个集合漏一个，指标就漏一整类，而且漏得很安静——它会把「全灭」显示成
// 「一切正常」。
//
// **新增占位值时必须同步进这个集合。** 靠约定守不住，所以放在这里当常量：新加一种
// 写法的人会路过这个文件。
var ModelPlaceholderValues = []string{
	"",        // 空串：adapter 没填
	"unknown", // #32：codex 在拿不到会话文件时的兜底字面量
}

// IsPlaceholderModel 判定一个模型名是不是「没采到」的占位值。
//
// 大小写不敏感、两端空白不计：占位写法是各 adapter 各自拼出来的，不该让 `Unknown` 和
// `unknown` 被算成两件事。
//
// `auto` **不是**占位值——它是一个真实的通用模型 id（见用量落地处对 provider 的回填
// 注释），把它算进来会让 codex 的正常行永久报警。
func IsPlaceholderModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, placeholder := range ModelPlaceholderValues {
		if m == placeholder {
			return true
		}
	}
	return false
}

// ProviderModelCompleteness 是一个 provider 的模型名采集完整度。
//
// Rows 是分母（落到库里的用量行），MissingRows 是其中模型名为占位值的行数。比值不在这里
// 算成百分数的理由见本文件顶部第 3 条：Rows 很小的比值没有意义，而把「没有意义」编码成
// 一个 0 会被读成「没有缺失」。调用方拿到两个计数自己判断，展示层负责在 Rows == 0 时
// 显示「—」而不是 0%。
type ProviderModelCompleteness struct {
	Provider    string `json:"provider"`
	Rows        int64  `json:"rows"`
	MissingRows int64  `json:"missing_rows"`
}

// MissingRatio 返回缺失占比，Rows 为 0 时返回 false（比值不可用）。
//
// 单独给一个 bool 而不是返回 NaN 或 -1：`0` 会被读成「没有缺失」，`-1` 会被当成比值的
// 一种取值参与排序。调用方要显式处理「算不出来」。
func (p ProviderModelCompleteness) MissingRatio() (float64, bool) {
	if p.Rows <= 0 {
		return 0, false
	}
	return float64(p.MissingRows) / float64(p.Rows), true
}

// ModelCompletenessSnapshot 是一次快照。
type ModelCompletenessSnapshot struct {
	GeneratedAt time.Time                   `json:"generated_at"`
	Providers   []ProviderModelCompleteness `json:"providers"`
	// Totals 是所有 provider 的合计，用来回答「整体上采得怎么样」。
	// 它不等于各 provider 占比的平均——那样会把一条样本的行和一万条样本的行等权。
	Totals ProviderModelCompleteness `json:"totals"`
}

// ModelCompleteness 按 provider 累计用量行的模型名完整性。
//
// 进程内累计，**不落库**：它衡量的是「这一路进程看到的用量行」，重启即清零。这是有意的
// 取舍——落库就要引入一张表和一条迁移，而本指标要回答的是「此刻有没有在漏」，不是
// 「历史上漏了多少」；后者扫 `task_usage.model` 就能得到。
type ModelCompleteness struct {
	mu         sync.Mutex
	byProvider map[string]*completenessCounter
}

type completenessCounter struct {
	rows    int64
	missing int64
}

// Observe 在**一条用量行落地之后**调用。
func (m *ModelCompleteness) Observe(provider, model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byProvider == nil {
		m.byProvider = make(map[string]*completenessCounter)
	}
	c := m.byProvider[provider]
	if c == nil {
		c = &completenessCounter{}
		m.byProvider[provider] = c
	}
	c.rows++
	if IsPlaceholderModel(model) {
		c.missing++
	}
}

// Snapshot 返回当前累计值，按 provider 名升序，保证输出稳定可比。
func (m *ModelCompleteness) Snapshot() ModelCompletenessSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := ModelCompletenessSnapshot{
		GeneratedAt: time.Now().UTC(),
		Providers:   make([]ProviderModelCompleteness, 0, len(m.byProvider)),
	}
	for provider, c := range m.byProvider {
		out.Providers = append(out.Providers, ProviderModelCompleteness{
			Provider:    provider,
			Rows:        c.rows,
			MissingRows: c.missing,
		})
		out.Totals.Rows += c.rows
		out.Totals.MissingRows += c.missing
	}
	sort.Slice(out.Providers, func(i, j int) bool {
		return out.Providers[i].Provider < out.Providers[j].Provider
	})
	return out
}

// Reset 清空累计值，只给测试用。
func (m *ModelCompleteness) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byProvider = nil
}

// DefaultModelCompleteness 是进程级的采集器，用量行落地处直接记到这里。
//
// 做成包级单例而不是依赖注入，是因为用量行的落地点在一个 handler 里，而 handler 拿不到
// 「这条用量属于哪个 provider 的采集器」这种上下文——它只有 provider 字符串。
var DefaultModelCompleteness = &ModelCompleteness{}
