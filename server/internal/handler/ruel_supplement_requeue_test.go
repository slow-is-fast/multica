package handler

// #42 的集成测试。重点不是「端点返回 200」，而是三件事：
//
//  1. 评论真的重新进了队列，会被下一条 Run 的输入带上——库里要能查到那条 Run 带着
//     它（trigger 或 coalesced 都算）。只断言 200 等于什么都没验。
//  2. attempt_count > 0 的必须拒绝。这类回执的文字可能已经进了上下文，重放有可能
//     让它被读两遍，而那正是上游那条排除规则要防的。
//  3. 接不住的时候不许解绑。解绑了却发现没人接，这条评论就从「失败的回执」变成
//     「什么都没有」——比失败更糟，因为连重试的入口都看不到了。

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ruelNeverDeliveredReceipt 造出一个「投递从未发生」的失败回执：评论绑到了一个
// running 的 Run 上，那一轮随后结束，回执被 settle 成 failed / turn_ended，而
// attempt_count 仍是 0——它从未被 claim 过，agent 根本没见过这段文字。
//
// 返回的 commentID 就是那条追加指导，taskID 是它没能进去的那一轮。
func ruelNeverDeliveredReceipt(t *testing.T, claimed bool) (f supplementFixture, commentID string) {
	t.Helper()
	f = newSupplementFixture(t, "codex", "running", true)
	var created CommentResponse
	supplementRequest(t, f, "0199a4e8-22ce-7b01-bba5-333333333331", "这条没能送进去").Want(http.StatusCreated).JSON(&created)
	if claimed {
		// claim 会把 attempt_count 推到 1 并置为 delivering：投递尝试已经发生。
		if _, err := testHandler.Queries.ClaimNextTaskSupplement(t.Context(), parseUUID(f.taskID)); err != nil {
			t.Fatalf("claim 失败: %v", err)
		}
	}
	if w := completeTaskViaHandler(t, f.taskID, "done"); w.Code != http.StatusOK {
		t.Fatalf("结束那一轮失败: status %d: %s", w.Code, w.Body.String())
	}
	return f, created.ID
}

func ruelRequeueRequest(t *testing.T, f supplementFixture, commentID string) *testutil.Response {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/issues/"+f.issueID+"/tasks/"+f.taskID+"/supplements/"+commentID+"/requeue", nil)
	return testutil.Call(t, testHandler.RuelRequeueTaskSupplement,
		withURLParams(req, "id", f.issueID, "taskId", f.taskID, "commentId", commentID))
}

// ruelCommentCoveredByAnotherRun 是「被下一条 Run 真正消费」的库内证据：某条 Run
// 把它当作 trigger，或折进了 coalesced 集合。两者都算「输入里带这条评论」。
func ruelCommentCoveredByAnotherRun(t *testing.T, f supplementFixture, commentID string) int {
	t.Helper()
	return dbfx.Count(t, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1
		  AND id <> $2
		  AND (trigger_comment_id = $3 OR $3 = ANY(coalesced_comment_ids))
	`, f.issueID, f.taskID, commentID)
}

// TestRuelRequeueNeverDeliveredPutsItBackInTheQueue 是 #42 的主验收用例。
func TestRuelRequeueNeverDeliveredPutsItBackInTheQueue(t *testing.T) {
	f, commentID := ruelNeverDeliveredReceipt(t, false)

	// 前置：确认它确实是「从未投递」那一类，否则这个用例验的不是它想验的。
	before, err := testHandler.Queries.GetTaskSupplementForRun(t.Context(), db.GetTaskSupplementForRunParams{
		CommentID: parseUUID(commentID), TaskID: parseUUID(f.taskID), WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("读回执失败: %v", err)
	}
	if before.Status != "failed" || before.AttemptCount != 0 {
		t.Fatalf("回执 = %s / attempt %d，want failed / 0（这个用例要的是从未投递）", before.Status, before.AttemptCount)
	}

	var body struct {
		Requeued bool   `json:"requeued"`
		Dispatch string `json:"dispatch"`
	}
	ruelRequeueRequest(t, f, commentID).Want(http.StatusOK).JSON(&body)
	if !body.Requeued {
		t.Fatal("返回了 200 但 requeued=false")
	}

	// 绑定解除了。
	if n := dbfx.Count(t, `SELECT count(*) FROM task_supplement WHERE comment_id=$1 AND task_id=$2`,
		commentID, f.taskID); n != 0 {
		t.Fatalf("解除后仍留着 %d 条绑定", n)
	}
	// 而且有人接住了它——这才是「改为排到下一轮」的实质。
	if n := ruelCommentCoveredByAnotherRun(t, f, commentID); n == 0 {
		t.Fatalf("dispatch=%s，但没有任何一条 Run 的输入带上这条评论", body.Dispatch)
	}
}

// TestRuelRequeueRefusesWhatTheAgentMayAlreadyHaveRead 钉住判据的另一半。
//
// attempt_count > 0 说明投递尝试已经发生：文字可能已经进入上下文，ack 只是迟到。
// 这时重放有可能让它被读两遍。这个用例防止有人把判据放宽成「只要 failed 就给退路」。
func TestRuelRequeueRefusesWhatTheAgentMayAlreadyHaveRead(t *testing.T) {
	f, commentID := ruelNeverDeliveredReceipt(t, true)

	claimed, err := testHandler.Queries.GetTaskSupplementForRun(t.Context(), db.GetTaskSupplementForRunParams{
		CommentID: parseUUID(commentID), TaskID: parseUUID(f.taskID), WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("读回执失败: %v", err)
	}
	if claimed.AttemptCount == 0 {
		t.Fatal("claim 没把 attempt_count 推上去，这个用例验的不是它想验的")
	}

	ruelRequeueRequest(t, f, commentID).Want(http.StatusConflict)
	// 拒绝时**不许解绑**：解绑了又没接住，这条评论就彻底消失了。
	if n := dbfx.Count(t, `SELECT count(*) FROM task_supplement WHERE comment_id=$1 AND task_id=$2`,
		commentID, f.taskID); n != 1 {
		t.Fatalf("被拒绝后绑定变成了 %d 条，want 1（拒绝不得有副作用）", n)
	}
}

// TestRuelRequeueRefusesAReceiptThatIsNotFailed：只有 failed 才有「退路」这回事。
func TestRuelRequeueRefusesAReceiptThatIsNotFailed(t *testing.T) {
	f := newSupplementFixture(t, "codex", "running", true)
	var created CommentResponse
	supplementRequest(t, f, "0199a4e8-22ce-7b01-bba5-333333333332", "还在 pending").Want(http.StatusCreated).JSON(&created)

	ruelRequeueRequest(t, f, created.ID).Want(http.StatusConflict)
	if n := dbfx.Count(t, `SELECT count(*) FROM task_supplement WHERE comment_id=$1 AND task_id=$2`,
		created.ID, f.taskID); n != 1 {
		t.Fatalf("pending 的回执被重排了，绑定剩 %d 条", n)
	}
}

// TestRuelRequeueRefusesAnUnboundComment：没绑过的评论不走这条路——它本来就在队列里，
// 重排它只会造出重复的一轮。
func TestRuelRequeueRefusesAnUnboundComment(t *testing.T) {
	f := newSupplementFixture(t, "codex", "running", true)
	other := dbfx.Comment(t, f.issueID, "跟那一轮没关系")
	ruelRequeueRequest(t, f, other).Want(http.StatusNotFound)
}
