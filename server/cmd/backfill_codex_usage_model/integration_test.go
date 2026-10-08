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

// These two tests exist because the failure they cover is silent.
//
// Re-labelling a usage row changes its rollup bucket KEY. task_usage has no
// UPDATE trigger (only BEFORE DELETE), so the rollup's window function can
// only discover the NEW key from updated_at — the old 'unknown' bucket is
// nobody's dirty key, is never recomputed, and keeps its old numbers while
// the new bucket adds the same usage again. The first test proves the ghost
// is real; the second proves applyUpdates kills it by enqueueing the old key.

func TestNaiveRelabelLeavesAGhostBucket(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	created := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	fx := seedFixture(t, ctx, pool, "ghost", created)
	wsID := fx.WorkspaceID
	ghostA := seedUsage(t, ctx, pool, fx.TaskA, "unknown", 1000, 10, 100, created)
	ghostB := seedUsage(t, ctx, pool, fx.TaskB, "unknown", 2000, 20, 200, created)
	seedUsage(t, ctx, pool, fx.TaskC, "gpt-5.6-sol", 4000, 40, 400, created)

	materializeHourly(t, ctx, pool, created.Add(-time.Hour), created.Add(time.Hour))
	assertHourlyTotals(t, ctx, pool, wsID, 3, 7000)
	assertBucketEvents(t, ctx, pool, wsID, "unknown", 2)

	// The naive repair: rename the row, bump updated_at, let the rollup
	// discover it. No old key is enqueued.
	if _, err := pool.Exec(ctx,
		`UPDATE task_usage SET model = 'gpt-5.6-sol', updated_at = now() WHERE id = $1`, ghostA); err != nil {
		t.Fatalf("naive relabel: %v", err)
	}
	_ = ghostB // stays a placeholder; the ghost bucket keeps counting it
	materializeHourly(t, ctx, pool, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))

	// 3 real rows, but 4 events across the buckets: the moved row is counted
	// in both its old bucket and its new one.
	assertHourlyTotals(t, ctx, pool, wsID, 4, 8000)
	assertBucketEvents(t, ctx, pool, wsID, "unknown", 2)
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
