// Package dispatch holds the canonical, cross-layer vocabulary for execution
// admission outcomes (MUL-4525). It is a leaf package (no internal deps) so both
// the service layer — which MAKES the admission/skip decision and therefore owns
// the reason at its source — and the handler layer — which serializes it to the
// wire — share one enum and can never drift.
//
// A ReasonCode is decided at the branch that blocks/skips a run and carried
// through to the response verbatim; it is never reverse-engineered from a
// human-readable failure string. Codes are stable, localizable by clients, and
// enumeration-safe: a code never reveals whether a private agent exists, its
// name, or its owner.
package dispatch

// ReasonCode is a stable, client-localizable admission/dispatch reason.
type ReasonCode string

const (
	// ReasonQueued / ReasonCoalesced / ReasonDeferred are the success-path codes.
	ReasonQueued    ReasonCode = "queued"
	ReasonCoalesced ReasonCode = "coalesced"
	ReasonDeferred  ReasonCode = "deferred"
	// ReasonSteered: the comment went into the target's running turn.
	ReasonSteered ReasonCode = "steered"

	// ReasonInvocationNotAllowed: the acting principal may not trigger this
	// target under the invocation-permission model. Deliberately generic — it
	// does not distinguish "target is private" from "target does not exist".
	ReasonInvocationNotAllowed ReasonCode = "invocation_not_allowed"
	// ReasonTargetUnavailable: the target cannot run (archived agent, deleted /
	// archived squad, unresolvable leader, or no assignee).
	ReasonTargetUnavailable ReasonCode = "target_unavailable"
	// ReasonRuntimeOffline: the target is permitted and bound to a runtime, but
	// that runtime is not online at dispatch time. The task is not lost — the
	// user's fix is to bring the machine back, and queued work waits for it.
	ReasonRuntimeOffline ReasonCode = "runtime_offline"
	// ReasonRuntimeUnusable: the target is bound to a runtime whose machine is
	// reachable, but whose agent CLI cannot be executed there — the npm
	// placeholder stub left behind when a package's postinstall was blocked is
	// the case in the field (MUL-6164). Distinct from runtime_offline for the
	// same reason agent_runtime_required is: waiting changes nothing here. The
	// machine is already on, and the fix is a command the user runs on it, which
	// the daemon reports with this verdict so clients can show it.
	ReasonRuntimeUnusable ReasonCode = "runtime_unusable"
	// ReasonRuntimeAccessDenied: the target is permitted, but its agent owner
	// cannot execute it on the private runtime selected for the task. This is
	// distinct from invocation_not_allowed: the caller may invoke the agent,
	// while the runtime/agent ownership binding still prevents execution.
	ReasonRuntimeAccessDenied ReasonCode = "runtime_access_denied"
	// ReasonRuntimeProfileMissing: the target is bound to a reachable runtime
	// whose agent CLI runs fine, but a runtime profile that CLI needs in order
	// to speak Multica's protocol is not installed on that machine — DeepSeek
	// Harness, whose `multica` profile supplies the `--stdio` protocol, is the
	// case in the field. Blocked for the same reason as runtime_unusable
	// (MUL-6164): the machine is already on and waiting changes nothing. Kept
	// APART from runtime_unusable because the repair is different in kind — the
	// CLI is not broken and reinstalling it fixes nothing, so copy that says
	// "reinstall the CLI" sends the user to the wrong place entirely.
	ReasonRuntimeProfileMissing ReasonCode = "runtime_profile_missing"
	// ReasonAgentRuntimeRequired: the target is permitted but bound to no
	// runtime at all (agent.runtime_id IS NULL), which is where an agent lands
	// when its runtime is deleted (MUL-5559). Distinct from runtime_offline on
	// purpose: there is no machine to bring back, nothing will ever claim work
	// for this agent, and the only fix is binding it to a runtime. Clients that
	// collapse the two send the user looking for an offline computer that does
	// not exist.
	ReasonAgentRuntimeRequired ReasonCode = "agent_runtime_required"
	// ReasonAttributionBlocked: a fail-closed workspace could not resolve a
	// responsible human for the run, so it was refused.
	ReasonAttributionBlocked ReasonCode = "attribution_blocked"
	// ReasonAlreadyActive: a run is already active/pending for this target and
	// this trigger did not coalesce.
	ReasonAlreadyActive ReasonCode = "already_active"
	// ReasonSelfTriggerSuppressed: the target was intentionally not (re-)triggered
	// because doing so would be a self-trigger the guard suppresses, and no active
	// run remains to cover it — e.g. a squad leader's own @mention of its squad
	// whose latest task is already terminal. Not a permission block, but NOT
	// success: nothing new runs. (Named to avoid implying the NEW comment was
	// already processed.)
	ReasonSelfTriggerSuppressed ReasonCode = "self_trigger_suppressed"
	// ReasonIssueInTriage: the issue is waiting in Triage, which has no executor
	// to act on (MUL-7189 §2.3). Returned when a trigger would have had to
	// derive one from the issue — running its assignee again, or asking it to
	// perform an action. An @mention is not refused: naming an agent by hand is
	// a conversation, and it dispatches normally.
	//
	// It is a property of the ISSUE, not of the target, so it is
	// enumeration-safe by construction: every target on the same issue gets the
	// same code, and the caller already sees the issue. Nothing is queued and
	// nothing is pending — the trigger is answered by accepting the issue out of
	// Triage, not by waiting.
	ReasonIssueInTriage ReasonCode = "issue_in_triage"
	// ReasonDelegationDepthExceeded: Ruel 新增。这条触发会让委派链再长一环，而链已经
	// 到了上限——拒绝入队。
	//
	// 上游没有委派深度闸门：委派只在子 Run 上拷一份父 id（`delegated_from_task_id`），
	// 没有任何计数、深度或时间窗会累积。实测两个 Agent 各自在 Run 未结束时交替派单，
	// 20 轮下来一条 Issue 上有 21 个 Run，且没有成本预算兜底——它会一直跑到有人发现。
	// 上游唯一声称能防住这事的是 `HasPendingTaskForIssueAndAgent` 去重，而它按
	// (issue, agent, thread) 记，交替循环每轮换人且上一轮已终态，一次都命中不了。
	//
	// 与 invocation_not_allowed 之类分开，是因为**修法不同**：权限问题要改授权，这里要
	// 做的是把链断开或让人接手。跟 already_active 也分开——那是一条 Run 正在跑，这里是
	// 一条链跑太久了。
	ReasonDelegationDepthExceeded ReasonCode = "delegation_depth_exceeded"
	// ReasonDelegationBudgetExceeded: Ruel 新增。这条 Issue 上累计的折算成本已经到预算，
	// 拒绝再转一次手（见 service/ruel_delegation_budget.go）。
	//
	// 与 delegation_depth_exceeded 分开，是因为**两件事各自独立，修法也不同**：
	//   - 深度是「转手次数」维度，封的是代数；换一条链就归零。
	//   - 预算是「钱」维度，封的是总量；per_issue 累计不会归零，撞上一次就是永久的，
	//     要人先解释那笔钱花在哪。
	//
	// 两者任一触限即停，但**不合并成一个「综合分」**——合并之后没人说得清到底是因为
	// 什么停的，也就没法决定该改哪个上限。
	//
	// 与 quota_exceeded 也分开：那个是 Cloud 的 autopilot 区间配额，这里是本机折算成本。
	ReasonDelegationBudgetExceeded ReasonCode = "delegation_budget_exceeded"
	// ReasonPeriodBudgetExceeded: Ruel 新增。某个周期性成本维度（per_agent /
	// per_workspace 的日或月）累计已到上限，拒绝这一单（见
	// service/ruel_period_budget.go）。
	//
	// 与 delegation_budget_exceeded 分开，是因为**出路不同**：那条是 per_issue 累计
	// 不归零，撞上一次就是永久的，要人先解释那笔钱花在哪；这一条按日历周期归零，等
	// 下一个周期自己就开了。混在一起，接收方会照着错的那个去处理——要么白等一个永远
	// 不会到来的「下个周期」，要么去解释一笔其实会自动恢复的账。
	//
	// 与 run_cost_budget_exceeded 也分开：那个是**取消正在跑的那一轮**，这里是**不让
	// 新的一轮开始**。
	ReasonPeriodBudgetExceeded ReasonCode = "periodic_budget_exceeded"
	// ReasonQuotaExceeded is a policy-neutral refusal for an exhausted
	// Cloud-provided autopilot interval.
	ReasonQuotaExceeded ReasonCode = "quota_exceeded"
	// ReasonIssueLimitReached means a create_issue Autopilot was admitted for a
	// run, but Cloud's effective workspace issue-count limit blocked the issue.
	ReasonIssueLimitReached ReasonCode = "issue_limit_reached"
	// ReasonInternalError: an unexpected server error prevented a clean decision.
	ReasonInternalError ReasonCode = "internal_error"
)
