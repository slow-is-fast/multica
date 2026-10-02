-- name: ListConversationRootOwners :many
-- Preserve the newest non-null squad per agent, including terminal tasks.
-- Routing does not need execution context, results, or other issue threads.
--
-- Thread ownership is keyed on comment_thread_id (migration 451 derives it
-- from the trigger comment and it never changes afterwards), NOT on
-- trigger_comment_id: MergeCommentIntoPendingTask deliberately re-stamps
-- trigger_comment_id to the newest folded comment (MUL-4302), so a thread's
-- first merged reply silently erased its own owner and every later reply in
-- that thread routed to nothing — no receipt, no run. trigger_comment_id is
-- still accepted so pre-451 rows that predate the derived column keep their
-- owner. Callers always pass the thread ROOT, for which the two are equal.
SELECT DISTINCT ON (agent_id) agent_id, squad_id
FROM agent_task_queue
WHERE issue_id = $1 AND agent_id IS NOT NULL
  AND (comment_thread_id = $2 OR trigger_comment_id = $2)
ORDER BY agent_id, (squad_id IS NOT NULL) DESC, created_at DESC, id DESC;
