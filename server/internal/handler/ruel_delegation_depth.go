package handler

// Ruel 新增：委派链的深度闸门。
//
// 上游没有这道闸门。委派只在子 Run 上拷一份父 id（`delegated_from_task_id`），没有任何
// 计数、深度或时间窗会累积。实测两个 Agent 各自在 Run 未结束时交替派单，20 轮下来一条
// Issue 上有 21 个 Run，而且没有成本预算兜底——它会一直跑到有人发现为止。
//
// 上游唯一声称能防住这事的是 `HasPendingTaskForIssueAndAgentInThread` 的去重，而它按
// (issue, agent, thread) 记：交替循环每轮换人、且上一轮已经终态，一次都命中不了。
//
// 为什么不直接数「这条 Issue 上跑了多少个 Run」：那会把正常的长任务流一起砍掉——一个
// Issue 被反复补充评论、重跑十几次是合理的。要拦的是**委派**这件事自己转起来，所以判据
// 是链的深浅，不是数目的多少。

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
)

// ruelDelegationDepthLimit 是一条委派链上最多允许多少次转手。
//
// 取值偏松，是刻意的：这道闸门要挡的是「转不起来又停不下来」的循环，不是正常的深链。
// 一个 leader 派给 worker、worker 回报、leader 再派给另一个 worker 的合理流程，十来次
// 转手已经很深了；真到 12 次还在互相派，几乎不可能是人想要的。
//
// 越限的行为是**拒绝入队并给出原因**（dispatch reason 会回到调用方），不是静默丢弃。
// 静默丢弃是最糟的一种：派单的 Agent 以为自己派出去了，实际没人接，而界面上什么都不说。
const ruelDelegationDepthLimit = 12

// ruelDelegationChainTooDeep 判断这条触发会不会让委派链再长一环。
//
// 链的深浅由「写这条评论的那个 Run 自己有多深」决定：新 Run 是它的子 Run，深度加一。
// 没有 source_task_id 的评论（人写的、或系统写的）不产生委派关系，直接放行——闸门只对
// Agent 之间的转手生效。
//
// 沿 delegated_from_task_id 往上走，走一步数一步；走到链根或读不到就停。循环本身不可能
// 出现在这一列上（每次转手都指向一个已经存在的、更早的 Run），但循环防护仍然由 limit
// 兜着，读数失败也一律当作链到此为止——**读不出来不能变成拒绝**，那会把一次普通的查询
// 抖动变成派单失败。
func (h *Handler) ruelDelegationChainTooDeep(ctx context.Context, triggerCommentID pgtype.UUID) (depth int, tooDeep bool) {
	if !triggerCommentID.Valid {
		return 0, false
	}
	comment, err := h.Queries.GetComment(ctx, triggerCommentID)
	if err != nil || !comment.SourceTaskID.Valid {
		return 0, false
	}
	current := comment.SourceTaskID
	for current.Valid {
		task, err := h.Queries.GetAgentTask(ctx, current)
		if err != nil {
			return depth, false
		}
		if !task.DelegatedFromTaskID.Valid {
			return depth, false
		}
		depth++
		if depth >= ruelDelegationDepthLimit {
			return depth, true
		}
		current = task.DelegatedFromTaskID
	}
	return depth, false
}

// ruelLogDelegationDepthExceeded 记一条能被搜到的日志。
//
// 拒绝必须留痕：这是唯一能回答「为什么这个派单没生效」的地方——界面上有 reason，但排查
// 一条已经跑歪的链时，要看的是它到底转了多少次手、停在哪个 Issue 上。
func ruelLogDelegationDepthExceeded(ctx context.Context, issueID, agentID, triggerCommentID string, depth int) {
	slog.WarnContext(ctx, "ruel: 委派链到了深度上限，拒绝再转一次手",
		"issue_id", issueID,
		"agent_id", agentID,
		"trigger_comment_id", triggerCommentID,
		"depth", depth,
		"limit", ruelDelegationDepthLimit,
	)
}
