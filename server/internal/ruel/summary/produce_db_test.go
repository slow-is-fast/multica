package summary

// #50 的**落库**那一半。summary_test.go 证明的是「块解析得对」，这里证明的是「条目
// 真的写进了库、五个来源字段齐全、状态是待审」——中间隔着 Produce 到 knowledge.Store
// 的一次真实 INSERT，而恰恰这一层原先一个测试都没有：把「写条目」摘掉，全包依旧全绿，
// 判据要求的突变校验根本无从谈起。本文件就是那条会红的用例。
//
// 连真实 Postgres（DATABASE_URL，默认与仓库其它落库套件一致），不可达时 SKIP 而不是
// 失败，保证无数据库的 CI 看到的是跳过。每个用例自建 workspace / user / agent /
// runtime / issue / task / comment，并在 cleanup 里删干净。

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("could not connect to %s: %v", dbURL, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database not reachable at %s: %v", dbURL, err)
	}
	return pool
}

type produceFixture struct {
	pool        *pgxpool.Pool
	workspaceID string
	userID      string
	agentID     string
	runtimeID   string
	issueID     string
	taskID      string
}

func seedProduceFixture(t *testing.T, pool *pgxpool.Pool) *produceFixture {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	f := &produceFixture{pool: pool}

	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ('Summary User', $1) RETURNING id`,
		fmt.Sprintf("summary-%d@multica.test", suffix)).Scan(&f.userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, f.userID) })

	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug) VALUES ('summary ws', $1) RETURNING id`,
		fmt.Sprintf("summary-%d", suffix)).Scan(&f.workspaceID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, f.workspaceID) })

	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`,
		f.workspaceID, f.userID); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
		VALUES ($1, 'summary-runtime', 'cloud', 'codex', 'online', '', '{}'::jsonb, $2)
		RETURNING id`, f.workspaceID, f.userID).Scan(&f.runtimeID); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args)
		VALUES ($1, 'summary-agent', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3, '', '{}'::jsonb, '[]'::jsonb)
		RETURNING id`, f.workspaceID, f.runtimeID, f.userID).Scan(&f.agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, creator_type, creator_id, assignee_type, assignee_id, priority)
		VALUES ($1, 'summary issue', 'member', $2, 'agent', $3, 'medium')
		RETURNING id`, f.workspaceID, f.userID, f.agentID).Scan(&f.issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority,
			originator_user_id, accountable_user_id, originator_source)
		VALUES ($1, $2, $3, 'completed', 0, $4, $4, 'direct_human')
		RETURNING id`, f.agentID, f.runtimeID, f.issueID, f.userID).Scan(&f.taskID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, f.taskID) })
	return f
}

// resultComment 插一条属于本 Run 的结果评论：source_task_id 盖上本 task，这是 Produce
// 定位 Run 输出所用的唯一锚点（#46 §3.4：不是 delivered_comment_ids）。
//
// author_type 用 member 只是为了绕开作者列的外键形状；Produce 按 source_task_id 与
// issue_id 取评论，不看作者，所以这里不影响被测路径。
func (f *produceFixture) resultComment(t *testing.T, content string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, source_task_id, created_at, updated_at)
		VALUES ($1, $2, 'member', $3, $4, 'comment', $5, now(), now())
		RETURNING id`, f.issueID, f.workspaceID, f.userID, content, f.taskID).Scan(&id); err != nil {
		t.Fatalf("seed result comment: %v", err)
	}
	t.Cleanup(func() { f.pool.Exec(context.Background(), `DELETE FROM comment WHERE id = $1`, id) })
	return id
}

// entries 返回本 Run 已落库的知识条目（statement, rationale, status, workspace, 来源锚点）。
func (f *produceFixture) entries(t *testing.T) []produceEntry {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT statement, rationale, status, workspace_id, source_issue_id, source_comment_id
		FROM ruel_project_knowledge WHERE source_task_id = $1`, f.taskID)
	if err != nil {
		t.Fatalf("query knowledge entries: %v", err)
	}
	defer rows.Close()
	var out []produceEntry
	for rows.Next() {
		var e produceEntry
		if err := rows.Scan(&e.statement, &e.rationale, &e.status, &e.workspaceID, &e.sourceIssueID, &e.sourceCommentID); err != nil {
			t.Fatalf("scan knowledge entry: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate knowledge entries: %v", err)
	}
	return out
}

type produceEntry struct {
	statement       string
	rationale       string
	status          string
	workspaceID     pgtype.UUID
	sourceIssueID   pgtype.UUID
	sourceCommentID pgtype.UUID
}

func (f *produceFixture) produce(t *testing.T) {
	t.Helper()
	task := &db.AgentTaskQueue{
		ID:      util.MustParseUUID(f.taskID),
		IssueID: util.MustParseUUID(f.issueID),
	}
	if err := Produce(context.Background(), f.pool, task); err != nil {
		t.Fatalf("Produce: %v", err)
	}
	t.Cleanup(func() {
		f.pool.Exec(context.Background(), `DELETE FROM ruel_project_knowledge WHERE source_task_id = $1`, f.taskID)
	})
}

// TestProducePersistsPendingEntryWithAllSourceFields 钉住 #50 的落库与五字段齐全。
//
// 为什么必须单开：summary_test.go 全是纯解析用例，Produce 的 INSERT 一行都没被覆盖。
// 摘掉 store.Create 之后解析用例照样绿，判据里的「突变校验」形同虚设。这条用例在真实
// 库上断言「一条待审条目、五个来源字段都实」，摘掉写入即红。
func TestProducePersistsPendingEntryWithAllSourceFields(t *testing.T) {
	pool := newTestPool(t)
	f := seedProduceFixture(t, pool)

	commentID := f.resultComment(t, block(`{"version":1,"decisions":[{"what":"cache uses Redis","why":"local cache diverges","refs":["c-123"]}],"open_items":[]}`))
	f.produce(t)

	got := f.entries(t)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 pending entry, got %d", len(got))
	}
	e := got[0]
	if e.status != "pending" {
		t.Errorf("status = %q, want pending", e.status)
	}
	if e.statement == "" {
		t.Error("statement is empty")
	}
	if e.rationale == "" {
		t.Error("rationale is empty")
	}
	if !e.workspaceID.Valid || util.UUIDToString(e.workspaceID) != f.workspaceID {
		t.Errorf("workspace_id = %v, want %s", e.workspaceID, f.workspaceID)
	}
	if !e.sourceIssueID.Valid || util.UUIDToString(e.sourceIssueID) != f.issueID {
		t.Errorf("source_issue_id = %v, want %s", e.sourceIssueID, f.issueID)
	}
	if !e.sourceCommentID.Valid || util.UUIDToString(e.sourceCommentID) != commentID {
		t.Errorf("source_comment_id = %v, want %s", e.sourceCommentID, commentID)
	}
}

// TestProduceKeepsOpenItemsOutOfTheKnowledgeBase 钉住设计约束：open_items 是待办不是
// 知识，不进知识库。块里有一条决定 + 一个未完成项时，库里只能多出一条条目。
func TestProduceKeepsOpenItemsOutOfTheKnowledgeBase(t *testing.T) {
	pool := newTestPool(t)
	f := seedProduceFixture(t, pool)

	f.resultComment(t, block(`{"version":1,"decisions":[{"what":"decision one","why":"because","refs":[]}],"open_items":[{"what":"rollout switch not wired","why_left":"waiting for ops"}]}`))
	f.produce(t)

	if got := f.entries(t); len(got) != 1 {
		t.Fatalf("open_items must not become knowledge entries: got %d entries, want 1", len(got))
	}
}
