package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/migrations"
)

// These tests exist because the failure they cover is silent.
//
// Re-labelling a usage row changes its rollup bucket KEY. The rollup's window
// function discovers dirty keys from updated_at, so after a row is re-labelled
// the only key it can see is the NEW model. Without a guard that enqueues the
// OLD key, the old bucket is nobody's dirty key: it is never recomputed and
// keeps its old numbers while the new bucket adds the same usage again — the
// moved row is counted twice (ruel #35). trg_tu_dirty_hourly (migration 102,
// ruel #53) now fires BEFORE UPDATE OF provider, model and enqueues the old
// key automatically, so a bare re-label is guarded. The tests pin both
// states: one proves the ghost is real when the guard is absent, another
// proves the trigger kills it.

// TestNaiveRelabelLeavesAGhostBucketWhenGuardAbsent 钉住 ruel #53 的对立面：把
// 守卫触发器（trg_tu_dirty_hourly 的 UPDATE 分支）摘掉后，裸改写维度列不把旧键入队，
// 旧桶不会被重算、用量被算两次——这正是 #35 当初的故障形态。
//
// 这是突变校验的「摘掉守卫即红」那一半：触发器在迁移 102 里默认存在，裸改写不可能再
// 留下幽灵；TestDimensionUpdateTriggerEnqueuesOldKey 钉住「守卫在」的常态。
func TestNaiveRelabelLeavesAGhostBucketWhenGuardAbsent(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	// 复现 #35 的故障形态：摘掉守卫，让裸改写（不入队旧键）把旧桶留下幽灵。
	if _, err := pool.Exec(ctx, `DROP TRIGGER IF EXISTS trg_tu_dirty_hourly ON task_usage`); err != nil {
		t.Fatalf("drop guard trigger: %v", err)
	}

	created := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	fx := seedFixture(t, ctx, pool, "ghost", created)
	wsID := fx.WorkspaceID
	ghostA := seedUsage(t, ctx, pool, fx.TaskA, "unknown", 1000, 10, 100, created)
	ghostB := seedUsage(t, ctx, pool, fx.TaskB, "unknown", 2000, 20, 200, created)
	seedUsage(t, ctx, pool, fx.TaskC, "gpt-5.6-sol", 4000, 40, 400, created)

	materializeHourly(t, ctx, pool, created.Add(-time.Hour), created.Add(time.Hour))
	assertHourlyTotals(t, ctx, pool, wsID, 3, 7000)
	assertBucketEvents(t, ctx, pool, wsID, "unknown", 2)

	// 裸改写：改名、bump updated_at，让 rollup 从 updated_at 发现新键。旧键没入队。
	if _, err := pool.Exec(ctx,
		`UPDATE task_usage SET model = 'gpt-5.6-sol', updated_at = now() WHERE id = $1`, ghostA); err != nil {
		t.Fatalf("naive relabel: %v", err)
	}
	_ = ghostB // stays a placeholder; the ghost bucket keeps counting it
	materializeHourly(t, ctx, pool, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))

	// 3 条真实用量，却被算成 4 事件：被改写的行同时落在旧桶和新桶。
	assertHourlyTotals(t, ctx, pool, wsID, 4, 8000)
	assertBucketEvents(t, ctx, pool, wsID, "unknown", 2)
}

// TestDimensionUpdateTriggerEnqueuesOldKey 钉住 ruel #53 的核心不变量：改 task_usage
// 的维度列（provider/model）必须经 trg_tu_dirty_hourly 触发器把旧键插进
// task_usage_hourly_dirty，否则旧桶不会被重算、用量被算两次。
//
// 这是「守卫在则绿」的那一半：裸改写（不显式入队，只靠触发器）后，脏队列里必须出现旧
// 键；rollup 之后旧桶消失、合计回到真实值。把触发器摘掉，本条断言立刻变红。
func TestDimensionUpdateTriggerEnqueuesOldKey(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	created := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	fx := seedFixture(t, ctx, pool, "guard", created)
	wsID := fx.WorkspaceID
	// 一行 unknown（待改写的占位 model），一行已是正式 model。
	seedUsage(t, ctx, pool, fx.TaskA, "unknown", 1000, 10, 100, created)
	seedUsage(t, ctx, pool, fx.TaskB, "gpt-5.6-sol", 4000, 40, 400, created)

	materializeHourly(t, ctx, pool, created.Add(-time.Hour), created.Add(time.Hour))
	assertHourlyTotals(t, ctx, pool, wsID, 2, 5000)
	assertBucketEvents(t, ctx, pool, wsID, "unknown", 1)

	// 裸 UPDATE（不走 backfill 的显式入队，只靠触发器守卫）：把 unknown 改成
	// 已有的 gpt-5.6-sol。
	oldModel := "unknown"
	if _, err := pool.Exec(ctx,
		`UPDATE task_usage SET model = 'gpt-5.6-sol', updated_at = now() WHERE model = $1`, oldModel); err != nil {
		t.Fatalf("bare re-label: %v", err)
	}

	// 守卫必须生效：脏队列里出现旧键 unknown，rollup 才会去重算并删掉它。
	assertDirtyKeyHasModel(t, ctx, pool, wsID, oldModel, true)

	materializeHourly(t, ctx, pool, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))

	// 没有幽灵：unknown 桶被删（0 事件），合计仍是 2 行 / 5000 token。
	assertBucketEvents(t, ctx, pool, wsID, "unknown", 0)
	assertHourlyTotals(t, ctx, pool, wsID, 2, 5000)
}

func TestApplyUpdatesRebuildsRollupWithoutTheGhost(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	created := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	fx := seedFixture(t, ctx, pool, "tool", created)
	wsID := fx.WorkspaceID
	workDir := fx.WorkDir
	seedUsage(t, ctx, pool, fx.TaskA, "unknown", 1000, 10, 100, created)
	seedUsage(t, ctx, pool, fx.TaskB, "unknown", 2000, 20, 200, created)
	seedUsage(t, ctx, pool, fx.TaskC, "gpt-5.6-sol", 4000, 40, 400, created)

	materializeHourly(t, ctx, pool, created.Add(-time.Hour), created.Add(time.Hour))
	assertHourlyTotals(t, ctx, pool, wsID, 3, 7000)

	cfg := config{provider: codexProvider, workspaceID: wsID, codexHomeDirName: codexHomeDirName, batchSize: 10}
	rows, err := loadCandidates(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("loadCandidates: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("candidates = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row.WorkDir != workDir {
			t.Fatalf("candidate work_dir = %q, want %q", row.WorkDir, workDir)
		}
	}
	resolve(rows, cfg)
	for _, row := range rows {
		if row.ResolvedModel != "gpt-5.6-sol" {
			t.Fatalf("candidate %s resolved to %q (skip=%q), want gpt-5.6-sol", row.UsageID, row.ResolvedModel, row.SkipReason)
		}
	}

	startedAt, err := databaseClock(ctx, pool)
	if err != nil {
		t.Fatalf("databaseClock: %v", err)
	}
	updated, err := applyUpdates(ctx, pool, cfg, rows)
	if err != nil {
		t.Fatalf("applyUpdates: %v", err)
	}
	if updated != 2 {
		t.Fatalf("updated = %d, want 2", updated)
	}
	finishedAt, err := databaseClock(ctx, pool)
	if err != nil {
		t.Fatalf("databaseClock: %v", err)
	}
	from, to := rollupWindow(startedAt, finishedAt)
	if _, err := rebuildRollup(ctx, pool, from, to); err != nil {
		t.Fatalf("rebuildRollup: %v", err)
	}

	// No ghost: the old bucket is gone and the totals match ground truth.
	assertBucketEvents(t, ctx, pool, wsID, "unknown", 0)
	assertHourlyTotals(t, ctx, pool, wsID, 3, 7000)

	// And re-running is a no-op, because the UPDATE is guarded on the old
	// model name.
	rows, err = loadCandidates(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("loadCandidates after backfill: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("candidates after backfill = %d, want 0", len(rows))
	}
}

func seedUsage(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	taskID, model string,
	input, output, cacheRead int64,
	createdAt time.Time,
) string {
	t.Helper()

	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO task_usage (
			task_id, provider, model,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			created_at, updated_at
		) VALUES ($1, 'codex', $2, $3, $4, $5, 0, $6, $6)
		RETURNING id
	`, taskID, model, input, output, cacheRead, createdAt).Scan(&id); err != nil {
		t.Fatalf("seed task_usage %s: %v", model, err)
	}
	return id
}

// fixture is one seeded workspace with two sibling tasks. Two, not one,
// because task_usage is unique per (task_id, provider, model): two rows with
// the same placeholder model have to live on two different tasks.
type fixture struct {
	WorkspaceID string
	RuntimeID   string
	AgentID     string
	TaskA       string
	TaskB       string
	TaskC       string
	WorkDir     string
	SessionID   string
}

// seedFixture builds workspace -> runtime -> agent -> two tasks, plus a task
// root whose codex-home holds a rollout naming the model.
func seedFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, suffix string, modTime time.Time) fixture {
	t.Helper()

	taskRoot := filepath.Join(t.TempDir(), "task-"+suffix)
	workDir := filepath.Join(taskRoot, "workdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	sessionID := "01a11401-92eb-7393-b2ab-e6bf00813ba9"
	writeFixtureRollout(t, filepath.Join(taskRoot, codexHomeDirName), sessionID, "gpt-5.6-sol", modTime)

	var wsID, runtimeID, agentID, taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug) VALUES ($1, $2) RETURNING id
	`, "model backfill "+suffix, "model-backfill-"+suffix).Scan(&wsID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider, status,
			device_info, metadata, last_seen_at
		) VALUES ($1, NULL, $2, 'cloud', 'codex', 'online', '{}'::jsonb, '{}'::jsonb, now())
		RETURNING id
	`, wsID, "model backfill runtime "+suffix).Scan(&runtimeID); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, max_concurrent_tasks
		) VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'workspace', 1)
		RETURNING id
	`, wsID, "model backfill agent "+suffix, runtimeID).Scan(&agentID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	taskID = seedTask(t, ctx, pool, agentID, runtimeID, sessionID, workDir)
	taskB := seedTask(t, ctx, pool, agentID, runtimeID, sessionID, workDir)
	taskC := seedTask(t, ctx, pool, agentID, runtimeID, sessionID, workDir)
	return fixture{
		WorkspaceID: wsID,
		RuntimeID:   runtimeID,
		AgentID:     agentID,
		TaskA:       taskID,
		TaskB:       taskB,
		TaskC:       taskC,
		WorkDir:     workDir,
		SessionID:   sessionID,
	}
}

func seedTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool, agentID, runtimeID, sessionID, workDir string) string {
	t.Helper()

	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, session_id, work_dir, status, payload)
		VALUES ($1, $2, $3, $4, 'completed', '{}'::jsonb)
		RETURNING id
	`, agentID, runtimeID, sessionID, workDir).Scan(&taskID); err != nil {
		altErr := pool.QueryRow(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, session_id, work_dir)
			VALUES ($1, $2, $3, $4)
			RETURNING id
		`, agentID, runtimeID, sessionID, workDir).Scan(&taskID)
		if altErr != nil {
			t.Fatalf("seed agent_task_queue: %v / %v", err, altErr)
		}
	}
	return taskID
}

// assertDirtyKeyHasModel 断言 task_usage_hourly_dirty 里是否存在某个 workspace 下、
// 某个旧 model 的脏键。触发器 trg_tu_dirty_hourly（迁移 102，ruel #53）在改维度列时
// 必须把旧键插进来；摘掉触发器则这里找不到，TestDimensionUpdateTriggerEnqueuesOldKey
// 据此判断守卫是否生效。
func assertDirtyKeyHasModel(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID, model string, want bool) {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(count(*), 0)::bigint
		  FROM task_usage_hourly_dirty WHERE workspace_id = $1 AND model = $2
	`, wsID, model).Scan(&n); err != nil {
		t.Fatalf("read dirty queue: %v", err)
	}
	if want && n == 0 {
		t.Fatalf("dirty queue has 0 rows for model=%s in ws %s, want >= 1 (trigger must enqueue the old key)", model, wsID)
	}
	if !want && n != 0 {
		t.Fatalf("dirty queue has %d rows for model=%s in ws %s, want 0", n, model, wsID)
	}
}

func materializeHourly(t *testing.T, ctx context.Context, pool *pgxpool.Pool, from, to time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`SELECT rollup_task_usage_hourly_window($1::timestamptz, $2::timestamptz)`, from, to); err != nil {
		t.Fatalf("rollup window %s..%s: %v", from.Format(time.RFC3339), to.Format(time.RFC3339), err)
	}
}

func assertHourlyTotals(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID string, wantEvents, wantInput int64) {
	t.Helper()
	var events, input int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(event_count), 0)::bigint, COALESCE(SUM(input_tokens), 0)::bigint
		  FROM task_usage_hourly WHERE workspace_id = $1
	`, wsID).Scan(&events, &input); err != nil {
		t.Fatalf("read hourly totals: %v", err)
	}
	if events != wantEvents || input != wantInput {
		t.Fatalf("hourly totals = %d events / %d input tokens, want %d / %d",
			events, input, wantEvents, wantInput)
	}
}

func assertBucketEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID, model string, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(event_count), 0)::bigint
		  FROM task_usage_hourly WHERE workspace_id = $1 AND model = $2
	`, wsID, model).Scan(&got); err != nil {
		t.Fatalf("read bucket %s: %v", model, err)
	}
	if got != want {
		t.Fatalf("model=%s bucket events = %d, want %d", model, got, want)
	}
}

func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	adminURL := os.Getenv("DATABASE_URL")
	if adminURL == "" {
		adminURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	if !testDatabaseReachable(ctx, adminURL) {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}

	tmpDB := fmt.Sprintf("multica_codex_model_backfill_%d", time.Now().UnixNano())
	if err := testCreateDatabase(ctx, adminURL, tmpDB); err != nil {
		t.Fatalf("create temp database %s: %v", tmpDB, err)
	}
	t.Cleanup(func() {
		if err := testDropDatabase(context.Background(), adminURL, tmpDB); err != nil {
			t.Logf("drop temp database %s: %v", tmpDB, err)
		}
	})

	pool, err := pgxpool.New(ctx, testReplaceDatabase(adminURL, tmpDB))
	if err != nil {
		t.Fatalf("connect to temp database: %v", err)
	}
	t.Cleanup(pool.Close)

	// Stop at the hourly pipeline: that is where the bucket key and the
	// dirty queue this whole tool is about were introduced.
	if err := testApplyMigrationsUpTo(ctx, pool, "102_task_usage_hourly_pipeline"); err != nil {
		t.Fatalf("apply migrations to 102: %v", err)
	}
	return pool
}

func testResolveMigrationsDir() (string, error) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller(0) failed")
	}
	dir := filepath.Clean(filepath.Join(filepath.Dir(here), "..", "..", "migrations"))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("migrations dir not at %s", dir)
	}
	return dir, nil
}

func testDatabaseReachable(ctx context.Context, url string) bool {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return false
	}
	defer pool.Close()
	return pool.Ping(ctx) == nil
}

func testCreateDatabase(ctx context.Context, adminURL, name string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name))
	return err
}

func testDropDatabase(ctx context.Context, adminURL, name string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
	return err
}

func testReplaceDatabase(url, name string) string {
	idx := strings.LastIndex(url, "/")
	if idx < 0 {
		return url
	}
	rest := url[idx+1:]
	q := strings.Index(rest, "?")
	if q < 0 {
		return url[:idx+1] + name
	}
	return url[:idx+1] + name + rest[q:]
}

func testApplyMigrationsUpTo(ctx context.Context, pool *pgxpool.Pool, lastVersion string) error {
	dir, err := testResolveMigrationsDir()
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)

	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return err
	}
	for _, f := range files {
		v := migrations.ExtractVersion(f)
		sql, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", v, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, v); err != nil {
			return err
		}
		if v == lastVersion {
			return nil
		}
	}
	return fmt.Errorf("migration %q not found", lastVersion)
}
