package service

// Ruel 新增：单次 Run 的成本上限 —— PRD 6.8 三层预算（per_run / per_issue /
// per_agent+per_workspace）的第一层。fork 上此前只有 per_issue 一层（#28）。
//
// ## 为什么这一层挂在这里，而不是挂在入队处
//
// 用量由 daemon 增量上报（ReportTaskUsage），服务端按 (task_id, provider, model)
// **整行覆盖、不是累加**（upsertTaskUsage 的 ON CONFLICT DO UPDATE）。所以每次上报
// 之后都能读到这一轮的完整累计，而那一刻 Run 存在且处于 running。
//
// 判定时机决定了一切：这一层能做**运行时熔断**，而委派闸门（#28）做不到——那道闸门
// 挂在入队之前，那一刻 Run 还不存在，它只能拒绝「再派一次」，拦不住「正在跑的这一
// 轮」。两者不是同一件事，别拿其中一处的结论去套另一处。
//
// ## 为什么终态是 cancelled + 原因码，而不是 blocked
//
// PRD 6.8 第 3 条写的是「Run 进入 blocked」。**但 blocked 不是这个状态机的值**：
// agent_task_queue_status_check 只允许 queued / dispatched / running /
// completed / failed / cancelled / waiting_local_directory / deferred 八个。
//
// 而且这个词在本仓库已经用了两次，各指一件事：daemon 把「agent 产出被毒化的兜底
// 输出」报成 TaskResult.Status = "blocked"；委派闸门把拒绝响应报成
// DispatchStatus = "blocked"。再借来指第三件事，比没有这个词更糟。
//
// 所以取 cancelled + failure_reason = run_cost_budget_exceeded，走
// CancelTaskWithReason —— 它是为「服务端决定停掉一个 Run 并留下原因」准备的现成
// 通路，而且**取消本身就是让 daemon 终止进程树的机制**（P0-4 实测过：服务端终态
// → daemon 打断 agent → Windows Job Object 终止整棵树）。自己另造一条停止通路，
// 只会得到「状态改了但进程还在跑」。
//
// 6.8 第 3 条真正要的三件事，这样都还在：
//   - 不静默截断 —— 原因码 + 事务外回执（#33）+ webhook 推送（#36）
//   - 不静默继续 —— 真的取消了，进程树真的停了
//   - 超出部分需人显式放行 —— 人把额度调高后重跑一次，就是放行
//
// ## 上限不给默认值
//
// 边界决策第 4 节的通则：凡本机样本算出来的数一律转成规则而不是常数。per_run 上限
// 连样本都没有（没人这么跑过），所以是**不配置就不熔断**，并且必须能被看出来是
// 「没配」而不是「配了个很大的数」。

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/multica-ai/multica/server/internal/gaterefusal"
	"github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// RunCostBudgetEnvVar 是 per_run 折算成本上限的环境变量名，单位 USD。
	RunCostBudgetEnvVar = "MULTICA_RUN_COST_BUDGET_USD"

	// RunCostFailureReason 是被预算停掉的 Run 落在库里的 failure_reason。它是机器
	// 读的原因码，与给人看的 ErrorMessage 分开——后者带具体数字，前者保持稳定，
	// 否则按原因码做聚合的代码会被数字变化打断。
	RunCostFailureReason = "run_cost_budget_exceeded"
)

// RunCostBudget 是 per_run 上限的配置读取结果。
//
// 为什么要区分 Configured 与 USD：一个 0 可能是「没配」，也可能是「配了 0」。前者是
// 放行且应当被看见，后者是「一分钱都不许花」。把两者压成一个 float64，运维就分不清
// 「我忘了配」和「我配了零」。
type RunCostBudget struct {
	USD        float64
	Configured bool
	// ParseError 非空表示环境变量设了但读不懂。此时按**未配置**处理（放行），并把
	// 错误暴露出去——一个读不懂的上限如果当成「无限」是静默放行，当成 0 是静默截断
	// 每一个 Run，两者都比显式报错糟。
	ParseError string
}

// RunCostBudgetFromEnv 读取 per_run 上限。每次调用都读环境变量，不在包初始化时
// 缓存：一是运维改了要能生效，二是测试要用 t.Setenv。
func RunCostBudgetFromEnv() RunCostBudget {
	raw, ok := os.LookupEnv(RunCostBudgetEnvVar)
	if !ok {
		return RunCostBudget{}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return RunCostBudget{}
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return RunCostBudget{ParseError: fmt.Sprintf("%s=%q is not a number: %v", RunCostBudgetEnvVar, raw, err)}
	}
	if v < 0 {
		return RunCostBudget{ParseError: fmt.Sprintf("%s=%q is negative", RunCostBudgetEnvVar, raw)}
	}
	return RunCostBudget{USD: v, Configured: true}
}

// RuelRunCost 是一轮 Run 累计下来的折算成本，带着「有多少行算不出来」。
//
// 为什么要带 UnpricedRows：#24 的四态口径里 unpriced 不能当 0 用。一条模型名为
// unknown 的用量行按 0 计入，会让累计永远追不上上限，闸门形同虚设——这正是 #32
// 踩过的坑（每条 codex 用量都 unpriced，等于纯 codex 的链成本恒为 0）。
type RuelRunCost struct {
	PricedUSD    float64
	PricedRows   int
	UnpricedRows int
}

// RuelRunCostFromUsage 汇总一轮 Run 所有用量行的折算成本。
//
// 单独导出成一个吃切片的函数，是为了让它能脱离数据库被测：db.TaskUsage 是普通结构
// 体，测试可以直接造。闸门真正的判据都在这里，能单测就不必每次都起一个库。
func RuelRunCostFromUsage(rows []db.TaskUsage) RuelRunCost {
	var acc RuelRunCost
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

// RuelEnforceRunCostBudget 在用量上报之后判定这一轮有没有花超，超了就停掉它。
//
// 返回是否触发了熔断。三条放行条件，各有理由：
//
//   - **没配上限放行**，且不做任何日志噪音：不设上限是明示的，不是漏的。
//   - **Run 不在 running 放行**：只停正在跑的那一轮。取消之后迟到的用量上报不能
//     再触发一次取消（P0-4「迟到回报不能覆盖取消」的同一条纪律）。
//   - **一条可计价的行都没有时放行**，日志写「无法判定」而不是「未超限」。「没超预算」
//     和「不知道花了多少」不是一回事——这是 #24 四态口径在预算上的直接推论。
//
// 顺序是**先记回执再取消**：回执记在事务之外（#33），就算取消那一步失败，人也已经
// 看得见这件事了。反过来则是一次静默失败。
func (s *TaskService) RuelEnforceRunCostBudget(ctx context.Context, task db.AgentTaskQueue) (bool, error) {
	budget := RunCostBudgetFromEnv()
	if budget.ParseError != "" {
		slog.ErrorContext(ctx, "ruel: per_run 成本上限读不懂，按未配置处理",
			"task_id", util.UUIDToString(task.ID),
			"error", budget.ParseError,
		)
	}
	if !budget.Configured {
		return false, nil
	}
	if task.Status != "running" {
		return false, nil
	}

	rows, err := s.Queries.GetTaskUsage(ctx, task.ID)
	if err != nil {
		// 读不到用量不能变成熔断，那会把一次查询抖动变成杀掉别人的 Run。
		slog.WarnContext(ctx, "ruel: per_run 成本上限读不到用量，放行",
			"task_id", util.UUIDToString(task.ID),
			"error", err,
		)
		return false, nil
	}
	acc := RuelRunCostFromUsage(rows)
	if acc.PricedRows == 0 {
		slog.WarnContext(ctx, "ruel: per_run 成本上限无法判定，放行",
			"task_id", util.UUIDToString(task.ID),
			"unpriced_rows", acc.UnpricedRows,
		)
		return false, nil
	}
	if acc.PricedUSD <= budget.USD {
		return false, nil
	}

	spentUSD := acc.PricedUSD
	budgetUSD := budget.USD
	pricedRows := acc.PricedRows
	unpricedRows := acc.UnpricedRows
	issueID := util.UUIDToString(task.IssueID)
	agentID := util.UUIDToString(task.AgentID)
	taskID := util.UUIDToString(task.ID)

	slog.WarnContext(ctx, "ruel: 这一轮到了 per_run 成本上限，停掉它",
		"task_id", taskID,
		"issue_id", issueID,
		"agent_id", agentID,
		"spent_usd", spentUSD,
		"budget_usd", budgetUSD,
		"priced_rows", pricedRows,
		"unpriced_rows", unpricedRows,
	)

	gaterefusal.Record(gaterefusal.Notice{
		Dimension: gaterefusal.DimensionRunCost,
		Path:      "report_task_usage",
		IssueID:   issueID,
		AgentID:   agentID,
		TaskID:    taskID,
		SpentUSD:  &spentUSD,
		BudgetUSD: &budgetUSD,
		// 四个数字全带，理由同 #28：已花 / 上限决定要不要调预算，priced_rows 决定
		// 这个判定有多少依据，**unpriced_rows 决定这个判定有多瞎**——算不出来的行
		// 数越多，「已花」越是个下限。藏掉它，人会把下限读成实际值。
		PricedRows:   &pricedRows,
		UnpricedRows: &unpricedRows,
	})

	msg := fmt.Sprintf(
		"Run stopped by the per-run cost budget: spent $%.4f of $%.4f (%d priced usage rows, %d unpriced). Raise %s and rerun to continue.",
		spentUSD, budgetUSD, pricedRows, unpricedRows, RunCostBudgetEnvVar,
	)
	if _, err := s.CancelTaskWithReason(ctx, task.ID, msg, RunCostFailureReason); err != nil {
		// 回执已经记下了，这次失败不会变成静默失败；但要说出来，否则「闸门响了却没停」
		// 会表现为一次莫名其妙的继续运行。
		slog.ErrorContext(ctx, "ruel: per_run 成本上限触发了但取消失败",
			"task_id", taskID,
			"error", err,
		)
		return true, err
	}
	return true, nil
}

// RuelRunCostBudgetStatus 是给观测展示位用的快照：这一层到底有没有设防。
//
// 未配置必须能被看出来——6.8 第 3 条要的是「不允许静默」，而一个没设上限的系统如果
// 在界面上什么都不显示，和一个设了很大上限的系统长得一模一样。
type RuelRunCostBudgetStatus struct {
	Configured bool    `json:"configured"`
	BudgetUSD  float64 `json:"budget_usd,omitempty"`
	ParseError string  `json:"parse_error,omitempty"`
}

// RuelRunCostBudgetStatusFromEnv 返回当前进程的 per_run 上限状态。
func RuelRunCostBudgetStatusFromEnv() RuelRunCostBudgetStatus {
	b := RunCostBudgetFromEnv()
	return RuelRunCostBudgetStatus{
		Configured: b.Configured,
		BudgetUSD:  b.USD,
		ParseError: b.ParseError,
	}
}
