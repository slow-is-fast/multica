package handler

// Ruel 新增：项目知识候选的审批链路（#48）。
//
// 表与域包在 #47（migrations/565、internal/ruel/knowledge）。这一层只做三件事：
// 把待审队列读出来给界面、把批准/拒绝落到库、把「为什么没生效」翻译成界面能用的
// 响应码。
//
// ## 为什么批准与拒绝是两个路由，而不是一个 /review 带 decision 字段
//
// 域层是一个 Review(decision)——那边两条分支共用同一条 UPDATE，合成一个参数是对的。
// HTTP 层不必照抄：分成两个路由之后，「decision 传了个没见过的词」这一整类输入错误
// 就不存在了，而且两条路由的备注策略本来就不同（拒绝必须写理由，批准可以不写）。
// 用一个路由装两套规则，规则就藏在一个 if 里。
//
// ## 拒绝必须写理由，批准可以不写
//
// 这是 #48 正文「拒绝要留痕」的可执行形式。留痕 = 记人 + 记时间 + 记为什么；少了
// 「为什么」，下一个人（或下一个提案的 agent）只看到「被拒了」，还得从头判断一遍
// ——那正是这条要求要避免的。批准不给这条约束：批准本身已经把候选推进了库里，
// 理由的有无不改变任何人做判断。
//
// 理由落在 review_note 里，与 comment 表同名字段的先例一致（迁移 565 的一致性约束
// 也把 note 与 reviewed_* 绑在一起：非 pending 必须有审阅痕迹）。
//
// ## 审批人取当前登录成员，不接受请求体里给的
//
// 「谁批准的」是这次操作的事实，不是调用方能声明的东西。收下客户端给的 reviewer
// 等于把追责链交给被追责的一方——那正好把「经人批准」这条 PRD 要求架空了。
//
// ## 三个路由都是 human-only
//
// 读也拦。理由不是「队列敏感」，而是：机器身份的调用方在这条链路上**没有任何它能
// 做的动作**——批准与拒绝都是人的决定（PRD 第 5 章「初版避免自动全局记忆」）。一个
// 只能看、不能动的入口对 agent 没有用处，却让「谁在什么时候看了待审队列」变成一个
// 需要解释的日志噪音。注入路径（#49）读的是已批准条目，走服务端，不经过这里。

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/ruel/knowledge"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RuelKnowledgeReviewRequest 是批准/拒绝的请求体。
//
// 只有理由。决定由路由表达（见文件头），审阅人由登录身份决定，时间由服务端取——
// 这三样都不该是客户端的输入。
type RuelKnowledgeReviewRequest struct {
	Note string `json:"note"`
}

// RuelKnowledgeResponse 是一条知识条目的对外形状。
//
// reviewed_* 三项与三个 source 在「没有」时是 null 而不是空串：它们不是「值是空」，
// 而是「这件事没有发生 / 这个来源不存在」。用指针承载，界面才能把「没人审过」与
// 「审阅人名字为空」分开显示。
type RuelKnowledgeResponse struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
	Rationale string `json:"rationale"`
	Status    string `json:"status"`

	// Source* 是溯源，可以指向一个已经不存在的 Run / Issue / 评论——库里刻意没有
	// 对应外键（见迁移 565），知识的存在意义正是在 Run 消失之后还在。
	SourceTaskID    *string `json:"source_task_id"`
	SourceIssueID   *string `json:"source_issue_id"`
	SourceCommentID *string `json:"source_comment_id"`

	ReviewNote     string  `json:"review_note"`
	ReviewedByType *string `json:"reviewed_by_type"`
	ReviewedByID   *string `json:"reviewed_by_id"`
	ReviewedAt     *string `json:"reviewed_at"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toKnowledgeResponse(e knowledge.Entry) RuelKnowledgeResponse {
	return RuelKnowledgeResponse{
		ID:              uuidToString(e.ID),
		Statement:       e.Statement,
		Rationale:       e.Rationale,
		Status:          e.Status,
		SourceTaskID:    uuidToPtr(e.SourceTaskID),
		SourceIssueID:   uuidToPtr(e.SourceIssueID),
		SourceCommentID: uuidToPtr(e.SourceCommentID),
		ReviewNote:      e.ReviewNote,
		ReviewedByType:  textToPtr(e.ReviewedByType),
		ReviewedByID:    uuidToPtr(e.ReviewedByID),
		ReviewedAt:      timestampToPtr(e.ReviewedAt),
		CreatedAt:       timestampToString(e.CreatedAt),
		UpdatedAt:       timestampToString(e.UpdatedAt),
	}
}

func toKnowledgeResponses(in []knowledge.Entry) []RuelKnowledgeResponse {
	// make(..., 0, n) 而不是 make(..., n)：库里一条都没有时也必须序列化成 []，
	// 而不是 null。界面上的「待审 0 条」与「还没加载完」在 JSON 里长得一样的话，
	// 空队列就永远显示不出来——那正是 #40 那条纪律要防的静默。
	out := make([]RuelKnowledgeResponse, 0, len(in))
	for _, e := range in {
		out = append(out, toKnowledgeResponse(e))
	}
	return out
}

// knowledgeReviewer 解析本请求的 workspace 与「谁在操作」。
//
// 取人优先走中间件注入的 context（生产路径，零查询），中间件没覆盖时（测试直接调
// handler）回落到一次真正的成员查询。回落路径不是测试后门：它顺带证明了「调用方
// 确实是这个 workspace 的成员」，与生产路径上中间件做的是同一件事。
func (h *Handler) knowledgeReviewer(w http.ResponseWriter, r *http.Request) (pgtype.UUID, db.Member, bool) {
	// 与路由上的 RequireHumanActor 重复的一道 handler 级兜底。这次重复是有意的：
	// 「知识必须经人批准」是 PRD 的硬约束，不是账户权限的顺手一挡。路由分组会被
	// 重构——这个文件里的路由刚从 daemon 组挪到 member 组——一旦有人把这几条挪出
	// human-only 组，agent 就能批准自己提的候选，而这条链路上没有任何别的环节会
	// 拦住它。兜底顺带让这条约束可以被直接测到，不必起整张路由表。
	if isMachineCredentialActor(r) {
		writeError(w, http.StatusForbidden, "this endpoint is only available to human actors")
		return pgtype.UUID{}, db.Member{}, false
	}
	workspaceID := h.resolveWorkspaceID(r)
	// 缺标识与畸形标识都落在这里。不这么写的话，空串会一路走到 parseUUID——
	// 那个 helper 是 panic-on-invalid 的，一个客户端漏了 header 就变成服务端 500。
	workspaceUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return pgtype.UUID{}, db.Member{}, false
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return pgtype.UUID{}, db.Member{}, false
	}
	return workspaceUUID, member, true
}

// ListRuelPendingKnowledge 返回本 workspace 的待审队列，最新在前。
//
// 直接返回数组而不是包一层 {items: ...}，与相邻的 ListRuelIssueArtifacts 保持一致。
// 待审计数就是数组长度——队列不翻页（人不按页审批），所以没有需要单独报的计数。
func (h *Handler) ListRuelPendingKnowledge(w http.ResponseWriter, r *http.Request) {
	workspaceID, _, ok := h.knowledgeReviewer(w, r)
	if !ok {
		return
	}
	entries, err := knowledge.NewStore(h.DB).ListForWorkspace(r.Context(), workspaceID, knowledge.StatusPending)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list pending knowledge")
		return
	}
	writeJSON(w, http.StatusOK, toKnowledgeResponses(entries))
}

// ApproveRuelKnowledge 批准一条待审候选，理由可选。
func (h *Handler) ApproveRuelKnowledge(w http.ResponseWriter, r *http.Request) {
	h.reviewRuelKnowledge(w, r, knowledge.StatusApproved)
}

// RejectRuelKnowledge 拒绝一条待审候选，必须写明理由（见文件头）。
func (h *Handler) RejectRuelKnowledge(w http.ResponseWriter, r *http.Request) {
	h.reviewRuelKnowledge(w, r, knowledge.StatusRejected)
}

// reviewRuelKnowledge 是批准与拒绝的共同收口。
//
// 两个决定走同一段代码是有意的：它们在库层的差别只有 status 一个值，而审阅人、
// 时间、理由的记法必须完全一致——分开写早晚会有一支漏记 reviewed_at。
func (h *Handler) reviewRuelKnowledge(w http.ResponseWriter, r *http.Request, decision string) {
	workspaceID, member, ok := h.knowledgeReviewer(w, r)
	if !ok {
		return
	}
	entryID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "knowledge id")
	if !ok {
		return
	}

	var req RuelKnowledgeReviewRequest
	// 空 body 等同于「没写理由」：批准可以整个 body 都不发，拒绝会在下面被挡下。
	// 把 EOF 当成 400 只会让「点击批准」变成一个看不懂的报错。
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	note := strings.TrimSpace(req.Note)
	if decision == knowledge.StatusRejected && note == "" {
		writeErrorCode(w, http.StatusBadRequest, "knowledge_reject_note_required",
			"a rejection must carry a reason")
		return
	}

	entry, err := knowledge.NewStore(h.DB).Review(
		r.Context(), entryID, workspaceID, decision, note, knowledge.ReviewerMember, member.UserID)
	switch {
	case err == nil:
		// 回整条记录而不是 {"status":"ok"}：批准的产物就是这条改了状态的记录，
		// 界面拿它直接更新那一行，不需要再拉一次队列来确认「到底记了谁」。
		writeJSON(w, http.StatusOK, toKnowledgeResponse(entry))

	case errors.As(err, &knowledge.ErrNotPending{}):
		// 重复点击或两个审批人同时看队列的正常结果，不是服务端故障。
		// 409 而不是 500，界面才能说「这条已经被处理过了」并刷新队列。
		writeErrorCode(w, http.StatusConflict, "knowledge_already_reviewed", err.Error())

	case errors.Is(err, knowledge.ErrNotFound):
		// 域层把「不存在」与「属于别的 workspace」合并成同一句话、同一个错误
		// （见 explainMiss）。这里也合并：区分开就等于给了一个探测别的项目有
		// 哪些条目的接口。
		writeErrorCode(w, http.StatusNotFound, "knowledge_not_found", "knowledge entry not found")

	default:
		// 回 500 的同时必须留日志。一个只回「失败了」而服务端什么都不说的 500，
		// 是本项目反复踩过的那个坑：界面上只有一句泛化的报错，而真正的原因
		// （列不存在、约束不匹配、连接断了）没有任何地方留下痕迹。
		slog.ErrorContext(r.Context(), "ruel: 审批项目知识失败",
			"entry_id", uuidToString(entryID), "decision", decision,
			"workspace_id", uuidToString(workspaceID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to review knowledge")
	}
}
