package handler

// Ruel 新增：#40 —— per_run 成本上限。
//
// 这里要证明的不是「钱算对了」（那在 service 包里脱离数据库测过了），而是**闸门真的
// 停掉了那一轮**。判定时机在这一层第一次变得可证：用量上报之后 Run 还在 running，
// 于是「停掉它」是一个有对象的动作，不像委派闸门那样只能拒绝一个还没出生的 Run。

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/gaterefusal"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ruelRunningTaskWithUsage 起一个 **running** 的 Run 并挂上用量。
//
// 状态必须是 running：闸门只对正在跑的那一轮生效，这是「迟到回报不能覆盖取消」那条
// 纪律的另一面——已经终态的 Run 不该被预算再动一次。
func ruelRunningTaskWithUsage(t *testing.T, agentID, runtimeID, issueID, model string, in, out, cr int64) string {
	t.Helper()
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "status": "running",
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
	})
	if err := testHandler.Queries.UpsertTaskUsage(context.Background(), db.UpsertTaskUsageParams{
		TaskID:          parseUUID(taskID),
		Provider:        "codex",
		Model:           model,
		InputTokens:     in,
		OutputTokens:    out,
		CacheReadTokens: cr,
	}); err != nil {
		t.Fatalf("给 Run %s 挂用量失败: %v", taskID, err)
	}
	return taskID
}

func ruelTaskOutcome(t *testing.T, taskID string) (status, failureReason string) {
	t.Helper()
	dbfx.QueryRow(t,
		`SELECT status, COALESCE(failure_reason, '') FROM agent_task_queue WHERE id = $1`,
		taskID,
	).Scan(&status, &failureReason)
	return status, failureReason
}

// TestPerRunCostBudgetStopsTheRunThatIsOver 是 #40 的主验收用例。
//
// 三重断言，缺一不可：
//  1. 闸门报告触发了
//  2. **库里那一轮真的成了 cancelled**（不是只在内存里改了个状态）
//  3. 原因码对得上——否则人只看到「被取消了」，看不出是被什么取消的
func TestPerRunCostBudgetStopsTheRunThatIsOver(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	gaterefusal.Reset()
	defer gaterefusal.Reset()

	// 上限 $0.05；下面那一轮实测花 $0.0793682。
	t.Setenv(service.RunCostBudgetEnvVar, "0.05")

	ctx := context.Background()
	_, agentA, _, runtimeA, _ := newRuelCyclePair(t, "Ruel: does the per-run budget actually stop a run?")
	issueID := dbfx.Issue(t, "Ruel: per-run over budget", testutil.Cols{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": agentA,
	})
	taskID := ruelRunningTaskWithUsage(t, agentA, runtimeA, issueID, "glm-5", 64389, 1257, 54784)

	task := mustRuelTask(t, taskID)
	stopped, err := testHandler.TaskService.RuelEnforceRunCostBudget(ctx, task)
	if err != nil {
		t.Fatalf("闸门返回错误: %v", err)
	}
	if !stopped {
		t.Fatal("花到 $0.0793682、上限 $0.05，闸门却没触发")
	}

	status, reason := ruelTaskOutcome(t, taskID)
	if status != "cancelled" {
		t.Fatalf("Run 状态 = %q, want cancelled。没停掉就等于「不允许静默继续」这条没做到", status)
	}
	if reason != service.RunCostFailureReason {
		t.Fatalf("failure_reason = %q, want %q。只写 cancelled 不写原因，人看不出是被什么停的",
			reason, service.RunCostFailureReason)
	}
	t.Logf("已停：Run %s → %s / %s", taskID, status, reason)

	// 回执必须留下：这是「不允许静默截断」的另一半。
	var found *gaterefusal.Notice
	snap := gaterefusal.Snapshot()
	for i := range snap {
		if snap[i].Dimension == gaterefusal.DimensionRunCost {
			found = &snap[i]
			break
		}
	}
	if found == nil {
		t.Fatal("触发了却没留下回执——这正是 #33 要修的那种静默")
	}
	if found.TaskID != taskID {
		t.Fatalf("回执的 task_id = %q, want %q。per_run 是按 Run 判的，回执必须指到那一轮", found.TaskID, taskID)
	}
	if found.SpentUSD == nil || found.BudgetUSD == nil || *found.SpentUSD <= *found.BudgetUSD {
		t.Fatalf("回执的已花/上限自相矛盾：%v / %v", found.SpentUSD, found.BudgetUSD)
	}
	t.Logf("回执：已花 $%.4f / 上限 $%.4f，计价 %d 行，算不出 %d 行",
		*found.SpentUSD, *found.BudgetUSD, *found.PricedRows, *found.UnpricedRows)
}

// 没超就不能动：预算是兜底，不是要取代单轮告警。误伤正常 Run 比漏拦更糟——漏拦只是
// 多花钱，误伤会让人把闸门关掉。
func TestPerRunCostBudgetLeavesARunUnderItAlone(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	t.Setenv(service.RunCostBudgetEnvVar, "1.00")

	ctx := context.Background()
	_, agentA, _, runtimeA, _ := newRuelCyclePair(t, "Ruel: does the per-run budget leave a cheap run alone?")
	issueID := dbfx.Issue(t, "Ruel: per-run under budget", testutil.Cols{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": agentA,
	})
	taskID := ruelRunningTaskWithUsage(t, agentA, runtimeA, issueID, "glm-5", 64389, 1257, 54784)

	stopped, err := testHandler.TaskService.RuelEnforceRunCostBudget(ctx, mustRuelTask(t, taskID))
	if err != nil || stopped {
		t.Fatalf("花 $0.0793682、上限 $1.00，闸门却触发了（stopped=%v err=%v）", stopped, err)
	}
	if status, _ := ruelTaskOutcome(t, taskID); status != "running" {
		t.Fatalf("Run 状态 = %q, want running（没超限就该继续跑）", status)
	}
}

// 不配置就不熔断。6.8 第 3 条要的是「不允许静默」，不设上限必须**明示**——所以它
// 既不能偷偷按某个默认值截断，也不能一声不吭。
func TestPerRunCostBudgetUnconfiguredDoesNotStopAnything(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	t.Setenv(service.RunCostBudgetEnvVar, "")

	ctx := context.Background()
	_, agentA, _, runtimeA, _ := newRuelCyclePair(t, "Ruel: does an unset per-run budget stay out of the way?")
	issueID := dbfx.Issue(t, "Ruel: per-run unconfigured", testutil.Cols{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": agentA,
	})
	// 一个亿 token，够贵了。
	taskID := ruelRunningTaskWithUsage(t, agentA, runtimeA, issueID, "glm-5", 100_000_000, 0, 0)

	stopped, err := testHandler.TaskService.RuelEnforceRunCostBudget(ctx, mustRuelTask(t, taskID))
	if err != nil || stopped {
		t.Fatalf("没配上限却触发了熔断（stopped=%v err=%v）", stopped, err)
	}
	if status, _ := ruelTaskOutcome(t, taskID); status != "running" {
		t.Fatalf("Run 状态 = %q, want running", status)
	}
}

// 已经终态的 Run 不能再被预算动一次。
func TestPerRunCostBudgetIgnoresRunsThatAreAlreadyDone(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	t.Setenv(service.RunCostBudgetEnvVar, "0.01")

	ctx := context.Background()
	_, agentA, _, runtimeA, _ := newRuelCyclePair(t, "Ruel: does the per-run budget touch finished runs?")
	issueID := dbfx.Issue(t, "Ruel: per-run on a finished run", testutil.Cols{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": agentA,
	})
	taskID := dbfx.Task(t, agentA, testutil.Cols{
		"runtime_id": runtimeA, "issue_id": issueID, "status": "completed",
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
	})
	if err := testHandler.Queries.UpsertTaskUsage(context.Background(), db.UpsertTaskUsageParams{
		TaskID: parseUUID(taskID), Provider: "codex", Model: "glm-5", InputTokens: 100_000_000,
	}); err != nil {
		t.Fatalf("挂用量失败: %v", err)
	}

	stopped, err := testHandler.TaskService.RuelEnforceRunCostBudget(ctx, mustRuelTask(t, taskID))
	if err != nil || stopped {
		t.Fatalf("已完成的 Run 被预算动了一次（stopped=%v err=%v）", stopped, err)
	}
	if status, _ := ruelTaskOutcome(t, taskID); status != "completed" {
		t.Fatalf("Run 状态 = %q, want completed（迟到的用量上报不该改写终态）", status)
	}
}

// TestPerRunCostBudgetCannotJudgeWhatItCannotPrice 钉住 #24 四态口径在预算上的推论：
// 一条可计价的行都没有时，累计金额必然是 0，但 0 在这里的意思是**不知道花了多少**，
// 不是「没花钱」。放行，且 Run 保持 running。
//
// 为什么值得单开一个用例：这是纯 codex 链（#32 里每条用量都 unpriced）的日常形态。
// 若把 0 当成「远低于上限」，预算在这类链上就永远不生效；若反过来当成「已超限」，
// 就会因为读不出价格而杀掉一个正常运转的 Run。两头都错，所以要钉在中间。
func TestPerRunCostBudgetCannotJudgeWhatItCannotPrice(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	t.Setenv(service.RunCostBudgetEnvVar, "0.01")

	ctx := context.Background()
	_, agentA, _, runtimeA, _ := newRuelCyclePair(t, "Ruel: can the per-run budget judge an unpriced run?")
	issueID := dbfx.Issue(t, "Ruel: per-run on an unpriced run", testutil.Cols{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": agentA,
	})
	// 十亿 token 的 unknown 模型：任一上限下都会「超」，如果实现把 unpriced 按 0 计入
	// 或把空判定当超限，这个用例就会红。
	taskID := ruelRunningTaskWithUsage(t, agentA, runtimeA, issueID, "unknown", 999_999_999, 0, 0)

	stopped, err := testHandler.TaskService.RuelEnforceRunCostBudget(ctx, mustRuelTask(t, taskID))
	if err != nil || stopped {
		t.Fatalf("算不出价格的 Run 被预算动了一次（stopped=%v err=%v）", stopped, err)
	}
	if status, reason := ruelTaskOutcome(t, taskID); status != "running" || reason != "" {
		t.Fatalf("Run = %q / %q, want running 且无失败原因（无法判定 ≠ 未超限，也不等于超限）", status, reason)
	}
}

// mustRuelTask 按 id 取回一个 Run，取不到就直接失败——闸门要的是一个活的
// db.AgentTaskQueue，不是随便造一个（状态、issue_id、agent_id 都得是真的）。
func mustRuelTask(t *testing.T, taskID string) db.AgentTaskQueue {
	t.Helper()
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
	if err != nil {
		t.Fatalf("load task %s: %v", taskID, err)
	}
	return task
}
