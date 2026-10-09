package service

// Ruel 新增：PRD 6.8 三层预算的最外一层——**per_agent 与 per_workspace 的日 / 月
// 累计上限**（#41）。
//
// 三层预算至此齐了，三者的形状不同，不能互相替代：
//
//   - per_issue（#28）：封一条 Issue 上的总量，**不归零**，所以必须留「人显式放行」
//     的逃生门（人自己派一次单即放行），否则一触发就永久封死那条 Issue。
//   - per_run（#40）：封一轮 Run，每轮归零，触限时**取消这一轮**。
//   - per_agent / per_workspace（本文件）：封**周期性总量**，按日或月归零。会归零
//     就不存在「永久封死」，所以这里**不需要** per_issue 那道逃生门——但它必须把
//     「周期边界」定义清楚，否则会出现「刚过零点算进哪一天」这类扯皮。
//
// ## 周期边界（判据要求写清楚，三条都在这里）
//
// **时区取 UTC。** 不是「服务器本地时区」：同一时刻在不同时区的实例上会落进不同的
// 周期，同一个上限在两个实例上给出不同的答案；而且本地时区的「日」会随夏令时变成
// 23 或 25 小时。UTC 是唯一能让「日」恒等于 24 小时、且处处一致的选择。
//
// **月 = 自然月**，从当月 1 日 00:00:00 UTC 到次月 1 日 00:00:00 UTC。**不是「最近
// 30 天」的滚动窗口**：滚动窗口的终点一直在动，人没法说清「这个月花了多少」，而
// 预算是要给人看的。
//
// **重置时刻的并发：没有这个时刻。** 这是本实现最要紧的一个选择——**不维护会归零的
// 计数器，而是在判定时按窗口重新聚合**。所以：
//
//   - 不需要定时任务去清零，也就没有「清零跑到一半」的中间态；
//   - 多个 API 实例各自算，结果一致，不需要跨进程同步；
//   - 上限调小后立刻生效，不用等下一个周期。
//
// 代价是每次判定要扫窗口内的用量行（`idx_task_usage_created_at` 可用）。这笔交易是
// 值得的：一个会归零的计数器同时要解决「谁来清零、清到一半怎么办、多个实例谁说了
// 算」三个问题，而它们全都是我们自找的。
//
// ## 四个维度独立判定，任一触限即停
//
// agent_day / agent_month / workspace_day / workspace_month 各自一个上限，**不合并成
// 一个「综合分」**：合并之后没人说得清到底是因为什么停的，也就没法决定该调哪个上限。
// 这与 #28 里「深度与预算不合并」是同一条纪律。
//
// ## 上限不给默认值
//
// 按边界决策第 4 节：凡本机样本算出来的数，进功能时一律转成规则而不是常数。这里的
// 上限只从环境变量读，源码里没有任何数值常量。未配置即不设防，且界面必须明示。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/gaterefusal"
	"github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrPeriodBudgetExceeded 是周期预算闸门拒绝派单时返回的哨兵错误。
//
// 与 ErrDelegationBudgetExceeded 同一条纪律：调用方要把它翻译成 reason code 回到调用
// 方，不能当普通错误吞掉——静默丢弃是把「跑飞」换成「哑火」，两者都没人知道。
var ErrPeriodBudgetExceeded = errors.New("periodic cost budget exhausted")

// 四个维度的环境变量名。全都不给默认值：没配就是不设防。
const (
	EnvAgentDailyBudgetUSD       = "MULTICA_AGENT_DAILY_BUDGET_USD"
	EnvAgentMonthlyBudgetUSD     = "MULTICA_AGENT_MONTHLY_BUDGET_USD"
	EnvWorkspaceDailyBudgetUSD   = "MULTICA_WORKSPACE_DAILY_BUDGET_USD"
	EnvWorkspaceMonthlyBudgetUSD = "MULTICA_WORKSPACE_MONTHLY_BUDGET_USD"
)

// PeriodKind 是周期的种类。
type PeriodKind string

const (
	PeriodDay   PeriodKind = "day"
	PeriodMonth PeriodKind = "month"
)

// PeriodBudgetScope 是预算的作用域。四个维度 = 两个作用域 × 两种周期。
type PeriodBudgetScope string

const (
	ScopeAgent     PeriodBudgetScope = "agent"
	ScopeWorkspace PeriodBudgetScope = "workspace"
)

// PeriodBudgetDimension 唯一确定一个维度。
// JSON 名字全部显式写出，不靠 Go 的默认字段名推断。
//
// 为什么必须显式：消费方（CLI、以及 #43 的界面）写的是 snake_case tag，而 Go 的 json
// 匹配虽然忽略大小写、却**不忽略下划线**——`parse_error` 永远匹配不上 `ParseError`，
// 结果是字段静默为零值、界面上把「配错了」显示成「未设上限」。这类错误没有任何报错，
// 只能靠形状对齐来防。
type PeriodBudgetDimension struct {
	Scope  PeriodBudgetScope `json:"scope"`
	Period PeriodKind       `json:"period"`
}

// Key 是回执与展示位上用的维度名，形如 `agent_daily` / `workspace_monthly`。
func (d PeriodBudgetDimension) Key() string {
	period := "daily"
	if d.Period == PeriodMonth {
		period = "monthly"
	}
	return string(d.Scope) + "_" + period
}

// EnvVar 是这个维度读哪个环境变量。
func (d PeriodBudgetDimension) EnvVar() string {
	switch d.Scope {
	case ScopeAgent:
		if d.Period == PeriodMonth {
			return EnvAgentMonthlyBudgetUSD
		}
		return EnvAgentDailyBudgetUSD
	default:
		if d.Period == PeriodMonth {
			return EnvWorkspaceMonthlyBudgetUSD
		}
		return EnvWorkspaceDailyBudgetUSD
	}
}

// AllPeriodBudgetDimensions 是四个维度，顺序固定：先日再月（便宜的、窗口小的先判）。
//
// 顺序不是随便排的：日窗口的行数远少于月窗口，先判日可以在大多数触限情况下省掉一次
// 全月扫描。这与 #28 里「深度闸门排在预算闸门之前」是同一条理由。
var AllPeriodBudgetDimensions = []PeriodBudgetDimension{
	{ScopeAgent, PeriodDay},
	{ScopeAgent, PeriodMonth},
	{ScopeWorkspace, PeriodDay},
	{ScopeWorkspace, PeriodMonth},
}

// PeriodWindow 是一个周期窗口的**闭开区间** `[Start, End)`。
//
// 半开是关键：闭区间会让落在边界瞬间的那一行同时属于两个周期（被算两次），开区间会
// 让它两边都不算（被漏掉）。`[Start, End)` 让每一行**恰好**属于一个周期——而且下一
// 个周期的 Start 就是这一个的 End，边界唯一，不需要在两处各写一遍。
type PeriodWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// PeriodStart 算出 `at` 所处周期的起点（UTC）。
//
// 日：当天 00:00:00 UTC。月：当月 1 日 00:00:00 UTC。
//
// 用 `time.Date(...)` 显式构造而不是做时间加减，是因为**加减会漂**：`at.AddDate(0,1,0)`
// 在 1 月 31 日会得到 3 月 3 日（Go 会归一化），而 `time.Date` 直接给出「下个月 1 日」
// 这个我们要的语义。
func PeriodStart(at time.Time, kind PeriodKind) time.Time {
	utc := at.UTC()
	if kind == PeriodMonth {
		return time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

// PeriodEnd 算出 `at` 所处周期的终点（不含）。
func PeriodEnd(at time.Time, kind PeriodKind) time.Time {
	utc := at.UTC()
	if kind == PeriodMonth {
		return time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	}
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

// periodWindow 是上面两个函数的合体，给调用方省掉一次拼接。
func periodWindow(at time.Time, kind PeriodKind) PeriodWindow {
	return PeriodWindow{Start: PeriodStart(at, kind), End: PeriodEnd(at, kind)}
}

// periodBudget 是一个维度的配置读取结果。
//
// 三态：未配置 / 已配置 / 配错了。与 #40 的 RunCostBudget 同形，理由也相同——「没设
// 上限」和「设了一个读不懂的值」在界面上必须能区分，否则配错的人会以为自己设防了。
type periodBudget struct {
	USD        float64
	Configured bool
	ParseError string
}

func periodBudgetFromEnv(envVar string) periodBudget {
	raw, ok := os.LookupEnv(envVar)
	if !ok || raw == "" {
		return periodBudget{}
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return periodBudget{ParseError: fmt.Sprintf("%s=%q is not a number: %v", envVar, raw, err)}
	}
	if v < 0 {
		return periodBudget{ParseError: fmt.Sprintf("%s=%q is negative", envVar, raw)}
	}
	return periodBudget{USD: v, Configured: true}
}

// PeriodCost 是一个维度在一个周期内累计的折算成本。
type PeriodCost struct {
	PricedUSD    float64
	PricedRows   int
	UnpricedRows int
}

// PeriodBudgetConfig 给展示位用：四个维度各自的设防情况与窗口边界。
//
// 窗口边界也带出去，是因为「周期边界」这件事本身要能被人看见——只显示一个上限数字，
// 人还是不知道「今天」到底算到几点。
type PeriodBudgetConfig struct {
	Dimension PeriodBudgetDimension `json:"dimension"`
	// Key 是同一个维度的扁平名字（`agent_daily` 等），与回执里 dimension 的后缀**完全
	// 一致**。
	//
	// 为什么要重复一次：Dimension 是结构化的（scope + period），给要按维度筛选的调用
	// 方；而人读的那一位、以及与回执对号入座的那一位，都是这个扁平串。只给结构化的
	// 话，每个消费方都要自己拼一遍 `scope + "_" + period`，拼法一旦不同就对不上号。
	Key        string       `json:"key"`
	EnvVar     string       `json:"env_var"`
	Configured bool         `json:"configured"`
	USD        float64      `json:"usd,omitempty"`
	ParseError string       `json:"parse_error,omitempty"`
	Window     PeriodWindow `json:"window"`
}

// PeriodBudgetStatus 是四个维度的设防快照，给 CLI / API 的观测位用。
type PeriodBudgetStatus struct {
	At       time.Time            `json:"at"`
	Statuses []PeriodBudgetConfig `json:"statuses"`
}

// PeriodBudgetStatusFromEnv 读四个维度的配置并算出各自的当前窗口。
func PeriodBudgetStatusFromEnv(at time.Time) PeriodBudgetStatus {
	out := PeriodBudgetStatus{At: at.UTC()}
	for _, d := range AllPeriodBudgetDimensions {
		b := periodBudgetFromEnv(d.EnvVar())
		out.Statuses = append(out.Statuses, PeriodBudgetConfig{
			Dimension:  d,
			Key:        d.Key(),
			EnvVar:     d.EnvVar(),
			Configured: b.Configured,
			USD:        b.USD,
			ParseError: b.ParseError,
			Window:     periodWindow(at, d.Period),
		})
	}
	return out
}

// AnyConfigured 报告四个维度里有没有任何一个设了防。
func (s PeriodBudgetStatus) AnyConfigured() bool {
	for _, st := range s.Statuses {
		if st.Configured {
			return true
		}
	}
	return false
}

// ruelPeriodUsageCost 汇总一个作用域在一个窗口内的折算成本。
//
// **unpriced 的行跳过不计**，但条数要报出去（#24 四态口径）：它们没有数字，当 0 计入
// 会让累计永远追不上上限，而纯 unpriced 的链（#32 里每条 codex 用量都是）会让这一层
// **永远不触发**——所以 UnpricedRows 必须跟着判定一起暴露，否则人看不出闸门是「没超」
// 还是「瞎了」。
func ruelPeriodUsageCost(rows []db.TaskUsage) PeriodCost {
	var acc PeriodCost
	for _, r := range rows {
		var providerTicks int64
		if r.CostUsdTicks.Valid {
			providerTicks = r.CostUsdTicks.Int64
		}
		c := metrics.EstimateUsageCost(r.Model, providerTicks, r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens)
		if !c.Priceable() {
			acc.UnpricedRows++
			continue
		}
		acc.PricedUSD += c.USD
		acc.PricedRows++
	}
	return acc
}

// ruelPeriodCostFor 取一个维度在当前窗口内的累计成本。
func (s *TaskService) ruelPeriodCostFor(ctx context.Context, d PeriodBudgetDimension, agentID, workspaceID pgtype.UUID, window PeriodWindow) (PeriodCost, error) {
	// 上下界都传到 SQL：只传下界的话，上一个周期的窗口会把之后的行也捞进来，
	// 「每行恰好属于一个周期」在库里就不成立了。
	since := pgtype.Timestamptz{Time: window.Start, Valid: true}
	until := pgtype.Timestamptz{Time: window.End, Valid: true}
	if d.Scope == ScopeAgent {
		rows, err := s.Queries.RuelListAgentTaskUsageInWindow(ctx, db.RuelListAgentTaskUsageInWindowParams{
			AgentID: agentID, Since: since, Until: until,
		})
		if err != nil {
			return PeriodCost{}, err
		}
		return ruelPeriodUsageCost(rows), nil
	}
	rows, err := s.Queries.RuelListWorkspaceTaskUsageInWindow(ctx, db.RuelListWorkspaceTaskUsageInWindowParams{
		WorkspaceID: workspaceID, Since: since, Until: until,
	})
	if err != nil {
		return PeriodCost{}, err
	}
	return ruelPeriodUsageCost(rows), nil
}

// ruelGuardPeriodBudget 在派单之前判定四个周期维度，任一触限即拒绝。
//
// 判**已花**而不是「这一单会花多少」：无法预知一单花多少，只能在派之前看已经花了多少。
// 所以这一层是**滞后**的——它会让第 N+1 单被拦下，而不是精确地卡在第 N 单中间。这个
// 形状要写明，不能让人以为它是精确的。
//
// 三条放行条件，各有理由：
//
//   - **四个维度都没配放行**，且不做日志噪音：不设防是明示的，不是漏的。
//   - **读不到用量放行**：读不出来不能变成拒绝，那会把一次查询抖动变成派单失败。
//   - **一条可计价的行都没有时放行**，日志写「无法判定」而不是「未超限」——#24 四态
//     口径在预算上的直接推论，与 #40 的那条同源。
func (s *TaskService) ruelGuardPeriodBudget(ctx context.Context, agentID, workspaceID, issueID pgtype.UUID, path string) error {
	now := time.Now().UTC()
	for _, d := range AllPeriodBudgetDimensions {
		budget := periodBudgetFromEnv(d.EnvVar())
		if budget.ParseError != "" {
			slog.ErrorContext(ctx, "ruel: 周期成本上限读不懂，按未配置处理",
				"dimension", d.Key(), "error", budget.ParseError)
		}
		if !budget.Configured {
			continue
		}
		window := periodWindow(now, d.Period)
		acc, err := s.ruelPeriodCostFor(ctx, d, agentID, workspaceID, window)
		if err != nil {
			slog.WarnContext(ctx, "ruel: 周期成本上限读不到用量，放行",
				"dimension", d.Key(), "path", path, "error", err)
			continue
		}
		if acc.PricedRows == 0 {
			slog.WarnContext(ctx, "ruel: 周期成本上限无法判定，放行",
				"dimension", d.Key(), "path", path, "unpriced_rows", acc.UnpricedRows)
			continue
		}
		if acc.PricedUSD < budget.USD {
			continue
		}
		slog.WarnContext(ctx, "ruel: 到了周期成本上限，拒绝派这一单",
			"dimension", d.Key(), "path", path,
			"spent_usd", acc.PricedUSD, "budget_usd", budget.USD,
			"window_start", window.Start.Format(time.RFC3339),
			"window_end", window.End.Format(time.RFC3339),
			"priced_rows", acc.PricedRows, "unpriced_rows", acc.UnpricedRows,
		)
		spentUSD := acc.PricedUSD
		budgetUSD := budget.USD
		pricedRows := acc.PricedRows
		unpricedRows := acc.UnpricedRows
		// 回执复用 #33 那条事务外的路。维度名带作用域与周期，因为四个维度的**修法不
		// 同**：agent 日上限要调这个 agent，workspace 月上限要调整个工作区。
		gaterefusal.Record(gaterefusal.Notice{
			Dimension:    gaterefusal.DimensionPeriodBudget + "." + d.Key(),
			Path:         path,
			// issue_id 要填：回执回答的是「哪一次派单被挡了」，只给 agent 与维度的话，
			// 人知道「这个 agent 今天超了」，却不知道**哪条 Issue 上那一单没派出去**，
			// 排查时还得回去翻日志。
			IssueID:      util.UUIDToString(issueID),
			AgentID:      util.UUIDToString(agentID),
			SpentUSD:     &spentUSD,
			BudgetUSD:    &budgetUSD,
			PricedRows:   &pricedRows,
			UnpricedRows: &unpricedRows,
		})
		return fmt.Errorf("%w: %s spent $%.4f of $%.4f in %s window",
			ErrPeriodBudgetExceeded, d.Key(), spentUSD, budgetUSD, d.Period)
	}
	return nil
}
