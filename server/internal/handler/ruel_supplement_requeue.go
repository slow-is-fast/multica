package handler

// Ruel 新增：给投递失败的追加指导一个「改为排到下一轮」的出口（#42）。
//
// 这是 PRD 6.2.1 写死的硬约束，不是可选优化——原文是「没有这个按钮就不该提供
// 注入选项」。理由是一条实测发生过的丢失路径：评论绑定成功后即被移出补给范围，
// 那一轮结束时 `SettleTerminalTaskSupplements` 把回执 settle 成 failed，此后
// `ListReconcilableCommentsForIssueSince` 的 NOT EXISTS 子句永久排除它。
//
// 上游那条排除是**有道理的**：它防止在 partially 已读的情况下重复起一轮。代价是
// 让失败变成终局。这条端点是那个代价的出口，但只开一半——
//
// ## 只做「从未投递」，判据是 attempt_count = 0
//
// 三类失败语义不同，混在一起处理会把「重放不存在重复」和「可能已经读过」当成一回事：
//
//   - **从未投递**（attempt_count = 0）：那一轮结束前这条从没被 claim 过，agent 根本
//     没见过这段文字，重放不存在重复。← 本端点只做这一类。
//   - **claim 过但没进上下文**（attempt_count > 0，无 delivered）：投递尝试已经发生，
//     文字可能已经进入上下文而 ack 只是迟到。
//   - **投递成功但那一轮随后失败**：随 Run 失败重放，语义又不一样。
//
// 后两类不在这里处理，也不是本端点能判的——判它需要 adapter 侧的确认。
//
// ## 顺序：先入队，再解绑
//
// 入队是「接住」，解绑是「解除排除」。这个顺序不能反过来：先解绑再入队，中间任何
// 一次失败都会让评论既没有被谁接住、也不再出现在失败回执里——那比失败更糟，人连
// 重试的入口都看不到了。先入队保证「有人接住了」之后才解除排除；入队失败时回执
// 保持 failed，按钮还在，用户可以再点一次。
//
// 副作用是入队成功、解绑失败时会留一条孤儿绑定。这条绑定会让后续 reconcile 跳过
// 该评论，但它已经在队列里、会被 delivered，所以不会丢——代价只是回执上多一行
// 历史。反过来（解绑成功、入队失败）会真的丢，所以选前者。

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RuelRequeueTaskSupplement 把一条从未投递的追加指导退回队列。
//
// 成功后这条评论重新进入待处理队列，会被下一次 Run 的输入带上——不是等某个后续
// Run 恰好完成时的 reconcile，而是现在就走一遍正常的入队路径（有 pending task 就
// 折进去，没有就新建一轮）。
func (h *Handler) RuelRequeueTaskSupplement(w http.ResponseWriter, r *http.Request) {
	issue, task, _, ok := h.loadTaskSupplementTarget(w, r)
	if !ok {
		return
	}
	commentID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "commentId"), "comment id")
	if !ok {
		return
	}
	ctx := r.Context()

	// 先看回执，再决定能不能重排。attempt_count 是唯一的判据：读它比读 status 更
	// 准——status 只说「失败了」，说不出「失败之前有没有送出去过」。
	existing, err := h.Queries.GetTaskSupplementForRun(ctx, db.GetTaskSupplementForRunParams{
		CommentID: commentID, TaskID: task.ID, WorkspaceID: issue.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErrorCode(w, http.StatusNotFound, "task_supplement_not_found", "this message is not bound to that run")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load additional message receipt")
		return
	}
	if existing.Status != "failed" {
		writeErrorCode(w, http.StatusConflict, "task_supplement_not_failed",
			"only a failed additional message can be moved to the next run")
		return
	}
	if existing.AttemptCount != 0 {
		// 投递尝试已经发生过：文字可能已经进了上下文，ack 只是迟到。这时重放有可能
		// 让它被读两遍，而这正是上游那条排除规则要防的。不给出口，也不假装安全。
		writeErrorCode(w, http.StatusConflict, "task_supplement_already_attempted",
			"this message was already handed to that run; replaying it could repeat it")
		return
	}

	comment, err := h.Queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{
		ID: commentID, WorkspaceID: issue.WorkspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "comment not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load comment")
		return
	}
	if comment.DeletedAt.Valid {
		writeErrorCode(w, http.StatusConflict, "comment_deleted", "a deleted comment is no longer input")
		return
	}

	// 复用补给路径的路由计算：重新判一遍这条评论现在会触发谁，然后只保留那个
	// 错过了它的 agent——绝不能把整条 fan-out 都叫醒，那会把一次「补发给 A」变成
	// 「给所有人各起一轮」。
	var parentComment *db.Comment
	if comment.ParentID.Valid {
		if parent, perr := h.Queries.GetCommentInWorkspace(ctx, db.GetCommentInWorkspaceParams{
			ID: comment.ParentID, WorkspaceID: issue.WorkspaceID,
		}); perr == nil {
			parentComment = &parent
		}
	}
	actorType := comment.AuthorType
	actorID := uuidToString(comment.AuthorID)
	originatorUserID := actorID
	if actorType != "member" {
		originatorUserID = uuidToString(h.TaskService.ResolveOriginatorFromTriggerComment(ctx, issue.WorkspaceID, comment.ID))
	}
	triggers, _ := h.computeCommentAgentTriggers(ctx, issue, comment.Content, parentComment, actorType, actorID, commentTriggerComputeOptions{
		ExcludeTriggerCommentID: comment.ID,
		AuthoringTaskID:         comment.SourceTaskID,
		OriginatorUserID:        originatorUserID,
	})
	agentID := uuidToString(task.AgentID)
	scoped := make([]commentAgentTrigger, 0, 1)
	for _, trigger := range triggers {
		if uuidToString(trigger.Agent.ID) == agentID {
			scoped = append(scoped, trigger)
		}
	}
	if len(scoped) == 0 {
		// 这条评论现在路由不到那个 agent（可能权限变了、@mention 撤了）。此时解绑
		// 只会让它从「失败的回执」变成「什么都没有」，所以拒绝。
		writeErrorCode(w, http.StatusConflict, "task_supplement_no_route",
			"this message no longer routes to that agent")
		return
	}

	res := h.enqueueCommentAgentTriggers(ctx, issue, comment.ID, scoped)[agentID]
	if res.status == DispatchBlocked {
		// 没接住。保持回执不动，让人还能看见它、还能再点一次。
		writeErrorCode(w, http.StatusConflict, "task_supplement_requeue_blocked",
			"the next run could not be scheduled for this message")
		return
	}

	// 接住了才解绑。这里失败不是静默的：回执还在，队列里也已经有了，只是后续
	// reconcile 还会跳过它——所以记 error 而不是 warn。
	if _, err := h.Queries.RuelRequeueTaskSupplement(ctx, db.RuelRequeueTaskSupplementParams{
		CommentID: commentID, TaskID: task.ID, WorkspaceID: issue.WorkspaceID,
	}); err != nil {
		slog.ErrorContext(ctx, "ruel: 评论已入队但解绑失败，回执仍在",
			"comment_id", uuidToString(commentID), "task_id", uuidToString(task.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to release the additional message binding")
		return
	}
	h.publishCommentSupplementUpdate(ctx, issue.WorkspaceID, commentID)
	writeJSON(w, http.StatusOK, map[string]any{
		"requeued": true,
		"dispatch": string(res.status),
		"agent_id": agentID,
	})
}
