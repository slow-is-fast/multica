package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/ruel/knowledge"
)

// ---- #48 审批链路的 HTTP 层用例 ----
//
// 全部走真库、真 handler（不经路由）。路由上的 RequireHumanActor 与成员中间件由
// cmd/server 那条路由级用例守；这里守的是行为：队列读什么、批准/拒绝写什么、
// 「没生效」的时候回什么。

// seedKnowledge 造一条待审候选。用例结束即删，不给后面的用例留脏数据。
//
// statement 带唯一后缀：待审队列是全 workspace 可见的，断言不能依赖「库里只有我
// 造的这几条」——那会把用例的正确性押在别处没有残留上。
func seedKnowledge(t *testing.T, workspaceID pgtype.UUID, statement string) knowledge.Entry {
	t.Helper()
	entry, err := knowledge.NewStore(testPool).Create(context.Background(), knowledge.Entry{
		WorkspaceID: workspaceID,
		Statement:   statement,
		Rationale:   "用例造的候选",
	})
	if err != nil {
		t.Fatalf("造待审候选 %q: %v", statement, err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(),
			`DELETE FROM ruel_project_knowledge WHERE id = $1`, entry.ID); err != nil {
			t.Errorf("清理条目 %v: %v", entry.ID, err)
		}
	})
	return entry
}

// uniqueStatement 让每条候选的 statement 只属于一个用例。
func uniqueStatement(t *testing.T, label string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", label, time.Now().UnixNano())
}

// newForeignWorkspace 造一个本用例专用、且 test 用户**不是成员**的 workspace。
//
// 「不是成员」正是它存在的理由：跨 workspace 的用例要的是一条真实存在的条目，
// 而调用方无权访问它——如果测试用户也是那个 workspace 的成员，跨边界的用例就
// 变成了一个纯权限用例，验不出「按 workspace 过滤」这件事。
func newForeignWorkspace(t *testing.T) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := testPool.QueryRow(context.Background(),
		`INSERT INTO workspace (name, slug) VALUES ($1, $2) RETURNING id`,
		t.Name(), fmt.Sprintf("ruelkh-%d", time.Now().UnixNano())).Scan(&id)
	if err != nil {
		t.Fatalf("建旁路 workspace: %v", err)
	}
	t.Cleanup(func() {
		// 条目在 workspace_id 上有 ON DELETE CASCADE，删 workspace 就跟着走。
		if _, err := testPool.Exec(context.Background(),
			`DELETE FROM workspace WHERE id = $1`, id); err != nil {
			t.Errorf("清理旁路 workspace: %v", err)
		}
	})
	return id
}

// knowledgeRowState 是直接读库看到的审阅痕迹。断言读库而不是读响应：响应可能是
// 拼出来的，库里那一行才是留痕本身。
type knowledgeRowState struct {
	Status         string
	ReviewNote     string
	ReviewedByType pgtype.Text
	ReviewedByID   pgtype.UUID
	ReviewedAt     pgtype.Timestamptz
}

func loadKnowledgeRow(t *testing.T, id pgtype.UUID) knowledgeRowState {
	t.Helper()
	var st knowledgeRowState
	if err := testPool.QueryRow(context.Background(), `
		SELECT status, review_note, reviewed_by_type, reviewed_by_id, reviewed_at
		  FROM ruel_project_knowledge WHERE id = $1`, id,
	).Scan(&st.Status, &st.ReviewNote, &st.ReviewedByType, &st.ReviewedByID, &st.ReviewedAt); err != nil {
		t.Fatalf("读回条目 %v: %v", id, err)
	}
	return st
}

// callKnowledge 打一次审批端点的 handler，返回响应码与解码后的条目。
func callKnowledge(t *testing.T, method, route string, urlParams map[string]string, body any) (int, RuelKnowledgeResponse, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	r := newRequest(method, route, body)
	for k, v := range urlParams {
		r = withURLParam(r, k, v)
	}
	dispatchKnowledge(t, r, w, route)
	var entry RuelKnowledgeResponse
	_ = json.Unmarshal(w.Body.Bytes(), &entry)
	return w.Code, entry, w.Body.Bytes()
}

// dispatchKnowledge 把请求交给对应的 handler（按用例给的路径精确分发）。
//
// 分开成一个小函数而不是写成表驱动：三条路由的处理函数签名相同但入口不同，
// 表驱动会把「哪个路径进哪个 handler」也变成被测对象，而那不是本文件要验的。
func dispatchKnowledge(t *testing.T, r *http.Request, w *httptest.ResponseRecorder, route string) {
	t.Helper()
	switch route {
	case "/pending":
		testHandler.ListRuelPendingKnowledge(w, r)
	case "/approve":
		testHandler.ApproveRuelKnowledge(w, r)
	case "/reject":
		testHandler.RejectRuelKnowledge(w, r)
	default:
		t.Fatalf("用例给了一个没人分发的路径 %q", route)
	}
}

// errorCodeOf 取出错误响应里的机器可读码。界面靠它翻译，而不是把英文句子直接
// 抛给中文用户——所以它是要被守的契约，不是实现细节。
func errorCodeOf(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("解码错误响应: %v; body=%s", err, body)
	}
	return payload.Code
}

func listPendingKnowledge(t *testing.T) []RuelKnowledgeResponse {
	t.Helper()
	w := httptest.NewRecorder()
	r := newRequest(http.MethodGet, "/pending", nil)
	testHandler.ListRuelPendingKnowledge(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("读待审队列: 状态 %d: %s", w.Code, w.Body.String())
	}
	var items []RuelKnowledgeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatalf("解码待审队列: %v; body=%s", err, w.Body.String())
	}
	return items
}

// TestKnowledgeResponses_NeverSerializeAsNull 守空队列是 [] 而不是 null。
//
// 不用建库：空队列的形状完全是 toKnowledgeResponses 的事，而 handler 就是调它。
// 这条要紧是因为界面上「待审 0 条」与「还没加载完」在 JSON 里不能长得一样——
// #48 明确要求没有待审项时也要显示，null 会让前端被迫猜。
func TestKnowledgeResponses_NeverSerializeAsNull(t *testing.T) {
	body, err := json.Marshal(toKnowledgeResponses(nil))
	if err != nil {
		t.Fatalf("序列化空队列: %v", err)
	}
	if string(body) != "[]" {
		t.Fatalf("空队列序列化成 %s，应当是 []", body)
	}
}

// TestListRuelPendingKnowledge_ShowsOnlyThisWorkspacesPending 守队列的三条边界：
// 只含本 workspace、只含待审、含有来源信息。
func TestListRuelPendingKnowledge_ShowsOnlyThisWorkspacesPending(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ws := parseUUID(testWorkspaceID)

	pendingA := seedKnowledge(t, ws, uniqueStatement(t, "待审A"))
	pendingB := seedKnowledge(t, ws, uniqueStatement(t, "待审B"))
	approved := seedKnowledge(t, ws, uniqueStatement(t, "已批准C"))
	if _, err := knowledge.NewStore(testPool).Review(
		context.Background(), approved.ID, ws, knowledge.StatusApproved, "可以", knowledge.ReviewerMember, parseUUID(testUserID),
	); err != nil {
		t.Fatalf("把 C 改成已批准: %v", err)
	}
	foreign := seedKnowledge(t, newForeignWorkspace(t), uniqueStatement(t, "别处的待审D"))

	// 把 A 的创建时间往回调一小时，制造确定的先后，不靠 sleep 拼时序。
	if _, err := testPool.Exec(context.Background(),
		`UPDATE ruel_project_knowledge SET created_at = created_at - interval '1 hour' WHERE id = $1`,
		pendingA.ID); err != nil {
		t.Fatalf("回拨创建时间: %v", err)
	}

	items := listPendingKnowledge(t)
	byID := map[string]RuelKnowledgeResponse{}
	for _, it := range items {
		byID[it.ID] = it
	}

	if _, ok := byID[uuidToString(pendingA.ID)]; !ok {
		t.Error("待审 A 没出现在队列里")
	}
	if _, ok := byID[uuidToString(pendingB.ID)]; !ok {
		t.Error("待审 B 没出现在队列里")
	}
	// 已批准的不该在待审队列里：队列只回答「还有什么等着人看」。
	if _, ok := byID[uuidToString(approved.ID)]; ok {
		t.Error("已批准的 C 出现在了待审队列里")
	}
	// 其他 workspace 的条目一条都不能混进来——workspace 是硬边界。
	if _, ok := byID[uuidToString(foreign.ID)]; ok {
		t.Error("别的 workspace 的条目混进了待审队列")
	}
	for _, it := range items {
		if it.Status != knowledge.StatusPending {
			t.Errorf("队列里出现了非待审条目 %s（status=%q）", it.ID, it.Status)
		}
	}

	// 最新在前。这是界面上的顺序：刚冒出来的候选在最上面，人才不用往下翻。
	if len(items) > 1 {
		for i := 1; i < len(items); i++ {
			if items[i-1].CreatedAt < items[i].CreatedAt {
				t.Fatalf("队列顺序不是最新在前：%q 排在了 %q 前面", items[i-1].CreatedAt, items[i].CreatedAt)
			}
		}
	}

	// 溯源要带出来，否则人无法判断这条候选是从哪一轮对话里总结出来的。
	got := byID[uuidToString(pendingB.ID)]
	if got.Statement != pendingB.Statement {
		t.Errorf("statement = %q，应当是 %q", got.Statement, pendingB.Statement)
	}
	if got.ReviewedAt != nil || got.ReviewedByType != nil || got.ReviewedByID != nil {
		t.Errorf("待审条目的 reviewed_* 应当是 null，得到 %v / %v / %v",
			got.ReviewedAt, got.ReviewedByType, got.ReviewedByID)
	}
	if got.ReviewNote != "" {
		t.Errorf("待审条目的 review_note 应当是空串，得到 %q", got.ReviewNote)
	}
}

// TestApproveRuelKnowledge_RecordsReviewerAndTime 是 #48 验收判据的核心：
// 「库里状态变为已批准且记了批准人」。
func TestApproveRuelKnowledge_RecordsReviewerAndTime(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	entry := seedKnowledge(t, parseUUID(testWorkspaceID), uniqueStatement(t, "待批准的结论"))

	code, resp, body := callKnowledge(t, http.MethodPost, "/approve",
		map[string]string{"id": uuidToString(entry.ID)},
		map[string]string{"note": "这条可以进库"})
	if code != http.StatusOK {
		t.Fatalf("批准: 状态 %d: %s", code, body)
	}
	if resp.Status != knowledge.StatusApproved {
		t.Errorf("响应 status = %q，应当是 %q", resp.Status, knowledge.StatusApproved)
	}
	// 审阅人取登录身份，不是请求体里给的——请求体里根本没有这个字段。
	if resp.ReviewedByType == nil || *resp.ReviewedByType != knowledge.ReviewerMember {
		t.Errorf("响应 reviewed_by_type = %v，应当是 %q", resp.ReviewedByType, knowledge.ReviewerMember)
	}
	if resp.ReviewedByID == nil || *resp.ReviewedByID != testUserID {
		t.Errorf("响应 reviewed_by_id = %v，应当是当前登录用户 %s", resp.ReviewedByID, testUserID)
	}
	if resp.ReviewedAt == nil || *resp.ReviewedAt == "" {
		t.Error("响应缺 reviewed_at——记时间与记人一样是「留痕」的一半")
	}

	row := loadKnowledgeRow(t, entry.ID)
	if row.Status != knowledge.StatusApproved {
		t.Errorf("库里 status = %q，应当是 %q", row.Status, knowledge.StatusApproved)
	}
	if !row.ReviewedAt.Valid {
		t.Error("库里 reviewed_at 是 NULL")
	}
	if !row.ReviewedByID.Valid || uuidToString(row.ReviewedByID) != testUserID {
		t.Errorf("库里 reviewed_by_id = %v，应当是 %s", row.ReviewedByID, testUserID)
	}
	if !row.ReviewedByType.Valid || row.ReviewedByType.String != knowledge.ReviewerMember {
		t.Errorf("库里 reviewed_by_type = %v，应当是 %q", row.ReviewedByType, knowledge.ReviewerMember)
	}
	if row.ReviewNote != "这条可以进库" {
		t.Errorf("库里 review_note = %q", row.ReviewNote)
	}
}

// TestApproveRuelKnowledge_AcceptsEmptyBody 守「批准可以不写理由，也可以连 body
// 都不发」。把空 body 判成 400 会把「点一下批准」变成一个看不懂的报错。
func TestApproveRuelKnowledge_AcceptsEmptyBody(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	entry := seedKnowledge(t, parseUUID(testWorkspaceID), uniqueStatement(t, "不带理由批准"))

	code, _, body := callKnowledge(t, http.MethodPost, "/approve",
		map[string]string{"id": uuidToString(entry.ID)}, nil)
	if code != http.StatusOK {
		t.Fatalf("空 body 批准: 状态 %d: %s", code, body)
	}
	row := loadKnowledgeRow(t, entry.ID)
	if row.Status != knowledge.StatusApproved {
		t.Errorf("库里 status = %q", row.Status)
	}
	if row.ReviewNote != "" {
		t.Errorf("库里 review_note = %q，应当是空串", row.ReviewNote)
	}
}

// TestRejectRuelKnowledge_RequiresReason 守「拒绝要留痕」的可执行部分。
//
// 关键不只是回了 400，而是**什么都没写**：一条被拒但没写理由的记录，下一个人看到
// 的只有「被拒了」，还得从头判断一遍——那正是这条要求要避免的。
func TestRejectRuelKnowledge_RequiresReason(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	entry := seedKnowledge(t, parseUUID(testWorkspaceID), uniqueStatement(t, "无理由拒绝"))
	id := map[string]string{"id": uuidToString(entry.ID)}

	for _, tc := range []struct {
		name string
		body any
	}{
		{"空 body", nil},
		{"空理由", map[string]string{"note": ""}},
		{"只有空白", map[string]string{"note": "   \t\n "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, body := callKnowledge(t, http.MethodPost, "/reject", id, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("状态 %d，应当是 400: %s", code, body)
			}
			if got := errorCodeOf(t, body); got != "knowledge_reject_note_required" {
				t.Errorf("错误码 = %q，界面靠它翻译成中文提示", got)
			}
			row := loadKnowledgeRow(t, entry.ID)
			if row.Status != knowledge.StatusPending {
				t.Errorf("库里 status = %q，被拒的请求不该改动任何东西", row.Status)
			}
			if row.ReviewedAt.Valid || row.ReviewedByID.Valid {
				t.Error("库里留下了审阅痕迹，可是这次请求被拒了")
			}
		})
	}
}

// TestRejectRuelKnowledge_RecordsReason 守拒绝也记人、记时间、记理由。
func TestRejectRuelKnowledge_RecordsReason(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	entry := seedKnowledge(t, parseUUID(testWorkspaceID), uniqueStatement(t, "带理由拒绝"))

	code, resp, body := callKnowledge(t, http.MethodPost, "/reject",
		map[string]string{"id": uuidToString(entry.ID)},
		map[string]string{"note": "  已被新方案替代  "})
	if code != http.StatusOK {
		t.Fatalf("拒绝: 状态 %d: %s", code, body)
	}
	if resp.Status != knowledge.StatusRejected {
		t.Errorf("响应 status = %q，应当是 %q", resp.Status, knowledge.StatusRejected)
	}

	row := loadKnowledgeRow(t, entry.ID)
	if row.Status != knowledge.StatusRejected {
		t.Errorf("库里 status = %q", row.Status)
	}
	// 去空白后落库：与 StatusPending 的处理一致，界面上的理由不该带用户手抖打出的空格。
	if row.ReviewNote != "已被新方案替代" {
		t.Errorf("库里 review_note = %q，应当去掉首尾空白", row.ReviewNote)
	}
	if !row.ReviewedAt.Valid || !row.ReviewedByID.Valid {
		t.Error("拒绝也必须有审阅人、有时间")
	}
	if uuidToString(row.ReviewedByID) != testUserID {
		t.Errorf("库里 reviewed_by_id = %v，应当是 %s", row.ReviewedByID, testUserID)
	}
}

// TestReviewRuelKnowledge_SecondDecisionConflicts 守「审过的不许再审」。
//
// 这是两个审批人同时看队列时的真实现场：先到的改状态，后到的必须拿到 409 而不是
// 悄悄覆盖前者——覆盖会让「谁批的」变成最后一次点击的人，而第一次批准已经生效过
// （比如已经注入过一轮 Run）。
func TestReviewRuelKnowledge_SecondDecisionConflicts(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	entry := seedKnowledge(t, parseUUID(testWorkspaceID), uniqueStatement(t, "被审两次"))
	id := map[string]string{"id": uuidToString(entry.ID)}

	if code, _, body := callKnowledge(t, http.MethodPost, "/approve", id,
		map[string]string{"note": "第一次批准"}); code != http.StatusOK {
		t.Fatalf("首次批准: 状态 %d: %s", code, body)
	}

	for _, tc := range []struct {
		name string
		path string
		body any
	}{
		{"再批准一次", "/approve", map[string]string{"note": "再来一次"}},
		{"批准后反悔拒绝", "/reject", map[string]string{"note": "反悔了"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, body := callKnowledge(t, http.MethodPost, tc.path, id, tc.body)
			if code != http.StatusConflict {
				t.Fatalf("状态 %d，应当是 409: %s", code, body)
			}
			if got := errorCodeOf(t, body); got != "knowledge_already_reviewed" {
				t.Errorf("错误码 = %q", got)
			}
			row := loadKnowledgeRow(t, entry.ID)
			if row.Status != knowledge.StatusApproved {
				t.Errorf("库里 status = %q，第一次批准不该被覆盖", row.Status)
			}
			if row.ReviewNote != "第一次批准" {
				t.Errorf("库里 review_note = %q，第一次的理由不该被覆盖", row.ReviewNote)
			}
		})
	}
}

// TestReviewRuelKnowledge_CrossWorkspaceIsNotFound 守跨 workspace 的审批做不到。
func TestReviewRuelKnowledge_CrossWorkspaceIsNotFound(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	foreignWS := newForeignWorkspace(t)
	target := seedKnowledge(t, foreignWS, uniqueStatement(t, "别人的待审"))

	// 两路都试：拿自己的 workspace 身份去审别人的条目（条目不存在于本 workspace），
	// 以及拿自己无权访问的 workspace 去审（成员检查先拦下）。
	for _, tc := range []struct {
		name        string
		workspaceID string
	}{
		{"用本 workspace 的身份", testWorkspaceID},
		{"用一个自己不是成员的 workspace", uuidToString(foreignWS)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := newRequest(http.MethodPost, "/approve", map[string]string{"note": "越权批准"})
			r.Header.Set("X-Workspace-ID", tc.workspaceID)
			r = withURLParam(r, "id", uuidToString(target.ID))
			testHandler.ApproveRuelKnowledge(w, r)

			if w.Code != http.StatusNotFound {
				t.Fatalf("状态 %d，应当是 404: %s", w.Code, w.Body.String())
			}
			row := loadKnowledgeRow(t, target.ID)
			if row.Status != knowledge.StatusPending {
				t.Fatalf("别人的条目被改了：status = %q", row.Status)
			}
		})
	}
}

// TestReviewRuelKnowledge_UnknownEntryIsNotFound 守不存在的 id 回 404 而不是 500。
//
// 域层把「不存在」与「属于别的 workspace」合并成一个错误（刻意不区分，否则这里
// 就成了一个探测别的项目有哪些条目的接口）；这条用例也顺带钉住那个合并——
// 两件事对外的响应必须一模一样。
func TestReviewRuelKnowledge_UnknownEntryIsNotFound(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	missing := uuidToString(pgtype.UUID{Bytes: [16]byte{0x9e}, Valid: true})

	code, _, body := callKnowledge(t, http.MethodPost, "/approve",
		map[string]string{"id": missing}, map[string]string{"note": "无中生有"})
	if code != http.StatusNotFound {
		t.Fatalf("状态 %d，应当是 404: %s", code, body)
	}
	if got := errorCodeOf(t, body); got != "knowledge_not_found" {
		t.Errorf("错误码 = %q", got)
	}
}

// TestReviewRuelKnowledge_InvalidIDIsBadRequest 守畸形 id 在碰到库之前就被挡住。
func TestReviewRuelKnowledge_InvalidIDIsBadRequest(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	code, _, body := callKnowledge(t, http.MethodPost, "/approve",
		map[string]string{"id": "not-a-uuid"}, map[string]string{"note": "x"})
	if code != http.StatusBadRequest {
		t.Fatalf("状态 %d，应当是 400: %s", code, body)
	}
}

// TestKnowledgeEndpoints_MissingWorkspaceIsBadRequest 守缺 workspace 标识时给 400。
//
// 不为 400 的话，resolveWorkspaceID 会回空串，接着 parseUUID("") 就是一次 panic
// 驱动的 500——一个客户端漏了 header 变成服务端故障。
func TestKnowledgeEndpoints_MissingWorkspaceIsBadRequest(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	entry := seedKnowledge(t, parseUUID(testWorkspaceID), uniqueStatement(t, "缺 workspace"))

	for _, tc := range []struct {
		name string
		path string
	}{
		{"队列", "/pending"},
		{"批准", "/approve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := newRequest(http.MethodPost, tc.path, map[string]string{"note": "x"})
			r.Header.Del("X-Workspace-ID")
			r = withURLParam(r, "id", uuidToString(entry.ID))
			dispatchKnowledge(t, r, w, tc.path)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("状态 %d，应当是 400: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestKnowledgeEndpoints_RejectMachineActors 守「知识必须经人批准」这条 PRD 硬约束
// 在 handler 层也成立。
//
// 路由上已经有一道 RequireHumanActor，这里验的是第二道：路由分组会被重构，而
// agent 自己批准自己提的候选是这条链路上唯一没有别的环节能拦住的越权。
func TestKnowledgeEndpoints_RejectMachineActors(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	entry := seedKnowledge(t, parseUUID(testWorkspaceID), uniqueStatement(t, "机器身份想批"))

	for _, path := range []string{"/pending", "/approve", "/reject"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := newRequest(http.MethodPost, path, map[string]string{"note": "我自己批"})
			// X-Actor-Source 是服务端盖章的，客户端伪造不了（Auth 中间件会先剥掉）；
			// 这里直接在 handler 上打，是在验「万一它真带着这个头到了 handler」。
			r.Header.Set("X-Actor-Source", "task_token")
			r = withURLParam(r, "id", uuidToString(entry.ID))
			dispatchKnowledge(t, r, w, path)

			if w.Code != http.StatusForbidden {
				t.Fatalf("状态 %d，应当是 403: %s", w.Code, w.Body.String())
			}
			row := loadKnowledgeRow(t, entry.ID)
			if row.Status != knowledge.StatusPending {
				t.Fatalf("机器身份改动了条目：status = %q", row.Status)
			}
		})
	}
}
