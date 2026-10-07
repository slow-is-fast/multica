package service

// Ruel 新增：委派链的**成本预算**闸门。它与深度闸门（`ruel_delegation_depth.go`）是
// 两个独立维度，任一触限即停，不合并成一个「综合分」。
//
// 为什么光有深度闸门不够：深度数的是**转手次数**，#25 清点六条入口时认清的一件事是
// **深度闸门管不住扇出**——一次 Run 可以建多个 Issue，它们全是同一个父的第 1 层，
// 深度永远是 1，但总量可以无限。深度封的是代数，封不住总量；总量只能靠钱来封。
//
// ## 上限怎么来的（不是拍的）
//
// 锚定在深度闸门上：深度上限 12 次转手 = 最多 13 个 Run。预算取
// 「13 个 Run，每个都按单 Run 折算成本的 p90 花」= 13 × $0.1358 = $1.7654。
//
// 也就是说：**一条走到深度上限的合法深链，如果每一步都只花到 p90，刚好不会触发预算
// 闸门**。超过它，说明要么步数超了（深度闸门管），要么**每一步比 p90 还贵**（预算闸门
// 管）。两个维度在「一条正常链的极限」处对齐，这是刻意的——否则说不清一个维度到底
// 比另一个宽多少。
//
// 用 p90 而不是 p50：观测到的 max/median 是 1.80，比 p90/median（1.35）还高。取 p50
// 的话，一条每一步只是略贵于中位数的正常深链会在第 8、9 步就被拦掉——预算闸门是
// **兜底**，不是要取代 6.6.1 那条「单 Run 超过中位数 3 倍」的告警。
//
// 两个 caveat 必须跟着这个数一起记（都来自 #24）：基准只来自 `m3-smoke` × `glm-5`
// **一个组合**，不是自然流量；`unpriced` 的行不计入累计（见下面 ruelIssueUsageCost）。

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/util"
)

// ErrDelegationBudgetExceeded 是预算闸门拒绝入队时返回的哨兵错误。
//
// 与 ErrDelegationDepthExceeded 同一条纪律：调用方要用它给出 reason code
// （handler 层映射到 dispatch.ReasonDelegationBudgetExceeded），不能当成普通错误吞掉。
var ErrDelegationBudgetExceeded = errors.New("delegation chain reached its cost budget")

const (
	// ruelDelegationBudgetRunTicks 是**单 Run** 折算成本的 p90，单位 1e-10 USD。
	//
	// 取自 #24 的实测：$0.1358（`m3-smoke` × `glm-5`，n=21）。同批的 p50 是 $0.1008、
	// max $0.1815。改这个数前先重跑那组统计——它就是预算的地基。
	ruelDelegationBudgetRunTicks = 1_358_000_000

	// RuelDelegationBudgetTicks 是一条委派链在**同一个 Issue 上**最多能累计花掉多少
	// 折算成本，单位 1e-10 USD。
	//
	// 写成两个常量的乘积而不是一个数，是因为它本来就是**推导**出来的：深度上限决定
	// 一条链最多该有多少步，p90 决定每步该花多少。任何一个变了，预算跟着变，不用
	// 再来拍一次。
	//
	// 导出是给验收测试用的：闸门在 service 层，而触发它的用例写在 handler 包里。
	RuelDelegationBudgetTicks = int64(RuelDelegationDepthLimit+1) * ruelDelegationBudgetRunTicks

	// ruelDelegationBudgetUSD 是同一个预算的美金表示，只用于日志与暂停原因里给人看。
	// 单独存一份是为了让调用方不用为了「把 ticks 说成人话」而 import metrics。
	ruelDelegationBudgetUSD = float64(RuelDelegationBudgetTicks) / metrics.CostUSDTicksPerUSD
)

// ruelIssueCost 是一条 Issue 上累计的折算成本，带着「有多少行算不出来」。
//
// 为什么要带 UnpricedRows：#24 的四态口径里，`unpriced` 不能当 0 用。一条模型名为
// `unknown` 的用量行按 0 计入累计，会让累计永远追不上上限，预算兜底形同虚设——而这
// 正是 #24 修掉的那个混淆。
type ruelIssueCost struct {
	PricedUSD    float64
	PricedRows   int
	UnpricedRows int
}

// ruelIssueUsageCost 汇总一条 Issue 上所有用量行的折算成本。
//
// **unpriced 的行跳过不计**：它们没有数字，既不能当 0（会稀释累计、让闸门永不触发），
// 也不能编一个数。跳过之后它们的**条数**要报出去——判定是在「看不见 N 行」的前提下
// 做出来的，这一点不能藏。
func (s *TaskService) ruelIssueUsageCost(ctx context.Context, issueID pgtype.UUID) (ruelIssueCost, error) {
	rows, err := s.Queries.ListIssueTaskUsage(ctx, issueID)
	if err != nil {
		return ruelIssueCost{}, err
	}
	var acc ruelIssueCost
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
	return acc, nil
}

// ruelGuardDelegationBudget 判定「让这个新 Run 认 parentTaskID 当爹」之前，这条 Issue
// 上已经累计花掉的钱有没有过预算，过了就拒绝。
//
// 两条放行条件，各有一条理由：
//
//   - **parentTaskID 无效放行**（不是委派来的）：闸门只对 Agent 之间的转手生效。这一条
//     同时就是 6.8 第 3 条要的「人显式放行」——人自己派一次单不带父 Run，天然不被拦。
//     per_issue 的累计不会自己归零，闸门一旦触发就永久封住这个 Issue，所以**必须有
//     这条出路**；这也是它和深度闸门不一样的地方（深度按链算，换条链就归零）。
//   - **读不到用量放行**：读不出来不能变成拒绝，那会把一次查询抖动变成派单失败。
//
// 还有一种放行必须说清：**一条可计价的行都没有时，放行**，并且日志写的是「无法判定」
// 而不是「未超限」。「没超预算」和「不知道花了多少」不是一回事——这是 #24 那条四态
// 口径在预算上的直接推论。全部行都 unpriced 时闸门确实拦不住任何东西，这个洞由 #32
// （codex adapter 不报模型名）从根上补，这里不假装它不存在。
func (s *TaskService) ruelGuardDelegationBudget(ctx context.Context, parentTaskID, issueID, agentID pgtype.UUID, path string) error {
	if !parentTaskID.Valid || !issueID.Valid {
		return nil
	}
	acc, err := s.ruelIssueUsageCost(ctx, issueID)
	if err != nil {
		slog.WarnContext(ctx, "ruel: 委派链成本预算读不到用量，放行",
			"path", path,
			"issue_id", util.UUIDToString(issueID),
			"agent_id", util.UUIDToString(agentID),
			"error", err,
		)
		return nil
	}
	if acc.PricedRows == 0 {
		slog.WarnContext(ctx, "ruel: 委派链成本预算无法判定，放行",
			"path", path,
			"issue_id", util.UUIDToString(issueID),
			"agent_id", util.UUIDToString(agentID),
			"parent_task_id", util.UUIDToString(parentTaskID),
			"unpriced_rows", acc.UnpricedRows,
		)
		return nil
	}
	spent := acc.PricedUSD * metrics.CostUSDTicksPerUSD
	if spent < float64(RuelDelegationBudgetTicks) {
		return nil
	}
	slog.WarnContext(ctx, "ruel: 委派链到了成本预算上限，拒绝再转一次手",
		"path", path,
		"issue_id", util.UUIDToString(issueID),
		"agent_id", util.UUIDToString(agentID),
		"parent_task_id", util.UUIDToString(parentTaskID),
		"spent_usd", acc.PricedUSD,
		"budget_usd", float64(RuelDelegationBudgetTicks)/metrics.CostUSDTicksPerUSD,
		"priced_rows", acc.PricedRows,
		"unpriced_rows", acc.UnpricedRows,
	)
	return ErrDelegationBudgetExceeded
}
