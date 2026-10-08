package handler

// Ruel 新增：#33 —— 闸门拒绝的回执要在**事务回滚之后**还在。
//
// 这是本条存在的全部理由。拒绝的方式是返回 error，调用方随即回滚整笔事务，于是活动流、
// 评论、timeline 这些「写在事务里」的通知全部跟着消失，库里一行痕迹都不留。过去只有两个
// 出口：reason code 回到调用方（派单的 Agent 看得到，人不一定看得到）和警告日志（只有翻
// 日志的人看得到）。
//
// 本文件要证明的是：拒绝发生之后，人还能问到它，而且问到的内容够他决定下一步。

import (
	"context"
	"errors"
	"testing"

	"github.com/multica-ai/multica/server/internal/gaterefusal"
	"github.com/multica-ai/multica/server/internal/service"
)

// TestRuelGateRefusalSurvivesTheRollback 是 #33 的验收用例。
//
// 构造一次**预算**拒绝，然后在事务已经回滚的前提下断言回执还在。选预算而不是深度，是
// 因为它更该被看见：深度换一条链就归零，per_issue 的累计不会自己归零。
func TestRuelGateRefusalSurvivesTheRollback(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	gaterefusal.Reset()
	defer gaterefusal.Reset()

	ctx := context.Background()
	chainIssueID, agentA, agentB, runtimeA, _ := newRuelCyclePair(t, "Ruel: does a refused delegation leave any trace?")
	parent := buildRuelDelegationChain(t, agentA, runtimeA, chainIssueID, 2)
	targetIssue := newRuelAgentCreatedIssue(t, "Ruel: over budget, does the refusal survive?", agentA, agentB, parent)

	// provider 各报 $1.00，合计 $2.00 —— 越过 $1.7654 的预算。
	dollar := int64(10_000_000_000)
	ruelSpendTask(t, agentB, runtimeA, targetIssue, "glm-5", &dollar, 1000)
	ruelSpendTask(t, agentB, runtimeA, targetIssue, "glm-5", &dollar, 1000)

	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, mustRuelIssue(t, targetIssue)); !errors.Is(err, service.ErrDelegationBudgetExceeded) {
		t.Fatalf("入队结果 = %v, want ErrDelegationBudgetExceeded", err)
	}

	// 关键断言：**事务已经回滚了**，而回执还在。
	snapshot := gaterefusal.Snapshot()
	var found *gaterefusal.Notice
	for i := range snapshot {
		if snapshot[i].IssueID == targetIssue && snapshot[i].Dimension == gaterefusal.DimensionBudget {
			found = &snapshot[i]
			break
		}
	}
	if found == nil {
		t.Fatal("事务回滚之后找不到预算拒绝的回执——这正是 #33 要修的事")
	}
	t.Logf("回执：维度=%s 入口=%s 次数=%d", found.Dimension, found.Path, found.Count)

	// 通知里必须有「维度、已花、上限、算不出来的行数」四项，缺一项人就决定不了下一步。
	if found.SpentUSD == nil || found.BudgetUSD == nil {
		t.Fatal("回执缺「已花」或「上限」：不给这两个数，人无法判断该不该调预算")
	}
	if found.PricedRows == nil || found.UnpricedRows == nil {
		t.Fatal("回执缺计价行数或算不出来的行数")
	}
	// 「算不出来的行数」尤其不能少：它决定了「已花」是个实际值还是个下限。
	if *found.UnpricedRows != 0 {
		t.Logf("有 %d 行算不出来，「已花 $%.4f」是下限而非实际值",
			*found.UnpricedRows, *found.SpentUSD)
	}
	if *found.SpentUSD < *found.BudgetUSD {
		t.Errorf("已花 $%.4f < 上限 $%.4f，回执与拒绝自相矛盾", *found.SpentUSD, *found.BudgetUSD)
	}
}

// TestRuelGateRefusalMergesBursts 守的是「通知自己不能变成噪音」。
//
// 委派链可以在很短时间内密集触发。每次拒绝都发一条，通知的下场是被忽略——那等于没有通知。
func TestRuelGateRefusalMergesBursts(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	gaterefusal.Reset()
	defer gaterefusal.Reset()

	ctx := context.Background()
	chainIssueID, agentA, agentB, runtimeA, _ := newRuelCyclePair(t, "Ruel: does a burst of refusals become noise?")
	parent := buildRuelDelegationChain(t, agentA, runtimeA, chainIssueID, 2)
	targetIssue := newRuelAgentCreatedIssue(t, "Ruel: burst of budget refusals", agentA, agentB, parent)

	dollar := int64(10_000_000_000)
	ruelSpendTask(t, agentB, runtimeA, targetIssue, "glm-5", &dollar, 1000)
	ruelSpendTask(t, agentB, runtimeA, targetIssue, "glm-5", &dollar, 1000)

	issue := mustRuelIssue(t, targetIssue)
	for i := 0; i < 3; i++ {
		if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); !errors.Is(err, service.ErrDelegationBudgetExceeded) {
			t.Fatalf("第 %d 次入队结果 = %v, want ErrDelegationBudgetExceeded", i+1, err)
		}
	}

	var matched []gaterefusal.Notice
	for _, n := range gaterefusal.Snapshot() {
		if n.IssueID == targetIssue && n.Dimension == gaterefusal.DimensionBudget {
			matched = append(matched, n)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("连续 3 次拒绝产生 %d 条回执, want 1（2 秒内的重复要合并）", len(matched))
	}
	if matched[0].Count != 3 {
		t.Errorf("Count = %d, want 3（合并后要能看出被拒了几次）", matched[0].Count)
	}
	t.Logf("连续 3 次拒绝合并成 1 条，Count=%d", matched[0].Count)
}
