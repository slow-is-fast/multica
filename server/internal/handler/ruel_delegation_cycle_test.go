package handler

// Ruel 新增：P0-5「循环 / 重复委派被拦截」的实测探针。
//
// 代码里对这件事的说法不一致，只能靠跑来定：
//
//  1. 委派写入点只拷贝父 id，全代码库检索不到 depth / chain / max 之类的深度闸门。
//     migrations/184 的注释说委派环路「对归因无害」——那句话回答的是归因，不是 Run。
//  2. `comment.go` 里 resolveMentionedAgentCommentTriggers 的注释说：自提及是允许的，
//     失控的循环由 `HasPendingTaskForIssueAndAgentInThread` 去重防住。
//
// 实测下来两件事都得修正：
//
//   - 那条去重是**按线程**的。同一条 Issue 上换一个线程重复委派，照样新起一个 Run
//     （见 TestRuelDuplicateDelegationOtherThreadIsNotCoalesced）。
//   - 另有一道没写在任何注释里的闸门：`agent_access.go` 的 invokeOriginatorFromRequest
//     只认**非终态**的发言 Run。已完成的 Run 解析不出 human originator，private agent
//     的委派会被直接拒掉。所以「跑完再派」这条路径根本走不通；环路真正可能的走法是
//     「在自己的 Run 还没结束时就发出委派，然后才结束这一轮」——本文件测的就是这种。
//
// 结论：交替循环**没有尽头**（实测 20 轮 → 21 个 Run）。据此在 fork 里补了深度闸门
// （ruel_delegation_depth.go），本文件随之改为验证链会在上限处停住，并且停得有说法。

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// claimRuelCycleRun 让 Agent 的 runtime 认领它的下一轮 Run（queued → dispatched）。
func claimRuelCycleRun(t *testing.T, runtimeID string) *AgentTaskResponse {
	t.Helper()
	req := newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil,
		testWorkspaceID, "ruel-delegation-cycle")
	req = withURLParam(req, "runtimeId", runtimeID)
	var response struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&response)
	return response.Task
}

// startRuelCycleRun 让一轮已认领的 Run 跑起来。
//
// 不能停在 queued：授权闸门只认非终态的发言 Run，而 StartTask 只接受已认领的任务。
func startRuelCycleRun(t *testing.T, ctx context.Context, taskID string) {
	t.Helper()
	if _, err := testHandler.TaskService.StartTask(ctx, parseUUID(taskID)); err != nil {
		t.Fatalf("start task %s: %v", taskID, err)
	}
}

// finishRuelCycleRun 结束一轮 Run。下一轮委派回同一个 Agent 时，按线程的重去才查不到活跃 Run。
func finishRuelCycleRun(t *testing.T, taskID string) {
	t.Helper()
	req := newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/complete",
		map[string]any{"output": "round finished"}, testWorkspaceID, "ruel-delegation-cycle")
	req = withURLParam(req, "taskId", taskID)
	testutil.Call(t, testHandler.CompleteTask, req).Want(http.StatusOK)
}

// postRuelDelegationComment 让 from 用一条 @mention 评论把活派给 to。
//
// fromTaskID 必须是 from 自己**正在跑**的 Run，否则授权闸门拿不到 human originator。
// parentID 不能一直为空：被评论触发起来的 Run 不允许再开新的顶层线程（实测 409
// "comment-triggered tasks cannot create top-level comments"），只能在同一线程里回复。
// 这条约束把委派链钉在一条线程上——而按线程的重去恰好也只看这一条线程。
func postRuelDelegationComment(t *testing.T, issueID, from, to, fromTaskID, parentID string) CommentResponse {
	t.Helper()
	content := fmt.Sprintf("[@Next](mention://agent/%s) handing this round over", to)
	body := map[string]any{"content": content}
	if parentID != "" {
		body["parent_id"] = parentID
	}
	req := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", body), "id", issueID)
	req.Header.Set("X-Agent-ID", from)
	req.Header.Set("X-Task-ID", fromTaskID)
	var response CommentResponse
	testutil.Call(t, testHandler.CreateComment, req).Want(http.StatusCreated).JSON(&response)
	if response.AuthorType != "agent" || response.SourceTaskID == nil || *response.SourceTaskID != fromTaskID {
		t.Fatal("delegation comment lost its authenticated source task")
	}
	return response
}

// latestQueuedRun 取某个 Agent 在这条 Issue 上最新入队的 Run。
func latestQueuedRun(t *testing.T, issueID, agentID string) string {
	t.Helper()
	var id string
	// Scan 失败即 t.Fatal 并把 SQL 打出来——查不到就是「这一轮没有入队」，
	// 那正是本用例要抓的事。
	dbfx.QueryRow(t,
		`SELECT id FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' ORDER BY created_at DESC LIMIT 1`,
		issueID, agentID).Scan(&id)
	return id
}

// newRuelCyclePair 造两个能互相委派的 Agent 与一条 Issue。
func newRuelCyclePair(t *testing.T, title string) (issueID, agentA, agentB, runtimeA, runtimeB string) {
	t.Helper()
	runtimeA = dbfx.Runtime(t, "Ruel cycle A runtime")
	agentA = dbfx.Agent(t, "ruel-cycle-a", runtimeA, testutil.Cols{"max_concurrent_tasks": 3})
	runtimeB = dbfx.Runtime(t, "Ruel cycle B runtime")
	agentB = dbfx.Agent(t, "ruel-cycle-b", runtimeB, testutil.Cols{"max_concurrent_tasks": 3})
	issueID = dbfx.Issue(t, title, testutil.Cols{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": agentA,
	})
	return issueID, agentA, agentB, runtimeA, runtimeB
}

// queuedRuelRun 取某个 Agent 在这条 Issue 上最新入队的 Run；没有就返回 false。
//
// 不能像 dbfx.QueryRow 那样查不到就 Fatal——闸门生效的那一轮**正是**查不到，那是要验的
// 结果，不是测试写错了。
func queuedRuelRun(t *testing.T, issueID, agentID string) (string, bool) {
	t.Helper()
	var id string
	err := testPool.QueryRow(context.Background(),
		`SELECT id FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' ORDER BY created_at DESC LIMIT 1`,
		issueID, agentID).Scan(&id)
	if err != nil {
		return "", false
	}
	return id, true
}

// TestRuelDelegationChainStopsAtDepthLimit 守那条补上的深度闸门。
//
// 两个 Agent 各自在 Run 未结束时交替派单——这是实测中唯一能绕开上游三道部分闸门的走法
// （换人躲开线程去重、未终态躲开授权闸门、回复躲开线程约束）。闸门生效后，链必须在
// service.RuelDelegationDepthLimit 处停住，而且**停得有说法**：拒绝要回到调用方（reason_code），
// 不能是静默丢弃——派单的 Agent 以为自己派出去了、实际没人接，是最糟的一种失败。
func TestRuelDelegationChainStopsAtDepthLimit(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	issueID, agentA, agentB, runtimeA, runtimeB := newRuelCyclePair(t, "Ruel: does a delegation chain stop?")

	current := dbfx.Task(t, agentA, testutil.Cols{
		"runtime_id": runtimeA, "issue_id": issueID, "status": "running",
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
	})

	// 委派链被 409 钉在同一条线程里：第一轮开线程，之后每轮都回复在前一轮的评论下。
	var parent string
	hops := 0
	refused := false
	for i := 0; i < service.RuelDelegationDepthLimit+3; i++ {
		from, to, toRuntime := agentA, agentB, runtimeB
		if i%2 == 1 {
			from, to, toRuntime = agentB, agentA, runtimeA
		}
		comment := postRuelDelegationComment(t, issueID, from, to, current, parent)
		parent = comment.ID
		next, ok := queuedRuelRun(t, issueID, to)
		// from 这一轮结束，下一轮才可能派回它。
		finishRuelCycleRun(t, current)
		if !ok {
			// 闸门拦住了：没有新的 Run。拒绝必须带回原因。
			assertRuelDepthRefusal(t, comment, to)
			refused = true
			break
		}
		if claimed := claimRuelCycleRun(t, toRuntime); claimed == nil || claimed.ID != next {
			t.Fatalf("round %d: %s did not claim its delegated run", i+1, to)
		}
		startRuelCycleRun(t, ctx, next)
		var delegated string
		dbfx.QueryRow(t,
			`SELECT COALESCE(delegated_from_task_id::text, '') FROM agent_task_queue WHERE id = $1`, next,
		).Scan(&delegated)
		if delegated != current {
			t.Fatalf("round %d: 委派关系没接上，delegated_from = %q, want %q", i+1, delegated, current)
		}
		current = next
		hops++
	}

	if !refused {
		t.Fatalf("链跑了 %d 次转手仍未被拦住，上限是 %d", hops, service.RuelDelegationDepthLimit)
	}
	if hops != service.RuelDelegationDepthLimit {
		t.Fatalf("转手 %d 次才停，want %d（上限之内应当照常放行，越限才拦）", hops, service.RuelDelegationDepthLimit)
	}
	var total int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&total)
	if total != service.RuelDelegationDepthLimit+1 {
		t.Fatalf("Run 总数 = %d, want %d（起始 1 + 上限内的 %d 次转手）", total, service.RuelDelegationDepthLimit+1, service.RuelDelegationDepthLimit)
	}
	t.Logf("交替委派在第 %d 次转手处被拦住，这条 Issue 上共 %d 个 Run", hops, total)
}

// assertRuelDepthRefusal 校验「拒绝」被如实上报，而不是静默丢弃。
func assertRuelDepthRefusal(t *testing.T, comment CommentResponse, targetAgentID string) {
	t.Helper()
	for _, outcome := range comment.TriggerOutcomes {
		if outcome.TargetID != targetAgentID {
			continue
		}
		if outcome.Status != DispatchBlocked {
			t.Fatalf("被拦的那一轮 status = %q, want %q", outcome.Status, DispatchBlocked)
		}
		if outcome.ReasonCode != ReasonDelegationDepthExceeded {
			t.Fatalf("reason_code = %q, want %q", outcome.ReasonCode, ReasonDelegationDepthExceeded)
		}
		return
	}
	t.Fatalf("响应里没有 %s 的触发结果（%d 条）：拒绝必须回到调用方，不能静默丢弃",
		targetAgentID, len(comment.TriggerOutcomes))
}

// TestRuelDuplicateDelegationSameThreadIsCoalesced 测去重守得住的那一半。
//
// 同一个 Agent 在同一条 Issue / **同一个线程**里已有未完成的 Run 时，重复委派会被合并而
// 不是再起一个。把它单独测出来，是为了把「拦得住的」和「拦不住的」分开：拦得住的是同线程
// 的重复，拦不住的是换线程的重复与交替循环。
func TestRuelDuplicateDelegationSameThreadIsCoalesced(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	issueID, agentA, agentB, runtimeA, _ := newRuelCyclePair(t, "Ruel: is a repeated delegation coalesced?")
	leader := dbfx.Task(t, agentA, testutil.Cols{
		"runtime_id": runtimeA, "issue_id": issueID, "status": "running",
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
	})
	root := dbfx.Comment(t, issueID, "delegation thread root", testutil.Cols{
		"author_type": "agent", "author_id": agentA, "source_task_id": leader,
	})

	// 同一线程里连派两次给 B，中间不让 B 的 Run 终态。
	_ = postRuelDelegationComment(t, issueID, agentA, agentB, leader, root)
	first := latestQueuedRun(t, issueID, agentB)
	_ = postRuelDelegationComment(t, issueID, agentA, agentB, leader, root)
	_ = postRuelDelegationComment(t, issueID, agentA, agentB, leader, root)

	var count int
	dbfx.QueryRow(t,
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, agentB,
	).Scan(&count)
	if count != 1 {
		t.Fatalf("B runs = %d, want 1: 同线程的重复委派应被合并（首个 Run = %s）", count, first)
	}
}

// TestRuelDuplicateDelegationOtherThreadIsNotCoalesced 记去重够不到的那一半。
//
// 换一个线程重复委派同一件事，去重按 (issue, agent, thread) 记，于是照样新起一个 Run。
// 这不是缺陷判断，是边界记录——判据里「重复被拦截」到底覆盖到哪一层，得先知道这条线在哪。
func TestRuelDuplicateDelegationOtherThreadIsNotCoalesced(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	issueID, agentA, agentB, runtimeA, _ := newRuelCyclePair(t, "Ruel: does a new thread escape the dedupe?")
	leader := dbfx.Task(t, agentA, testutil.Cols{
		"runtime_id": runtimeA, "issue_id": issueID, "status": "running",
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
	})

	// 两个不同的线程各派一次，B 的 Run 始终不终态。
	_ = postRuelDelegationComment(t, issueID, agentA, agentB, leader, "")
	other := dbfx.Comment(t, issueID, "second thread root", testutil.Cols{
		"author_type": "agent", "author_id": agentA, "source_task_id": leader,
	})
	_ = postRuelDelegationComment(t, issueID, agentA, agentB, leader, other)

	var count int
	dbfx.QueryRow(t,
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, agentB,
	).Scan(&count)
	if count != 2 {
		t.Fatalf("B runs = %d, want 2: 换线程后去重不再命中（现状记录，见本用例注释）", count)
	}
}
