package knowledge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- 纯校验：数据库传 nil，证明非法输入在碰到库之前就被拒绝 ----
//
// 这些用例刻意不建库连接。与 artifacts_test.go 同一个理由：校验若发生在写库之后，
// 那么一次拼错的状态会先落进表里再报错，库里留下一条界面上读不懂的记录。

func TestValidStatus(t *testing.T) {
	for _, status := range []string{StatusPending, StatusApproved, StatusRejected, StatusArchived} {
		if !ValidStatus(status) {
			t.Errorf("已知状态 %q 应当通过校验", status)
		}
	}
	// 大小写与拼写必须严格：状态是界面上那句话的索引，放行一个没见过的词，
	// 前端只能显示空白，缺陷就又被藏回去了。
	for _, status := range []string{"", " ", "approve", "APPROVED", "Approved", "deleted"} {
		if ValidStatus(status) {
			t.Errorf("未知状态 %q 不该通过校验", status)
		}
	}
	// 首尾空白被容忍，与兄弟包 artifacts.ValidStatus 同一处理（那里也是 TrimSpace）。
	// 容忍本身无害，前提是**写库与比较用的都是去掉空白后的值**——否则 status 列里
	// 会出现 "approved "，而所有按 'approved' 筛的查询会静默漏掉它。这条由
	// TestReviewStoresNormalizedStatus 在真库上守。
	if !ValidStatus(" pending ") {
		t.Error("首尾空白应当被容忍（与 artifacts.ValidStatus 一致）")
	}
}

func TestValidReviewerType(t *testing.T) {
	for _, rt := range []string{ReviewerMember, ReviewerAgent, ReviewerSystem, ReviewerPlugin} {
		if !ValidReviewerType(rt) {
			t.Errorf("已知审阅人类型 %q 应当通过校验", rt)
		}
	}
	for _, rt := range []string{"", "user", "Member", "human"} {
		if ValidReviewerType(rt) {
			t.Errorf("未知审阅人类型 %q 不该通过校验", rt)
		}
	}
}

func validUUID(seed byte) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{seed}, Valid: true}
}

// TestCreateRejectsInvalidInput 守 Create 的三条前置校验。
func TestCreateRejectsInvalidInput(t *testing.T) {
	store := NewStore(nil)
	ctx := context.Background()

	// workspace_id 是硬边界，不是可选的归属字段。
	if _, err := store.Create(ctx, Entry{Statement: "x"}); err == nil {
		t.Error("缺 workspace_id 应当被拒绝")
	}
	for _, stmt := range []string{"", "   ", "\t\n "} {
		_, err := store.Create(ctx, Entry{WorkspaceID: validUUID(1), Statement: stmt})
		if err == nil {
			t.Errorf("空白 statement %q 应当被拒绝：一条空知识进了后续 Run 的上下文只占 token", stmt)
		}
	}
}

// TestReviewOnlyAcceptsApproveOrReject 守审批入口不收别的「决定」。
//
// pending 不是审批结果（它没有发生），archived 要走 Archive——两者都必须在这里被挡住，
// 而不是落到库层的 CHECK 上变成一个 500。
func TestReviewOnlyAcceptsApproveOrReject(t *testing.T) {
	store := NewStore(nil)
	ctx := context.Background()
	ws, id, reviewer := validUUID(1), validUUID(2), validUUID(3)

	for _, decision := range []string{StatusPending, StatusArchived, "", "bogus", "APPROVED", "Approved"} {
		_, err := store.Review(ctx, id, ws, decision, "", ReviewerMember, reviewer)
		if err == nil {
			t.Errorf("decision=%q 不该被审批接受", decision)
		}
	}
	if _, err := store.Review(ctx, id, ws, StatusApproved, "", "human", reviewer); err == nil {
		t.Error("未知审阅人类型应当被拒绝")
	}
	// 一条没有批准人的「已批准」记录无法追责；库层的一致性约束也会拒，这里要提前拒。
	if _, err := store.Review(ctx, id, ws, StatusApproved, "", ReviewerMember, pgtype.UUID{}); err == nil {
		t.Error("缺批准人应当被拒绝")
	}
}

// TestArchiveRejectsInvalidInput 守归档也要记人。
func TestArchiveRejectsInvalidInput(t *testing.T) {
	store := NewStore(nil)
	ctx := context.Background()
	ws, id, reviewer := validUUID(1), validUUID(2), validUUID(3)

	if _, err := store.Archive(ctx, id, ws, "", "human", reviewer); err == nil {
		t.Error("未知审阅人类型应当被拒绝")
	}
	if _, err := store.Archive(ctx, id, ws, "", ReviewerMember, pgtype.UUID{}); err == nil {
		t.Error("缺归档人应当被拒绝")
	}
}

// TestReadsRejectInvalidInput 守读路径也不放行未知状态。
func TestReadsRejectInvalidInput(t *testing.T) {
	store := NewStore(nil)
	ctx := context.Background()

	if _, err := store.ListForWorkspace(ctx, pgtype.UUID{}, ""); err == nil {
		t.Error("缺 workspace_id 应当被拒绝")
	}
	if _, err := store.ListForWorkspace(ctx, validUUID(1), "everything"); err == nil {
		t.Error("未知状态的筛选应当被拒绝，而不是静默返回空集")
	}
	if _, err := store.DeleteByWorkspace(ctx, pgtype.UUID{}); err == nil {
		t.Error("缺 workspace_id 应当被拒绝")
	}
}

// ---- 以下走真库。库不可达时整条跳过，go test ./... 在没有数据库的机器上仍可用 ----

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTestWorkspace 造一个只属于本用例的 workspace，并把清理登记好。
//
// 不复用库里已有的 workspace：那会把用例的正确性押在「这台机器上碰巧有数据」上，
// 而且真库里的 workspace 被删会连带删掉别人的行。slug 唯一，避免撞唯一约束。
func newTestWorkspace(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	slug := fmt.Sprintf("know-%d-%d", time.Now().UnixNano(), len(t.Name()))
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO workspace (name, slug) VALUES ($1, $2) RETURNING id`,
		t.Name(), slug).Scan(&id)
	if err != nil {
		t.Fatalf("建测试 workspace: %v", err)
	}
	t.Cleanup(func() {
		// 知识条目在 workspace_id 上有 ON DELETE CASCADE，删 workspace 就跟着走。
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM workspace WHERE id = $1`, id); err != nil {
			t.Errorf("清理测试 workspace: %v", err)
		}
	})
	return id
}

// TestCreateAlwaysLandsPending 守「不能直接造一条已批准的记录」。
//
// 调用方传进来的 Status 必须被忽略：能直接建出一条 approved 的记录，就等于绕开审批，
// 而审批正是这张表存在的理由（PRD 第 5 章要求知识必须经人批准才进库）。
func TestCreateAlwaysLandsPending(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ws := newTestWorkspace(t, pool)
	ctx := context.Background()

	entry, err := store.Create(ctx, Entry{
		WorkspaceID: ws,
		Statement:   "  结论前后有空白  ",
		Rationale:   "因为",
		Status:      StatusApproved, // 必须被忽略
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if entry.Status != StatusPending {
		t.Errorf("Status = %q，Create 应当恒落 pending——调用方传入的 %q 必须被忽略",
			entry.Status, StatusApproved)
	}
	if entry.Statement != "结论前后有空白" {
		t.Errorf("Statement = %q，应当去掉首尾空白", entry.Statement)
	}
	if entry.ReviewedAt.Valid || entry.ReviewedByID.Valid || entry.ReviewedByType.Valid {
		t.Error("新建的待审条目不该带审阅痕迹")
	}
}

// TestReviewIsGuardedByPendingStatus 守 Review 的条件更新。
//
// 关键在「重复审批必须失败」：两个审批人同时点击时，先到的改状态、后到的拿 0 行。
// 条件写在 UPDATE 的 WHERE 里而不是先查后写，否则两边都看到成功、后者覆盖前者。
func TestReviewIsGuardedByPendingStatus(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ws := newTestWorkspace(t, pool)
	ctx := context.Background()
	reviewer := validUUID(9)

	entry, err := store.Create(ctx, Entry{WorkspaceID: ws, Statement: "待审的结论"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	approved, err := store.Review(ctx, entry.ID, ws, StatusApproved, "可以", ReviewerMember, reviewer)
	if err != nil {
		t.Fatalf("首次批准: %v", err)
	}
	if approved.Status != StatusApproved || !approved.ReviewedAt.Valid {
		t.Fatalf("批准后 status=%q reviewedAt.Valid=%v", approved.Status, approved.ReviewedAt.Valid)
	}

	// 第二次必须被拒，且错误要能说清当前是什么态——调用方要把它映射成 409 而不是 500。
	_, err = store.Review(ctx, entry.ID, ws, StatusRejected, "反悔", ReviewerMember, reviewer)
	var notPending ErrNotPending
	if !errors.As(err, &notPending) {
		t.Fatalf("重复审批 err = %v，应当是可识别的 ErrNotPending", err)
	}
	if notPending.Status != StatusApproved {
		t.Errorf("ErrNotPending.Status = %q，应当回读当前状态 %q", notPending.Status, StatusApproved)
	}
}

// TestReviewStoresNormalizedStatus 守「容忍首尾空白，但落库必须是规范值」。
//
// 容忍空白与 artifacts.ValidStatus 一致，本身无害。要紧的是写进去的值：
// 若 status 列里出现 "approved "（带空格），那么所有按 status = 'approved' 的查询
// 都会漏掉它——包括为注入路径建的那个部分索引。后果是一条**已经被批准、却永远
// 注入不进后续 Run** 的知识：它看起来是批过了，实际没人能读到。
//
// 所以这里不满足于「没报错」，而是走一遍已批准查询，确认它真能被读到。
func TestReviewStoresNormalizedStatus(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ws := newTestWorkspace(t, pool)
	ctx := context.Background()

	entry, err := store.Create(ctx, Entry{WorkspaceID: ws, Statement: "带空白提交的审批"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := store.Review(ctx, entry.ID, ws, " approved ", "", ReviewerMember, validUUID(9))
	if err != nil {
		t.Fatalf("批准（decision 带首尾空白）: %v", err)
	}
	if got.Status != StatusApproved {
		t.Errorf("落库 status = %q，应当是规范值 %q", got.Status, StatusApproved)
	}
	approved, err := store.ListApprovedForWorkspace(ctx, ws)
	if err != nil {
		t.Fatalf("读已批准: %v", err)
	}
	if len(approved) != 1 {
		t.Fatalf("已批准查询拿到 %d 条，应当 1 条——落库值不是规范值时这里会静默为 0", len(approved))
	}
}

// TestArchiveCannotSkipReview 守「待审不能直接归档」。
//
// 归档与拒绝的区别是「有人看过并否决了」与「没人看过」。允许 pending → archived
// 就等于给了一条绕开审批的路。
func TestArchiveCannotSkipReview(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ws := newTestWorkspace(t, pool)
	ctx := context.Background()
	reviewer := validUUID(9)

	entry, err := store.Create(ctx, Entry{WorkspaceID: ws, Statement: "还没人看过的结论"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Archive(ctx, entry.ID, ws, "过时了", ReviewerMember, reviewer); err == nil {
		t.Fatal("待审条目被直接归档了；这等于绕开审批")
	}

	// 先拒绝（有人看过），再归档，才应当成功。
	if _, err := store.Review(ctx, entry.ID, ws, StatusRejected, "不对", ReviewerMember, reviewer); err != nil {
		t.Fatalf("拒绝: %v", err)
	}
	archived, err := store.Archive(ctx, entry.ID, ws, "归档留档", ReviewerMember, reviewer)
	if err != nil {
		t.Fatalf("已拒绝条目归档: %v", err)
	}
	if archived.Status != StatusArchived {
		t.Errorf("Status = %q，应当为 %q", archived.Status, StatusArchived)
	}
}

// TestListApprovedDoesNotCrossWorkspaces 是本 Issue 最要紧的一条：
// workspace 是硬边界，跨 workspace 不能混入（PRD 6.7 里程碑 3 的退出条件）。
//
// 两个 workspace 各放一条已批准条目，读 A 只能拿到 A 的。若哪一天有人给这个包加了
// 一个不带 workspaceID 的「查全部已批准」，注入路径会立刻把 A 的知识灌进 B 的 Run。
func TestListApprovedDoesNotCrossWorkspaces(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	wsA := newTestWorkspace(t, pool)
	wsB := newTestWorkspace(t, pool)
	ctx := context.Background()
	reviewer := validUUID(9)

	mk := func(ws pgtype.UUID, statement string) {
		t.Helper()
		e, err := store.Create(ctx, Entry{WorkspaceID: ws, Statement: statement})
		if err != nil {
			t.Fatalf("Create %s: %v", statement, err)
		}
		if _, err := store.Review(ctx, e.ID, ws, StatusApproved, "", ReviewerMember, reviewer); err != nil {
			t.Fatalf("批准 %s: %v", statement, err)
		}
	}
	mk(wsA, "A 的结论")
	mk(wsB, "B 的结论")

	gotA, err := store.ListApprovedForWorkspace(ctx, wsA)
	if err != nil {
		t.Fatalf("ListApprovedForWorkspace(A): %v", err)
	}
	if len(gotA) != 1 || gotA[0].Statement != "A 的结论" {
		t.Fatalf("读 A 拿到 %d 条（%v），应当只有 A 自己那条", len(gotA), statements(gotA))
	}
	for _, e := range gotA {
		if e.WorkspaceID != wsA {
			t.Errorf("读 A 却拿到了属于其他 workspace 的条目 %v", e.ID)
		}
	}
}

// TestListForWorkspaceFiltersByStatus 守状态筛选，以及「不传状态就是不筛」。
func TestListForWorkspaceFiltersByStatus(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ws := newTestWorkspace(t, pool)
	ctx := context.Background()
	reviewer := validUUID(9)

	pending, err := store.Create(ctx, Entry{WorkspaceID: ws, Statement: "待审那条"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	approved, err := store.Create(ctx, Entry{WorkspaceID: ws, Statement: "已批准那条"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Review(ctx, approved.ID, ws, StatusApproved, "", ReviewerMember, reviewer); err != nil {
		t.Fatalf("批准: %v", err)
	}

	all, err := store.ListForWorkspace(ctx, ws, "")
	if err != nil {
		t.Fatalf("不筛状态: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("不筛状态拿到 %d 条，应当 2 条（%v）", len(all), statements(all))
	}
	onlyPending, err := store.ListForWorkspace(ctx, ws, StatusPending)
	if err != nil {
		t.Fatalf("筛 pending: %v", err)
	}
	if len(onlyPending) != 1 || onlyPending[0].ID != pending.ID {
		t.Errorf("筛 pending 拿到 %v，应当只有那条待审的", statements(onlyPending))
	}
}

// TestDeleteByWorkspaceOnlyTouchesItsOwn 守拆除不会多删。
//
// A 里两条、B 里一条，删 A 必须只影响 A 的两条。多删的行是别人的知识，而且不会报错。
func TestDeleteByWorkspaceOnlyTouchesItsOwn(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	wsA := newTestWorkspace(t, pool)
	wsB := newTestWorkspace(t, pool)
	ctx := context.Background()

	for _, stmt := range []string{"A-1", "A-2"} {
		if _, err := store.Create(ctx, Entry{WorkspaceID: wsA, Statement: stmt}); err != nil {
			t.Fatalf("Create %s: %v", stmt, err)
		}
	}
	if _, err := store.Create(ctx, Entry{WorkspaceID: wsB, Statement: "B-1"}); err != nil {
		t.Fatalf("Create B-1: %v", err)
	}

	n, err := store.DeleteByWorkspace(ctx, wsA)
	if err != nil {
		t.Fatalf("DeleteByWorkspace(A): %v", err)
	}
	if n != 2 {
		t.Errorf("删了 %d 行，应当 2 行", n)
	}
	leftA, err := store.ListForWorkspace(ctx, wsA, "")
	if err != nil {
		t.Fatalf("读 A: %v", err)
	}
	if len(leftA) != 0 {
		t.Errorf("A 还剩 %v", statements(leftA))
	}
	leftB, err := store.ListForWorkspace(ctx, wsB, "")
	if err != nil {
		t.Fatalf("读 B: %v", err)
	}
	if len(leftB) != 1 || leftB[0].Statement != "B-1" {
		t.Errorf("B 被误删了：剩 %v", statements(leftB))
	}
}

// TestCreateRejectsUnknownWorkspace 守外键真的在挡。
//
// 只写 NOT NULL 的话，一个不存在的 workspace_id 照样能插进去，「硬边界」就只剩
// 文档里的一句话。这条用例会红，如果哪天外键被挪掉了。
func TestCreateRejectsUnknownWorkspace(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)

	_, err := store.Create(context.Background(), Entry{
		WorkspaceID: validUUID(0x7f),
		Statement:   "孤儿知识",
	})
	if err == nil {
		t.Fatal("写进了一个不存在的 workspace；workspace_id 的外键没起作用")
	}
}

// TestReviewMissClassifiesNotFound 守「没找到」是一个可识别的哨兵值，且两种 miss
// 给的是同一个。
//
// HTTP 层靠 errors.Is(err, ErrNotFound) 决定回 404 还是 500。若让它去比对提示语的
// 文本，改一句话就会让 404 静默退化成 500——一个拼错的 id 变成服务端故障。
//
// 两种 miss 必须合并：「本 workspace 里没有这个 id」与「条目存在但属于别的
// workspace」。区分开就等于给了一个探测别的项目有哪些条目的接口，而 workspace 是
// 硬边界（PRD 6.7 里程碑 3 的退出条件）。
func TestReviewMissClassifiesNotFound(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	wsA := newTestWorkspace(t, pool)
	wsB := newTestWorkspace(t, pool)
	ctx := context.Background()

	// ① 这个 id 在 A 里没有。
	_, err := store.Review(ctx, validUUID(0x5a), wsA, StatusApproved, "", ReviewerMember, validUUID(9))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在的 id: err = %v，应当是 ErrNotFound", err)
	}

	// ② 条目真实存在，但属于 B。
	entry, err := store.Create(ctx, Entry{WorkspaceID: wsB, Statement: "B 的候选"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = store.Review(ctx, entry.ID, wsA, StatusApproved, "", ReviewerMember, validUUID(9))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("跨 workspace 的 id: err = %v，应当与①同一个 ErrNotFound", err)
	}
	// 两件事都不能被误判成「已经审过了」——那会让界面显示 409，把一次越权（或一次
	// 拼错）说成「这条已经被处理过」。
	var notPending ErrNotPending
	if errors.As(err, &notPending) {
		t.Error("跨 workspace 的审批被误判成了 ErrNotPending")
	}
	// 同理也不能被误判成「参数不对」。
	if errors.Is(err, ErrUnknownStatus) {
		t.Error("跨 workspace 的审批被误判成了 ErrUnknownStatus")
	}

	// 那条件目本身必须原封不动。
	still, err := store.ListForWorkspace(ctx, wsB, StatusPending)
	if err != nil {
		t.Fatalf("读 B: %v", err)
	}
	if len(still) != 1 || still[0].Status != StatusPending {
		t.Errorf("B 的条目被动了：%v", statements(still))
	}
}

// TestArchiveMissClassifiesNotFound 与上一条同源（explainMiss），只钉 Archive 这一支
// 也没漏掉哨兵——它同样被 HTTP 层按 404 处理。
func TestArchiveMissClassifiesNotFound(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	ws := newTestWorkspace(t, pool)

	_, err := store.Archive(context.Background(), validUUID(0x5b), ws, "", ReviewerMember, validUUID(9))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("归档一个不存在的 id: err = %v，应当是 ErrNotFound", err)
	}
}

func statements(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Statement)
	}
	return out
}
