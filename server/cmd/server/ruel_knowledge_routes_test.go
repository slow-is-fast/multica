package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ---- #48 审批链路的路由级用例 ----
//
// 与 internal/handler/ruel_knowledge_test.go 不重复：那边直接调 handler，审阅人是
// knowledgeReviewer 的**回落查库**那一支；这里过完整张路由表，走的是中间件注入的
// ctxMember——生产路径。两支都要有自己的用例，否则「零查询那一支」从没被跑过。

// seedPendingKnowledge 直接写一条待审候选。
//
// 到 #48 为止还没有产出候选的那一环（它属于压缩层），所以夹具只能直接插表——
// 这不是绕开被测代码：本 Issue 的被测对象是「看到候选之后怎么处理它」。
func seedPendingKnowledge(t *testing.T, statement string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO ruel_project_knowledge (workspace_id, statement, rationale)
		VALUES ($1, $2, $3) RETURNING id`,
		testWorkspaceID, statement, "路由级用例造的候选").Scan(&id); err != nil {
		t.Fatalf("造待审候选: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(),
			`DELETE FROM ruel_project_knowledge WHERE id = $1`, id); err != nil {
			t.Errorf("清理条目 %s: %v", id, err)
		}
	})
	return id
}

// TestRuelKnowledgeRoutes_DenyAnonymous 守三条路由都挂在有守卫的组里。
//
// 401 与 404 在这一步是两种完全不同的结论：401 说明路由存在且被拦住，404 说明
// 路由压根没挂上（或者被挂在了一个没有鉴权的组里）。写成「不等于 404」会把后者
// 也放过去，所以这里钉死 401。
func TestRuelKnowledgeRoutes_DenyAnonymous(t *testing.T) {
	entryID := "00000000-0000-0000-0000-0000000000ff"
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/ruel/knowledge/pending"},
		{http.MethodPost, "/api/ruel/knowledge/" + entryID + "/approve"},
		{http.MethodPost, "/api/ruel/knowledge/" + entryID + "/reject"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, testServer.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("构造请求: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("状态 %d，应当是 401（404 说明路由没挂上）", resp.StatusCode)
			}
		})
	}
}

// TestRuelKnowledgeApprovalEndToEnd 走一遍 #48 的验收判据：
// 造一条待审候选 → 界面批准 → 库里状态变为已批准且记了批准人。
//
// 「界面批准」在这里就是界面会打的那个请求：POST /api/ruel/knowledge/{id}/approve，
// 带登录会话，不带任何声称「我是谁」的字段——审阅人必须由会话推出来。
func TestRuelKnowledgeApprovalEndToEnd(t *testing.T) {
	statement := fmt.Sprintf("端到端候选-%d", time.Now().UnixNano())
	entryID := seedPendingKnowledge(t, statement)

	type knowledgeItem struct {
		ID             string  `json:"id"`
		Statement      string  `json:"statement"`
		Status         string  `json:"status"`
		ReviewNote     string  `json:"review_note"`
		ReviewedByType *string `json:"reviewed_by_type"`
		ReviewedByID   *string `json:"reviewed_by_id"`
		ReviewedAt     *string `json:"reviewed_at"`
	}

	// ① 待审队列里有它。
	resp := authRequest(t, http.MethodGet, "/api/ruel/knowledge/pending", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("读待审队列: 状态 %d", resp.StatusCode)
	}
	var queue []knowledgeItem
	readJSON(t, resp, &queue)
	found := false
	for _, it := range queue {
		if it.ID == entryID {
			found = true
			if it.Statement != statement {
				t.Errorf("队列里的 statement = %q，应当是 %q", it.Statement, statement)
			}
			if it.Status != "pending" {
				t.Errorf("队列里的 status = %q", it.Status)
			}
		}
	}
	if !found {
		t.Fatalf("刚造的候选没出现在待审队列里（队列共 %d 条）", len(queue))
	}

	// ② 批准。
	resp = authRequest(t, http.MethodPost, "/api/ruel/knowledge/"+entryID+"/approve",
		map[string]string{"note": "端到端批准"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("批准: 状态 %d", resp.StatusCode)
	}
	var decided knowledgeItem
	readJSON(t, resp, &decided)
	if decided.Status != "approved" {
		t.Errorf("响应 status = %q，应当是 approved", decided.Status)
	}
	// 这条断言是本用例与 handler 用例的分界：这里走的是中间件注入的成员，
	// 走的不是回落查库那一支。
	if decided.ReviewedByType == nil || *decided.ReviewedByType != "member" {
		t.Errorf("响应 reviewed_by_type = %v", decided.ReviewedByType)
	}
	if decided.ReviewedByID == nil || *decided.ReviewedByID != testUserID {
		t.Errorf("响应 reviewed_by_id = %v，应当是会话对应的用户 %s", decided.ReviewedByID, testUserID)
	}
	if decided.ReviewedAt == nil || *decided.ReviewedAt == "" {
		t.Error("响应缺 reviewed_at")
	}
	if decided.ReviewNote != "端到端批准" {
		t.Errorf("响应 review_note = %q", decided.ReviewNote)
	}

	// ③ 库里确实变了，且批准人是会话那个用户（不是回落路径碰巧查对的一个）。
	var status string
	var reviewedByType pgtype.Text
	var reviewedByID pgtype.UUID
	var reviewedAt pgtype.Timestamptz
	if err := testPool.QueryRow(context.Background(), `
		SELECT status, reviewed_by_type, reviewed_by_id, reviewed_at
		  FROM ruel_project_knowledge WHERE id = $1`, entryID,
	).Scan(&status, &reviewedByType, &reviewedByID, &reviewedAt); err != nil {
		t.Fatalf("读回条目: %v", err)
	}
	if status != "approved" || !reviewedByID.Valid || !reviewedAt.Valid {
		t.Errorf("库里 status=%q reviewed_by_id=%v reviewed_at=%v", status, reviewedByID, reviewedAt)
	}
	if got := uuidString(reviewedByID); got != testUserID {
		t.Errorf("库里 reviewed_by_id = %s，应当是 %s", got, testUserID)
	}

	// ④ 它已经从待审队列里消失——批完的东西不该还占着人的视线。
	resp = authRequest(t, http.MethodGet, "/api/ruel/knowledge/pending", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("再读待审队列: 状态 %d", resp.StatusCode)
	}
	var after []knowledgeItem
	readJSON(t, resp, &after)
	for _, it := range after {
		if it.ID == entryID {
			t.Fatalf("已批准的条目 %s 还在待审队列里", entryID)
		}
	}

	// ⑤ 重复点击 → 409，且第一次的批准痕迹不被覆盖。
	resp = authRequest(t, http.MethodPost, "/api/ruel/knowledge/"+entryID+"/reject",
		map[string]string{"note": "反悔"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("已批准后再拒绝: 状态 %d，应当是 409", resp.StatusCode)
	}
	resp.Body.Close()
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM ruel_project_knowledge WHERE id = $1`, entryID).Scan(&status); err != nil {
		t.Fatalf("再读状态: %v", err)
	}
	if status != "approved" {
		t.Fatalf("第一次的批准被覆盖了：status = %q", status)
	}

	// ⑥ 无理由的拒绝 → 400，且不改动任何东西。
	secondID := seedPendingKnowledge(t, statement+"-第二个")
	resp = authRequest(t, http.MethodPost, "/api/ruel/knowledge/"+secondID+"/reject",
		map[string]string{"note": "   "})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("无理由拒绝: 状态 %d，应当是 400", resp.StatusCode)
	}
	resp.Body.Close()
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM ruel_project_knowledge WHERE id = $1`, secondID).Scan(&status); err != nil {
		t.Fatalf("读第二个条目: %v", err)
	}
	if status != "pending" {
		t.Fatalf("被 400 拒掉的请求改了库：status = %q", status)
	}

	// ⑦ 畸形 id 在路由这层就能到 handler 的入参校验（而不是被当成路由不匹配）。
	resp = authRequest(t, http.MethodPost, "/api/ruel/knowledge/not-a-uuid/approve", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("畸形 id: 状态 %d，应当是 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// uuidString 把 pgtype.UUID 转成规范字符串。
func uuidString(u pgtype.UUID) string {
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
