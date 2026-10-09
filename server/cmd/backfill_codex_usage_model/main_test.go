package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/metrics"
)

// writeFixtureRollout writes a minimal Codex rollout the way Codex does: a
// session_meta line that owns the thread, then events that name the model.
// It returns the path so tests can point work_dir at the right task root.
func writeFixtureRollout(t *testing.T, codexHome, threadID, model string, modTime time.Time) string {
	t.Helper()

	sessions := filepath.Join(codexHome, "sessions", "2026", "10", "07")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	path := filepath.Join(sessions, "rollout-2026-10-07T09-00-00-"+threadID+".jsonl")
	body := `{"timestamp":"2026-10-07T01:00:00.000Z","type":"session_meta","payload":{"id":"` + threadID + `"}}` + "\n" +
		`{"timestamp":"2026-10-07T01:00:01.000Z","type":"turn_context","payload":{"model":"` + model + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("set rollout mtime: %v", err)
	}
	return path
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     config
		wantErr string
	}{
		{name: "defaults are valid", cfg: config{batchSize: 100, codexHomeDirName: codexHomeDirName}},
		{name: "batch size must be positive", cfg: config{batchSize: 0, codexHomeDirName: codexHomeDirName}, wantErr: "batch-size"},
		{name: "limit must not be negative", cfg: config{batchSize: 1, limit: -1, codexHomeDirName: codexHomeDirName}, wantErr: "limit"},
		{name: "codex home dir must not be empty", cfg: config{batchSize: 1, codexHomeDirName: "  "}, wantErr: "codex-home-dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() = nil, want error containing %q", tt.wantErr)
			}
		})
	}
}

func TestDeriveCodexHome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		workDir string
		want    string
	}{
		{
			name:    "task root is the parent of work_dir",
			workDir: filepath.Join(string(filepath.Separator), "srv", "ruel-12", "workdir"),
			want:    filepath.Join(string(filepath.Separator), "srv", "ruel-12", "codex-home"),
		},
		{
			name:    "empty work_dir yields nothing",
			workDir: "   ",
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := deriveCodexHome(tt.workDir, codexHomeDirName); got != tt.want {
				t.Fatalf("deriveCodexHome(%q) = %q, want %q", tt.workDir, got, tt.want)
			}
		})
	}
}

// TestResolveReadsTheNameOutOfTheRunOwnRollout is the core guarantee: the
// replacement name comes from that run's file, and from nowhere else.
func TestResolveReadsTheNameOutOfTheRunOwnRollout(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	threadA := "01a11401-92eb-7393-b2ab-e6bf00813ba9"
	threadB := "01a11404-6cab-7e13-8eec-3a725a657246"
	homeA := filepath.Join(root, "task-a", codexHomeDirName)
	homeB := filepath.Join(root, "task-b", codexHomeDirName)
	modTime := time.Now()
	writeFixtureRollout(t, homeA, threadA, "gpt-5.6-sol", modTime)
	writeFixtureRollout(t, homeB, threadB, "gpt-5.6-terra", modTime)

	// The rollout is written DURING the run: after it started, before the
	// usage row landed. This is not a stylistic detail — resolving with the
	// usage row's created_at as the scan floor makes every real row come back
	// "no rollout found", because the file's mtime is earlier than that.
	rows := []candidate{
		{
			UsageID:   "a",
			SessionID: threadA,
			WorkDir:   filepath.Join(root, "task-a", "workdir"),
			Model:     "unknown",
			StartedAt: modTime.Add(-time.Minute),
			CreatedAt: modTime.Add(time.Minute),
		},
		{
			UsageID:   "b",
			SessionID: threadB,
			WorkDir:   filepath.Join(root, "task-b", "workdir"),
			Model:     "unknown",
			StartedAt: modTime.Add(-time.Minute),
			CreatedAt: modTime.Add(time.Minute),
		},
	}
	resolve(rows, config{codexHomeDirName: codexHomeDirName})

	if rows[0].ResolvedModel != "gpt-5.6-sol" {
		t.Fatalf("row a resolved to %q, want gpt-5.6-sol", rows[0].ResolvedModel)
	}
	if rows[1].ResolvedModel != "gpt-5.6-terra" {
		t.Fatalf("row b resolved to %q, want gpt-5.6-terra", rows[1].ResolvedModel)
	}
	if rows[0].RolloutPath == "" || rows[1].RolloutPath == "" {
		t.Fatalf("resolve must report the rollout it read: %q / %q", rows[0].RolloutPath, rows[1].RolloutPath)
	}
	if rows[0].SkipReason != "" || rows[1].SkipReason != "" {
		t.Fatalf("resolved rows must not carry a skip reason: %q / %q", rows[0].SkipReason, rows[1].SkipReason)
	}
}

// TestResolveNeverGuesses guards the other half: a row whose rollout is gone
// stays a placeholder. Inventing a model name would put a price on a run
// nobody can prove ran on that model.
func TestResolveNeverGuesses(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rows := []candidate{
		{UsageID: "no-workdir", SessionID: "thread-1", Model: "unknown"},
		{UsageID: "no-session", WorkDir: filepath.Join(root, "task", "workdir"), Model: "unknown"},
		{UsageID: "no-rollout", WorkDir: filepath.Join(root, "task", "workdir"), SessionID: "thread-3", Model: "unknown"},
	}
	resolve(rows, config{codexHomeDirName: codexHomeDirName})

	for _, row := range rows {
		if row.ResolvedModel != "" {
			t.Fatalf("%s resolved to %q, want empty", row.UsageID, row.ResolvedModel)
		}
		if row.SkipReason == "" {
			t.Fatalf("%s was skipped without a reason", row.UsageID)
		}
		if row.resolvable() {
			t.Fatalf("%s must not be resolvable", row.UsageID)
		}
	}
}

// TestPlanShowsTheCostSwing is the 口径 check: a placeholder model is
// `unpriced` under the four-state cost model, so re-labelling a row MOVES it
// into priced territory and raises historical cost. The dry-run report has to
// say so, in dollars, before anything is written.
func TestPlanShowsTheCostSwing(t *testing.T) {
	t.Parallel()

	row := candidate{
		UsageID:     "u1",
		Model:       "unknown",
		ResolvedModel: "gpt-5.6-sol",
		InputTokens: 28833,
		Output:      969,
		CacheRead:   122880,
	}
	total := logPlan([]candidate{row})

	before := metrics.EstimateUsageCost("unknown", "", 0, row.InputTokens, row.Output, row.CacheRead, row.CacheWrite)
	after := metrics.EstimateUsageCost("gpt-5.6-sol", "codex", 0, row.InputTokens, row.Output, row.CacheRead, row.CacheWrite)
	if before.Priceable() {
		t.Fatalf("placeholder model must be unpriced, got %s %.6f", before.Source, before.USD)
	}
	if !after.Priceable() || after.USD <= 0 {
		t.Fatalf("gpt-5.6-sol must be priced above zero, got %s %.6f", after.Source, after.USD)
	}
	if total.BeforePriced != 0 || total.AfterPriced != 1 {
		t.Fatalf("priced rows before/after = %d/%d, want 0/1", total.BeforePriced, total.AfterPriced)
	}
	if total.AfterUSD != after.USD {
		t.Fatalf("totals after = %.6f, want %.6f", total.AfterUSD, after.USD)
	}
}

// TestRolloutScanFloorIsTheRunStart pins the bug above in isolation: the
// floor has to come from the run, and a missing started_at must fall back to
// "no floor" rather than to the usage row's own timestamp.
func TestRolloutScanFloorIsTheRunStart(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 10, 7, 9, 36, 42, 0, time.UTC)
	withStart := candidate{StartedAt: started, CreatedAt: started.Add(44 * time.Second)}
	if got := withStart.rolloutScanFloor(); !got.Equal(started) {
		t.Fatalf("rolloutScanFloor = %s, want the run start %s", got, started)
	}

	noStart := candidate{CreatedAt: started.Add(44 * time.Second)}
	if got := noStart.rolloutScanFloor(); !got.IsZero() {
		t.Fatalf("missing started_at: rolloutScanFloor = %s, want the zero time (no floor)", got)
	}
}

func TestResolvableIgnoresNoOpRename(t *testing.T) {
	t.Parallel()

	same := candidate{Model: "gpt-5.6-sol", ResolvedModel: "gpt-5.6-sol"}
	if same.resolvable() {
		t.Fatal("a rename to the current name is not a change worth writing")
	}
	fixed := candidate{Model: "unknown", ResolvedModel: "gpt-5.6-sol"}
	if !fixed.resolvable() {
		t.Fatal("placeholder -> real name must be resolvable")
	}
}
