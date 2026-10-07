package service

// Ruel 新增：委派链的深度闸门，挂在 service 层。
//
// 上游没有这道闸门。委派只在子 Run 上拷一份父 id（`delegated_from_task_id`），没有任何
// 计数、深度或时间窗会累积。实测两个 Agent 各自在 Run 未结束时交替派单，20 轮下来一条
// Issue 上有 21 个 Run，而且没有成本预算兜底——它会一直跑到有人发现为止。
//
// 上游唯一声称能防住这事的是 `HasPendingTaskForIssueAndAgentInThread` 的去重，而它按
// (issue, agent, thread) 记：交替循环每轮换人、且上一轮已经终态，一次都命中不了。
//
// ## 为什么挂在这里，而不是挂在 handler 的评论收口上
//
// M4 的第一版闸门挂在 `handler.resolveCommentTriggerEnqueue`，只挡「评论触发新起一个
// Run」。但会写出 `delegated_from_task_id` 的入口有六条，评论只是其中一条——最要紧的那条
// 「Agent 自己建 Issue」（`origin_type='agent_create'`）根本不过评论。清单见
// ruel/docs/reference/multica-delegation-entrypoints.md。
//
// 这六条入口**唯一共享的东西**是「一个新 Run 带着一个父 Run 出生」。所以判定从
// triggerCommentID 改成父 Run 本身，位置下沉到真正落库的地方。在每条入口各挂一次是错的：
// 漏掉的那处正是循环会走的那处，因为循环不挑路径。
//
// 为什么不直接数「这条 Issue 上跑了多少个 Run」：那会把正常的长任务流一起砍掉——一个
// Issue 被反复补充评论、重跑十几次是合理的。要拦的是**委派**这件事自己转起来，所以判据
// 是链的深浅，不是数目的多少。

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
)

// ErrDelegationDepthExceeded 是闸门拒绝入队时返回的哨兵错误。
//
// 调用方要用它给出 reason code（handler 层映射到
// dispatch.ReasonDelegationDepthExceeded），不能当成普通错误吞掉——静默丢弃是最糟的一种：
// 派单的 Agent 以为自己派出去了，实际没人接，界面上什么都不说。
var ErrDelegationDepthExceeded = errors.New("delegation chain reached its depth limit")

// RuelDelegationDepthLimit 是一条委派链上最多允许多少次转手。
//
// 取值偏松，是刻意的：这道闸门要挡的是「转不起来又停不下来」的循环，不是正常的深链。
// 一个 leader 派给 worker、worker 回报、leader 再派给另一个 worker 的合理流程，十来次
// 转手已经很深了；真到 12 次还在互相派，几乎不可能是人想要的。
//
// 导出是给验收测试用的：闸门在 service 层，而触发它的用例写在 handler 包里，两边都要引用
// 同一个数——各写一份等于没有一致的上限。
const RuelDelegationDepthLimit = 12

// ruelDelegationDepthOf 返回走到 taskID 为止这条链转了多少次手。链根是 0，它的子 Run
// 是 1，依次 +1。
//
// 沿 delegated_from_task_id 往上走，走一步数一步；走到链根或读不到就停。**读不到一律当作
// 链到此为止**——读不出来不能变成拒绝，那会把一次普通的查询抖动变成派单失败。
//
// 走到 limit 就提前返回：再往上走只会更大，而调用方只关心「有没有到 limit」。附带的好处是
// 循环是**有界**的——链上出现环（比如失败恢复把父指回自己）时最多走 limit 步，不会挂住。
func (s *TaskService) ruelDelegationDepthOf(ctx context.Context, taskID pgtype.UUID) int {
	depth := 0
	current := taskID
	for current.Valid {
		task, err := s.Queries.GetAgentTask(ctx, current)
		if err != nil || !task.DelegatedFromTaskID.Valid {
			return depth
		}
		depth++
		if depth >= RuelDelegationDepthLimit {
			return depth
		}
		current = task.DelegatedFromTaskID
	}
	return depth
}

// ruelGuardDelegationDepth 判定「让这个新 Run 认 parentTaskID 当爹」会不会把链拉过上限，
// 会就拒绝。 `parentTaskID` 无效（不是委派来的）直接放行——闸门只对 Agent 之间的转手生效。
//
// path 只是日志里的一个标签，用来区分是从哪条入口被拒的：排查一条已经跑歪的链时，要看的
// 是它从哪条路转进来的。
func (s *TaskService) ruelGuardDelegationDepth(ctx context.Context, parentTaskID, issueID, agentID pgtype.UUID, path string) error {
	if !parentTaskID.Valid {
		return nil
	}
	depth := s.ruelDelegationDepthOf(ctx, parentTaskID)
	if depth < RuelDelegationDepthLimit {
		return nil
	}
	slog.WarnContext(ctx, "ruel: 委派链到了深度上限，拒绝再转一次手",
		"path", path,
		"issue_id", util.UUIDToString(issueID),
		"agent_id", util.UUIDToString(agentID),
		"parent_task_id", util.UUIDToString(parentTaskID),
		"depth", depth,
		"limit", RuelDelegationDepthLimit,
	)
	return ErrDelegationDepthExceeded
}
