package handler

// Ruel 新增：#28 —— 深度闸门管不住的那一半，由成本预算来管。
//
// #25 清点六条委派入口时认清的一件事是**深度闸门管不住扇出**：一次 Run 可以建多个
// Issue，它们全是同一个父的第 1 层，深度永远是 1，但总量可以无限。深度封的是代数，
// 封不住总量。所以本条要验的组合是**深度远远没到上限、钱已经花过预算**。
//
// 三条纪律，各有一条用例守着：
//  1. 两个维度独立——浅而贵的链由预算拦，不是由深度拦（本文件第一个用例）。
//  2. 拒绝必须带 reason code 回到调用方（第二个用例）。
//  3. 读不出来不能变成拒绝——一条可计价的行都没有时**放行**（第三个用例）。

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ruelSpendTask 在 issueID 上造一个已经跑完的 Run，并给它挂一行用量。
//
// costTicks 为 nil 表示 provider 没报价（那一列写 NULL）；model 传一个价目表不认识的
// 名字就得到 `unpriced` 那一行——这正是 #24 的四态口径里最要紧的一种。
func ruelSpendTask(t *testing.T, agentID, runtimeID, issueID, model string, costTicks *int64, inputTokens int64) string {
	t.Helper()
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "completed",
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
	})
	params := db.UpsertTaskUsageParams{
		TaskID:           parseUUID(taskID),
		Provider:         "codex",
		Model:            model,
		InputTokens:      inputTokens,
		OutputTokens:     0,
		CacheReadTokens:  0,
		CacheWriteTokens: 0,
	}
	if costTicks != nil {
		params.CostUsdTicks = pgtype.Int8{Int64: *costTicks, Valid: true}
	}
	if err := testHandler.Queries.UpsertTaskUsage(context.Background(), params); err != nil {
		t.Fatalf("给 Run %s 挂用量失败: %v", taskID, err)
	}
	return taskID
}

// ruelRunsOn 数这条 Issue 上有多少个 Run。
func ruelRunsOn(t *testing.T, issueID string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&n)
	return n
}

// TestRuelDelegationBudgetStopsShallowButExpensiveChain 是「深度未超限、预算已超限」
// 这一组合的专门用例——它是 #28 存在的理由。
//
// 链长只有 2（链尾深度 1，上限是 12），深度闸门在任何情况下都不会响；这条链上已经花
// 掉的钱超过预算，所以**只可能是**预算闸门拦的。若这里拿到的是
// ErrDelegationDepthExceeded，说明两个维度被谁合成了一个，那正是本条要防的事。
func TestRuelDelegationBudgetStopsShallowButExpensiveChain(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	chainIssueID, agentA, agentB, runtimeA, _ := newRuelCyclePair(t, "Ruel: can a shallow chain still be stopped on cost?")

	// 链尾深度 1——离上限 12 远得很，深度闸门必须不响。
	parent := buildRuelDelegationChain(t, agentA, runtimeA, chainIssueID, 2)
	targetIssue := newRuelAgentCreatedIssue(t, "Ruel: shallow chain, over budget", agentA, agentB, parent)

	// 两条 Run，provider 各报 $1.00 —— 合计 $2.00，越过 $1.7654 的预算。
	// 用 provider 自报价（cost_usd_ticks）而不是价目表折算，是为了让这条用例不依赖
	// 价目表里有没有某个模型：报价在，成本就在。
	dollar := int64(10_000_000_000)
	ruelSpendTask(t, agentB, runtimeA, targetIssue, "glm-5", &dollar, 1000)
	ruelSpendTask(t, agentB, runtimeA, targetIssue, "glm-5", &dollar, 1000)
	before := ruelRunsOn(t, targetIssue)

	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, mustRuelIssue(t, targetIssue)); !errors.Is(err, service.ErrDelegationBudgetExceeded) {
		t.Fatalf("浅而贵的链入队结果 = %v, want ErrDelegationBudgetExceeded", err)
	}
	// 拒绝必须是**没起 Run**，不是起了又丢。
	if after := ruelRunsOn(t, targetIssue); after != before {
		t.Fatalf("被拒的 Issue 上 Run 数 %d -> %d, want 不变（%d）", before, after, before)
	}
	t.Logf("链尾深度 1（上限 %d）、已花 $2.00（预算 $%.4f）：被预算闸门拦住，没有起 Run",
		service.RuelDelegationDepthLimit, float64(service.RuelDelegationBudgetTicks)/10_000_000_000)
}

// TestRuelDelegationBudgetReasonReachesCaller 守的是「静默丢弃是把跑飞换成哑火」。
//
// 闸门在 service 层，拒绝要由 handler 翻译成 reason code 才到得了调用方。这一条把映射
// 单独钉住：它很便宜，但漏了就没人知道链为什么停。
func TestRuelDelegationBudgetReasonReachesCaller(t *testing.T) {
	if got := commentEnqueueFailureReason(service.ErrDelegationBudgetExceeded); got != ReasonDelegationBudgetExceeded {
		t.Fatalf("reason_code = %q, want %q", got, ReasonDelegationBudgetExceeded)
	}
	// 两个维度必须映射到**不同**的 code：合并成一个之后没人说得清到底是因为什么停的。
	if ReasonDelegationBudgetExceeded == ReasonDelegationDepthExceeded {
		t.Fatal("预算与深度共用同一个 reason code，调用方无法区分")
	}
}

// TestRuelDelegationBudgetPassesWhenNothingIsPriceable 守的是「读不出来不能变成拒绝」，
// 以及 unpriced 不得被当成 0 之外的任何东西。
//
// 这条 Issue 上有两条 Run，各消耗 50 万 token，但模型名价目表不认识、provider 也没报
// 价——四态口径里这就是 `unpriced`：**有消耗，算不出来**。此时必须放行。
//
// 它同时挡住一种「好心」的改法：给 unpriced 编一个估算值好让闸门能算。那样做会把
// 一次算不出来变成一次拒绝，而拒绝是比放行重得多的动作。
func TestRuelDelegationBudgetPassesWhenNothingIsPriceable(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	chainIssueID, agentA, agentB, runtimeA, _ := newRuelCyclePair(t, "Ruel: does an unpriceable chain get refused?")

	parent := buildRuelDelegationChain(t, agentA, runtimeA, chainIssueID, 2)
	targetIssue := newRuelAgentCreatedIssue(t, "Ruel: shallow chain, nothing priceable", agentA, agentB, parent)

	// 模型名故意取一个价目表里没有的，且不填 cost_usd_ticks。
	ruelSpendTask(t, agentB, runtimeA, targetIssue, "totally-unpriced-model", nil, 500_000)
	ruelSpendTask(t, agentB, runtimeA, targetIssue, "totally-unpriced-model", nil, 500_000)

	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, mustRuelIssue(t, targetIssue)); err != nil {
		t.Fatalf("一条可计价的行都没有时入队结果 = %v, want 放行（读不出来不能变成拒绝）", err)
	}
	t.Log("全部 unpriced：放行。闸门在这条链上确实看不见花了多少，这个洞由 #32 从根上补")
}
