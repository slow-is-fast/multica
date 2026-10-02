package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// A thread's owner used to be looked up by trigger_comment_id = <thread root>.
// MergeCommentIntoPendingTask deliberately re-stamps trigger_comment_id to the
// newest folded comment (MUL-4302), so the FIRST reply that merged into a
// queued run erased the very row that made its own thread routable: every later
// reply in that thread resolved to no trigger at all — no delivery receipt, no
// follow-up run, and completion reconcile could not compensate because it
// re-routes through the same lookup. Ownership is keyed on comment_thread_id,
// which migration 451 derives once at insert and which no merge ever rewrites.
func TestConversationRootOwnerSurvivesTriggerRestamp(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var agentID, runtimeID string
	dbfx.QueryRow(t,
		`SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND runtime_id IS NOT NULL LIMIT 1`,
		testWorkspaceID).Scan(&agentID, &runtimeID)

	issueID := dbfx.Issue(t, "thread owner restamp fixture", testutil.Cols{
		"status":        "in_progress",
		"number":        999031,
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})
	rootID := dbfx.Comment(t, issueID, "root of the thread")
	replyID := dbfx.Comment(t, issueID, "first reply", testutil.Cols{"parent_id": rootID})

	var taskID string
	dbfx.QueryRow(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, trigger_comment_id,
			delivered_comment_ids, status, priority, created_at)
		VALUES ($1, $2, $3, $4, ARRAY[$4::uuid], 'queued', 0, now())
		RETURNING id
	`, agentID, runtimeID, issueID, rootID).Scan(&taskID)
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID) })

	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	root, err := testHandler.Queries.GetComment(ctx, parseUUID(rootID))
	if err != nil {
		t.Fatal(err)
	}

	// Pre-merge: the root is the trigger, so the thread has an owner. This is
	// the state the first reply is routed in, which is why it merges at all.
	got, owned := testHandler.routeConversationOwnersForRoot(ctx, issue, root, testUserID, commentTriggerComputeOptions{})
	if !owned || len(got) != 1 {
		t.Fatalf("before restamp: owned=%t owners=%d, want 1", owned, len(got))
	}

	// What the merge does: the folded reply becomes the trigger and the old
	// trigger moves into the coalesced set.
	dbfx.Exec(t,
		`UPDATE agent_task_queue SET trigger_comment_id = $2, coalesced_comment_ids = ARRAY[$1::uuid] WHERE id = $3`,
		rootID, replyID, taskID)

	got, owned = testHandler.routeConversationOwnersForRoot(ctx, issue, root, testUserID, commentTriggerComputeOptions{})
	if !owned || len(got) != 1 {
		t.Fatalf("after restamp: owned=%t owners=%d, want 1 — the thread lost its owner", owned, len(got))
	}
	if uuidToString(got[0].Agent.ID) != agentID {
		t.Fatalf("owner = %s, want %s", uuidToString(got[0].Agent.ID), agentID)
	}
}

// The user-visible symptom: three comments in one thread — root, reply, reply —
// where the middle one already merged and became the run's trigger. The third
// arrives while the run is busy, so only completion reconcile can cover it.
// Before the fix the reconcile re-routed it through the same owner lookup,
// found nothing, and dropped it: no receipt, no run, no further retry.
func TestCompleteTask_ReconcilesThirdSameThreadComment(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var agentID, runtimeID string
	dbfx.QueryRow(t,
		`SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND runtime_id IS NOT NULL LIMIT 1`,
		testWorkspaceID).Scan(&agentID, &runtimeID)

	issueID := dbfx.Issue(t, "third same-thread comment fixture", testutil.Cols{
		"status":        "in_progress",
		"number":        999032,
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})
	rootID := dbfx.Comment(t, issueID, "root R", testutil.Cols{
		"created_at": testutil.Raw("now() - interval '10 minutes'"),
	})
	replyOneID := dbfx.Comment(t, issueID, "reply 1", testutil.Cols{
		"parent_id":  rootID,
		"created_at": testutil.Raw("now() - interval '9 minutes'"),
	})
	// The comment that used to be swallowed.
	dbfx.Comment(t, issueID, "reply 2", testutil.Cols{
		"parent_id":  rootID,
		"created_at": testutil.Raw("now() - interval '1 minute'"),
	})

	// The run as a merge leaves it: reply 1 is the trigger, root R is coalesced,
	// and both are already receipted as delivered.
	var taskID string
	dbfx.QueryRow(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, trigger_comment_id,
			coalesced_comment_ids, delivered_comment_ids, status, priority, created_at, started_at)
		VALUES ($1, $2, $3, $4, ARRAY[$5::uuid], ARRAY[$4::uuid, $5::uuid], 'running', 0,
			now() - interval '10 minutes', now() - interval '5 minutes')
		RETURNING id
	`, agentID, runtimeID, issueID, replyOneID, rootID).Scan(&taskID)
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID) })

	if w := completeTaskViaHandler(t, taskID, "done"); w.Code != http.StatusOK {
		t.Fatalf("CompleteTask: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if n := pendingTaskCountForAgentIssue(t, issueID, agentID); n != 1 {
		t.Fatalf("expected exactly 1 follow-up for the third same-thread comment, got %d", n)
	}
}
