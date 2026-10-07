package handler

// Ruel 新增：#25 —— 闸门从 handler 的评论收口挪到 service 层之后，必须证明它挡得住
// **非评论**的委派路径。
//
// 挑的是「Agent 自己建 Issue」这条（`origin_type='agent_create'`），因为它是 M4 那版闸门
// 最大的盲区：完全不过评论，而且一次 Run 可以建多个 Issue（扇出）。六条入口的清单与为什么
// 从评论判改成从父 Run 判，见 ruel/docs/reference/multica-delegation-entrypoints.md。
//
// 这里直接构造一条「由某个 Run 建出来的 Issue」再入队，而不是走 HTTP 建 Issue：打标那一半
// （handler/issue.go 的 agent_create 分支）需要一个 mat_ 令牌才走得通，而本用例要验的是
// 入队这一半——两半拼起来才是完整路径，打标那一半由下面第一个断言间接守住（见内注释）。

import (
	"context"
	"errors"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// buildRuelDelegationChain 造一条 n 个 Run 的委派链，返回链尾。第 0 个是链根（深度 0），
// 第 i 个深度 i；链尾深度 n-1。
func buildRuelDelegationChain(t *testing.T, agentID, runtimeID, issueID string, n int) string {
	t.Helper()
	var prev string
	for i := 0; i < n; i++ {
		cols := testutil.Cols{
			"runtime_id": runtimeID, "issue_id": issueID, "status": "completed",
			"originator_user_id": testUserID, "accountable_user_id": testUserID,
		}
		if prev != "" {
			cols["delegated_from_task_id"] = prev
		}
		prev = dbfx.Task(t, agentID, cols)
	}
	return prev
}

// newRuelAgentCreatedIssue 造一条「由 originTaskID 这个 Run 建出来、指派给 assigneeAgentID」
// 的 Issue。origin_type / origin_id 是 handler 建 Issue 时打的那两个标（issue.go 的
// agent_create 分支），这里直接写成列，等价于打标后的结果。
func newRuelAgentCreatedIssue(t *testing.T, title, creatorAgentID, assigneeAgentID, originTaskID string) string {
	t.Helper()
	return dbfx.Issue(t, title, testutil.Cols{
		"status":        "in_progress",
		"assignee_type": "agent",
		"assignee_id":   assigneeAgentID,
		"creator_type":  "agent",
		"creator_id":    creatorAgentID,
		"origin_type":   "agent_create",
		"origin_id":     originTaskID,
	})
}

// TestRuelDelegationDepthCoversAgentCreatedIssue 是「非评论路径同样被挡」的证明。
//
// 分两半，缺哪一半都不算证明：
//
//  1. **先证明这真的是一条委派路径**——链尾深度在上限之内时，为这条 Issue 起的 Run 必须
//     真的把 origin Run 记成 `delegated_from_task_id`。若这一半不成立，后面那半「被拦住」
//     可能只是因为这条路径压根不走委派，而不是闸门生效了。
//  2. **再证明越限时被拦**——链尾正好在深度上限时，同一个操作必须被拒。
func TestRuelDelegationDepthCoversAgentCreatedIssue(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	chainIssueID, agentA, agentB, runtimeA, _ := newRuelCyclePair(t, "Ruel: does the gate cover agent-created issues?")

	// ---- 第 1 半：上限之内，委派关系照常建立 ----
	// 12 个 Run：链尾深度 11，它当爹时新 Run 是 12，正好等于上限，应当放行。
	withinLimit := buildRuelDelegationChain(t, agentA, runtimeA, chainIssueID, service.RuelDelegationDepthLimit)
	okIssueID := newRuelAgentCreatedIssue(t, "Ruel: agent-created issue inside the depth limit", agentA, agentB, withinLimit)
	task, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, mustRuelIssue(t, okIssueID))
	if err != nil {
		t.Fatalf("上限之内为 Agent 建的 Issue 入队失败: %v", err)
	}
	var delegated string
	dbfx.QueryRow(t,
		`SELECT COALESCE(delegated_from_task_id::text, '') FROM agent_task_queue WHERE id = $1`, task.ID,
	).Scan(&delegated)
	if delegated != withinLimit {
		t.Fatalf("Agent 建 Issue 这条路径没接上委派关系：delegated_from = %q, want %q。"+
			"若这条不成立，本用例的「被拦住」就不能证明闸门生效", delegated, withinLimit)
	}
	t.Logf("上限之内：Agent 建的 Issue 起了 Run %s，父是 %s（深度 %d）",
		task.ID, withinLimit, service.RuelDelegationDepthLimit-1)

	// ---- 第 2 半：越限，必须被拦 ----
	// 13 个 Run：链尾深度 12，它当爹时新 Run 是 13，越过上限。
	atLimit := buildRuelDelegationChain(t, agentA, runtimeA, chainIssueID, service.RuelDelegationDepthLimit+1)
	deepIssueID := newRuelAgentCreatedIssue(t, "Ruel: agent-created issue past the depth limit", agentA, agentB, atLimit)
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, mustRuelIssue(t, deepIssueID)); !errors.Is(err, service.ErrDelegationDepthExceeded) {
		t.Fatalf("越限的 Agent 建 Issue 入队结果 = %v, want ErrDelegationDepthExceeded", err)
	}
	// 拒绝必须是**没起 Run**，不是起了又丢。
	var runs int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, deepIssueID).Scan(&runs)
	if runs != 0 {
		t.Fatalf("被拒的那条 Issue 上还有 %d 个 Run, want 0", runs)
	}
	t.Logf("越限：链尾深度 %d，为它建的 Issue 没有起 Run", service.RuelDelegationDepthLimit)
}

// mustRuelIssue 取一条 Issue 的完整行。入队要的是 db.Issue 而不是 id，因为归因要看
// origin_type / origin_id 这两个列。
func mustRuelIssue(t *testing.T, issueID string) db.Issue {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue %s: %v", issueID, err)
	}
	return issue
}
