// Backfill_codex_usage_model puts a real model name back on historical
// Codex task_usage rows that were written before the adapter learned to
// report one, and lands them as `unknown`.
//
// This is a hosted-data repair tool, not a general migration:
//   - dry-run is the default; pass --execute to mutate data
//   - only provider='codex' rows whose model is a placeholder are touched
//   - the replacement name is READ from the run's own rollout file, never
//     inferred from neighbouring rows and never defaulted
//   - rows whose rollout is gone are skipped and counted, not guessed
//   - the OLD rollup bucket key is enqueued explicitly (see below)
//
// THE ONE NON-OBVIOUS BIT — why this tool cannot just UPDATE and leave the
// rollup to discover the change:
//
// `task_usage_hourly` is keyed on (bucket_hour, workspace, runtime, agent,
// project, provider, model), and `task_usage` has NO update trigger — the
// only trigger on it is `trg_tu_dirty_hourly`, BEFORE DELETE. The rollup's
// window function discovers dirty keys from `updated_at`, so after a row is
// re-labelled the only dirty key it can see is the NEW model. The old
// 'unknown' bucket is therefore never recomputed and never deleted: it keeps
// its old numbers while the new bucket adds the same usage again.
//
// Measured on the 3 rows this tool exists for (see ruel #35): task_usage
// held 15 codex rows / 220175 input tokens, but after a naive re-label the
// hourly buckets summed to 16 events / 236514 input tokens — the moved row
// counted twice. Enqueueing the old key into `task_usage_hourly_dirty` puts
// it in the dirty set, `deleted_empty` removes it, and the totals return to
// 15 / 220175.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/pkg/agent"
)

// rollupAdvisoryLockID is the hourly rollup's lock (migration 272). Taken
// transaction-scoped here, so a cancelled or failed run cannot leak it —
// which is exactly the bug 272 fixed for the cron path.
const rollupAdvisoryLockID = 4246

// codexHomeDirName is the per-task CODEX_HOME directory name, kept in sync
// with internal/daemon/execenv.reclaimable.go. Duplicated rather than
// imported so this offline tool does not pull in the daemon's execenv tree.
const codexHomeDirName = "codex-home"

// codexProvider is the only provider this tool knows how to re-read a model
// name for: codex writes the model into its rollout file, and that file is
// the only place the name survives (see pkg/agent.codexSessionModel).
const codexProvider = "codex"

type config struct {
	provider            string
	workspaceID         string
	codexHomeDirName    string
	sessionsOverride    string
	batchSize           int
	limit               int
	sleepBetweenBatches time.Duration
	execute             bool
	rebuildRollup       bool
}

// candidate is one task_usage row that lost its model name.
type candidate struct {
	UsageID     string
	TaskID      string
	SessionID   string
	WorkDir     string
	Model       string
	CreatedAt   time.Time
	StartedAt   time.Time
	InputTokens int64
	Output      int64
	CacheRead   int64
	CacheWrite  int64

	// Provider is not selected per row: the load query pins every candidate
	// to cfg.provider, so there is exactly one value per run. It is carried
	// on the row anyway because EstimateUsageCost prices generic ids
	// (codex's `auto`, Cursor's `auto`) only when it knows which provider
	// reported them — a bare `auto` is ambiguous and must not be priced.
	Provider string

	// resolved by resolve(), not by the database
	ResolvedModel string
	RolloutPath   string
	CodexHome     string
	SkipReason    string
}

// resolvable reports whether this row has a real name to write back.
func (c candidate) resolvable() bool {
	return c.ResolvedModel != "" && c.ResolvedModel != c.Model
}

// rolloutScanFloor is the earliest rollout mtime the scan will accept.
//
// It must be the RUN's start, not the usage row's created_at. The live path
// passes the run start too, and the difference matters: Codex writes its last
// token_count before the run finishes, so the rollout's mtime is routinely
// EARLIER than the moment the usage row was inserted. Passing created_at here
// made all three real rows resolve to "no rollout found" — the file was
// there, the filter had just excluded it.
//
// started_at can be NULL (a task that never got dispatched), and a zero time
// means "no floor" in the scanner, so falling back to CreatedAt would
// silently reintroduce the bug. Fall back to the zero time instead.
func (c candidate) rolloutScanFloor() time.Time {
	if c.StartedAt.IsZero() {
		return time.Time{}
	}
	return c.StartedAt
}

type totals struct {
	Candidates   int
	Resolved     int
	Skipped      int
	Updated      int
	BeforeUSD    float64
	BeforePriced int
	AfterUSD     float64
	AfterPriced  int
}

func main() {
	logger.Init()
	if err := run(); err != nil {
		slog.Error("codex usage model backfill failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config{}
	flag.StringVar(&cfg.workspaceID, "workspace-id", "", "optional workspace UUID to limit the backfill")
	flag.StringVar(&cfg.codexHomeDirName, "codex-home-dir", codexHomeDirName, "per-task CODEX_HOME directory name, relative to the task's work_dir parent")
	flag.StringVar(&cfg.sessionsOverride, "codex-home", "", "use this CODEX_HOME for every row instead of deriving one per task")
	flag.IntVar(&cfg.batchSize, "batch-size", 100, "rows per transaction when --execute is set")
	flag.IntVar(&cfg.limit, "limit", 0, "stop after this many candidate rows (0 = no limit)")
	flag.DurationVar(&cfg.sleepBetweenBatches, "sleep-between-batches", 0, "pause between batches to throttle write pressure")
	flag.BoolVar(&cfg.execute, "execute", false, "mutate task_usage rows; without this flag the command only prints a dry-run report")
	flag.BoolVar(&cfg.rebuildRollup, "rebuild-rollup", true, "after --execute, call rollup_task_usage_hourly_window for the update window")
	flag.Parse()
	cfg.provider = codexProvider

	if err := cfg.validate(); err != nil {
		return err
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	rows, err := loadCandidates(ctx, pool, cfg)
	if err != nil {
		return err
	}
	resolve(rows, cfg)
	total := logPlan(rows)

	if total.Candidates == 0 {
		slog.Info("no codex task_usage rows with a placeholder model name found")
		return nil
	}
	if !cfg.execute {
		slog.Info("dry-run complete; review the report, then re-run with --execute to apply the backfill")
		return nil
	}

	updateStartedAt, err := databaseClock(ctx, pool)
	if err != nil {
		return err
	}
	updated, err := applyUpdates(ctx, pool, cfg, rows)
	if err != nil {
		return err
	}
	total.Updated = updated
	slog.Info("task_usage rows re-labelled", "rows", updated)
	if updated == 0 || !cfg.rebuildRollup {
		return nil
	}

	updateFinishedAt, err := databaseClock(ctx, pool)
	if err != nil {
		return err
	}
	rollupFrom, rollupTo := rollupWindow(updateStartedAt, updateFinishedAt)
	rollupRows, err := rebuildRollup(ctx, pool, rollupFrom, rollupTo)
	if err != nil {
		return err
	}
	slog.Info("hourly rollup rebuilt",
		"from", rollupFrom.Format(time.RFC3339),
		"to", rollupTo.Format(time.RFC3339),
		"rows_touched", rollupRows)
	return nil
}

func (cfg *config) validate() error {
	if cfg.batchSize <= 0 {
		return fmt.Errorf("--batch-size must be positive")
	}
	if cfg.limit < 0 {
		return fmt.Errorf("--limit must not be negative")
	}
	if strings.TrimSpace(cfg.codexHomeDirName) == "" {
		return fmt.Errorf("--codex-home-dir must not be empty")
	}
	return nil
}

// loadCandidates reads every placeholder-model codex row, newest last.
func loadCandidates(ctx context.Context, pool *pgxpool.Pool, cfg config) ([]candidate, error) {
	const query = `
SELECT tu.id::text,
       tu.task_id::text,
       tu.model,
       tu.created_at,
       COALESCE(atq.started_at, tu.created_at)::timestamptz,
       COALESCE(atq.session_id, '')::text,
       COALESCE(atq.work_dir, '')::text,
       COALESCE(tu.input_tokens, 0)::bigint,
       COALESCE(tu.output_tokens, 0)::bigint,
       COALESCE(tu.cache_read_tokens, 0)::bigint,
       COALESCE(tu.cache_write_tokens, 0)::bigint
  FROM task_usage tu
  JOIN agent_task_queue atq ON atq.id   = tu.task_id
  JOIN agent            a   ON a.id     = atq.agent_id
 WHERE tu.provider = $1
   AND lower(btrim(COALESCE(tu.model, ''))) = ANY($2::text[])
   AND (NULLIF($3, '')::uuid IS NULL OR a.workspace_id = NULLIF($3, '')::uuid)
 ORDER BY tu.created_at, tu.id`

	// --limit is an int we format ourselves, not user text, so splicing it
	// into the statement cannot inject anything; pgx has no placeholder for
	// LIMIT that also keeps the rest of the parameter list readable.
	stmt := query
	if cfg.limit > 0 {
		stmt += fmt.Sprintf(" LIMIT %d", cfg.limit)
	}

	rows, err := pool.Query(ctx, stmt, cfg.provider, metrics.ModelPlaceholderValues, cfg.workspaceID)
	if err != nil {
		return nil, fmt.Errorf("load placeholder-model codex rows: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(
			&c.UsageID, &c.TaskID, &c.Model, &c.CreatedAt, &c.StartedAt,
			&c.SessionID, &c.WorkDir,
			&c.InputTokens, &c.Output, &c.CacheRead, &c.CacheWrite,
		); err != nil {
			return nil, fmt.Errorf("scan placeholder-model codex row: %w", err)
		}
		c.Provider = cfg.provider
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate placeholder-model codex rows: %w", err)
	}
	return out, nil
}

// resolve fills in the real model name for every candidate, in place.
//
// It never invents a value: a row whose rollout file is gone (GC'd, or a
// layout that changed) stays a placeholder and is reported as skipped.
func resolve(rows []candidate, cfg config) {
	for i := range rows {
		c := &rows[i]
		home := strings.TrimSpace(cfg.sessionsOverride)
		if home == "" {
			home = deriveCodexHome(c.WorkDir, cfg.codexHomeDirName)
		}
		c.CodexHome = home
		switch {
		case home == "":
			c.SkipReason = "no work_dir to derive a CODEX_HOME from"
			continue
		case strings.TrimSpace(c.SessionID) == "":
			c.SkipReason = "task has no session_id, no rollout to read"
			continue
		}
		model, path := agent.CodexSessionModel(c.rolloutScanFloor(), home, c.SessionID)
		if model == "" {
			c.SkipReason = "rollout unreadable or has no model name"
			continue
		}
		c.ResolvedModel = model
		c.RolloutPath = path
	}
}

// deriveCodexHome turns agent_task_queue.work_dir into the per-task
// CODEX_HOME: the task root is work_dir's parent, and CODEX_HOME is a named
// directory inside it.
func deriveCodexHome(workDir, dirName string) string {
	wd := strings.TrimSpace(workDir)
	if wd == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(filepath.Clean(wd)), dirName)
}

// logPlan prints one line per candidate plus the cost swing the re-label
// causes, and returns the totals.
//
// The cost swing is the whole reason this tool prints anything before
// mutating: a placeholder model is `unpriced` under the four-state cost
// model, so it is excluded from medians and budgets. Re-labelling moves
// those rows into `table` pricing, which RAISES historical cost. That is a
// correction, but it has to be visible before it happens, not discovered
// afterwards.
func logPlan(rows []candidate) totals {
	var total totals
	for _, c := range rows {
		total.Candidates++
		if !c.resolvable() {
			total.Skipped++
			slog.Info("candidate skipped",
				"usage_id", c.UsageID,
				"created_at", c.CreatedAt.UTC().Format(time.RFC3339),
				"model", c.Model,
				"reason", c.SkipReason)
			continue
		}
		total.Resolved++
		before := metrics.EstimateUsageCost(c.Model, c.Provider, 0, c.InputTokens, c.Output, c.CacheRead, c.CacheWrite)
		after := metrics.EstimateUsageCost(c.ResolvedModel, c.Provider, 0, c.InputTokens, c.Output, c.CacheRead, c.CacheWrite)
		if before.Priceable() {
			total.BeforePriced++
			total.BeforeUSD += before.USD
		}
		if after.Priceable() {
			total.AfterPriced++
			total.AfterUSD += after.USD
		}
		slog.Info("candidate resolved",
			"usage_id", c.UsageID,
			"created_at", c.CreatedAt.UTC().Format(time.RFC3339),
			"model_before", c.Model,
			"model_after", c.ResolvedModel,
			"rollout", c.RolloutPath,
			"cost_before", fmt.Sprintf("%s %.6f", before.Source, before.USD),
			"cost_after", fmt.Sprintf("%s %.6f", after.Source, after.USD))
	}
	slog.Info("plan totals",
		"candidates", total.Candidates,
		"resolved", total.Resolved,
		"skipped", total.Skipped,
		"priced_rows_before", total.BeforePriced,
		"priced_usd_before", total.BeforeUSD,
		"priced_rows_after", total.AfterPriced,
		"priced_usd_after", total.AfterUSD)
	return total
}

// applyUpdates writes the resolved names back, batch by batch.
//
// Each batch is one transaction that does two things per row, and the second
// is the one that is easy to forget: re-label the row AND enqueue the OLD
// bucket key into task_usage_hourly_dirty. Without the enqueue the old
// bucket is never recomputed and the usage is counted twice (see the header
// comment).
func applyUpdates(ctx context.Context, pool *pgxpool.Pool, cfg config, rows []candidate) (int, error) {
	const relabel = `
UPDATE task_usage
   SET model = $1, updated_at = now()
 WHERE id = $2
   AND model = $3`

	const enqueueOldKey = `
INSERT INTO task_usage_hourly_dirty (
    bucket_hour, workspace_id, runtime_id, agent_id, project_id, provider, model
)
SELECT task_usage_hour_bucket(tu.created_at),
       a.workspace_id,
       atq.runtime_id,
       atq.agent_id,
       i.project_id,
       tu.provider,
       $2
  FROM task_usage tu
  JOIN agent_task_queue atq ON atq.id   = tu.task_id
  JOIN agent            a   ON a.id     = atq.agent_id
  LEFT JOIN issue       i   ON i.id     = atq.issue_id
 WHERE tu.id = $1
   AND atq.runtime_id IS NOT NULL
ON CONFLICT ON CONSTRAINT uq_task_usage_hourly_dirty_key DO UPDATE
    SET enqueued_at = GREATEST(task_usage_hourly_dirty.enqueued_at, EXCLUDED.enqueued_at)`

	var pending []candidate
	for _, c := range rows {
		if c.resolvable() {
			pending = append(pending, c)
		}
	}

	updated := 0
	for start := 0; start < len(pending); start += cfg.batchSize {
		end := start + cfg.batchSize
		if end > len(pending) {
			end = len(pending)
		}
		batch := pending[start:end]

		tx, err := pool.Begin(ctx)
		if err != nil {
			return updated, fmt.Errorf("begin batch transaction: %w", err)
		}
		for _, c := range batch {
			tag, err := tx.Exec(ctx, relabel, c.ResolvedModel, c.UsageID, c.Model)
			if err != nil {
				_ = tx.Rollback(ctx)
				return updated, fmt.Errorf("re-label task_usage %s: %w", c.UsageID, err)
			}
			if tag.RowsAffected() == 0 {
				// Someone re-labelled it between load and apply. Not an
				// error, but do not enqueue a stale old key for it.
				slog.Warn("candidate no longer has its placeholder model; leaving it alone",
					"usage_id", c.UsageID, "expected_model", c.Model)
				continue
			}
			if _, err := tx.Exec(ctx, enqueueOldKey, c.UsageID, c.Model); err != nil {
				_ = tx.Rollback(ctx)
				return updated, fmt.Errorf("enqueue old rollup key for %s: %w", c.UsageID, err)
			}
			updated++
		}
		if err := tx.Commit(ctx); err != nil {
			return updated, fmt.Errorf("commit batch: %w", err)
		}
		slog.Info("applied batch", "rows", len(batch), "total", updated)

		if cfg.sleepBetweenBatches > 0 {
			select {
			case <-time.After(cfg.sleepBetweenBatches):
			case <-ctx.Done():
				return updated, ctx.Err()
			}
		}
	}
	return updated, nil
}

// rebuildRollup replays the hourly rollup over the update window under the
// rollup's own advisory lock, taken transaction-scoped.
func rebuildRollup(ctx context.Context, pool *pgxpool.Pool, from, to time.Time) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin rollup transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, rollupAdvisoryLockID); err != nil {
		return 0, fmt.Errorf("acquire advisory lock %d: %w", rollupAdvisoryLockID, err)
	}
	var touched int64
	if err := tx.QueryRow(ctx,
		`SELECT rollup_task_usage_hourly_window($1::timestamptz, $2::timestamptz)`,
		from, to,
	).Scan(&touched); err != nil {
		return 0, fmt.Errorf("rebuild hourly rollup for %s..%s: %w",
			from.Format(time.RFC3339), to.Format(time.RFC3339), err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit rollup transaction: %w", err)
	}
	return touched, nil
}

func databaseClock(ctx context.Context, pool *pgxpool.Pool) (time.Time, error) {
	var ts time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&ts); err != nil {
		return time.Time{}, fmt.Errorf("read database clock: %w", err)
	}
	return ts.UTC(), nil
}

func rollupWindow(startedAt, finishedAt time.Time) (time.Time, time.Time) {
	return startedAt.UTC().Add(-time.Second), finishedAt.UTC().Add(time.Second)
}
