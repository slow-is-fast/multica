package daemon

// Ruel 新增：清理预览的测试（P0-9）。
//
// 这个文件只守两条硬性质，其余都是附带的：
//
//  1. 预览不删任何东西。这是预览存在的全部意义——它让用户敢去看「下一轮会没什么」，
//     而一个会顺手删东西的预览不叫预览。所以这里不只断言动作名正确，还要断言目录、
//     文件数、字节数在 PlanGC 前后完全一致。
//  2. 预览说的与真实 GC 做的一致。预览若另走一条判断路径，两处迟早分叉，而会撒谎的
//     预览比没有预览更糟。所以同一个 fixture 上先跑 PlanGC 记下动作，再跑真实 GC，
//     逐条核对：说会删的必须真删了，说会留的必须真留着。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

const (
	gcPlanDoneIssueID = "88888888-8888-8888-8888-888888888881"
	gcPlanOpenIssueID = "88888888-8888-8888-8888-888888888882"
)

// newGCPlanTestDaemon 搭一个能回答 issue 批量 gc-check 的 daemon。
//
// 真实 GC 与预览都走批量端点（gcWorkspaceIssues），所以这里只挂批量端点；若哪天预览
// 退化成逐个问，legacy 端点会被打到，测试会立刻失败——那正是要拦的分叉。
func newGCPlanTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/daemon/workspaces/ws-plan/issues/gc-check", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			IssueIDs []string `json:"issue_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode gc-check body: %v", err)
		}
		issues := make([]map[string]any, 0, len(body.IssueIDs))
		for _, id := range body.IssueIDs {
			switch id {
			case gcPlanDoneIssueID:
				issues = append(issues, map[string]any{
					"id": id, "found": true, "status": "done",
					"updated_at": time.Now().Add(-10 * 24 * time.Hour),
				})
			case gcPlanOpenIssueID:
				issues = append(issues, map[string]any{
					"id": id, "found": true, "status": "in_progress",
					"updated_at": time.Now().Add(-10 * 24 * time.Hour),
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": issues})
	})
	mux.HandleFunc("/api/daemon/issues/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "legacy endpoint must not be called", http.StatusInternalServerError)
	})
	d := newGCTestDaemon(t, mux)
	// 孤立目录按 mtime 判定，fixture 不可能真的等 72 小时。TTL 归零的语义是「一律
	// 视为过期」，与既有测试一致。
	d.cfg.GCOrphanTTL = 0
	return d
}

// gcPlanFixture 造四种命运的目录：会整删的、会留的、无主的、正在跑的。
type gcPlanFixture struct {
	daemon *Daemon
	wsDir  string
	clean  string // 父记录已终态 → clean
	keep   string // 父记录仍在进行 → keep
	orphan string // 无 meta 且已过期 → orphan
	active string // 有任务正在跑 → active
}

func newGCPlanFixture(t *testing.T, d *Daemon) gcPlanFixture {
	t.Helper()
	f := gcPlanFixture{daemon: d, wsDir: filepath.Join(d.cfg.WorkspacesRoot, "ws-plan")}

	f.clean = createTaskDir(t, d.cfg.WorkspacesRoot, "ws-plan", "clean-task", &execenv.GCMeta{
		Kind:        execenv.GCKindIssue,
		IssueID:     gcPlanDoneIssueID,
		WorkspaceID: "ws-plan",
		CompletedAt: time.Now().Add(-10 * 24 * time.Hour),
	})
	// 未提交的工作：clean 会连它一起删，这是用户最需要被提前告知的东西。
	writeFile(t, filepath.Join(f.clean, "workdir", "uncommitted.bin"), 512)

	f.keep = createTaskDir(t, d.cfg.WorkspacesRoot, "ws-plan", "keep-task", &execenv.GCMeta{
		Kind:        execenv.GCKindIssue,
		IssueID:     gcPlanOpenIssueID,
		WorkspaceID: "ws-plan",
		CompletedAt: time.Now(),
	})
	writeFile(t, filepath.Join(f.keep, "workdir", "keep.bin"), 256)

	// 没有 meta：GC 只能看 mtime，TTL 归零后判 orphan。
	f.orphan = createTaskDir(t, d.cfg.WorkspacesRoot, "ws-plan", "orphan-task", nil)
	writeFile(t, filepath.Join(f.orphan, "scratch.txt"), 128)

	// 父记录同样是已终态的 done，唯一的区别是有任务正在用这个目录——这正是预览要
	// 显示成「正在运行」而不是「会被删」的那一条。
	f.active = createTaskDir(t, d.cfg.WorkspacesRoot, "ws-plan", "active-task", &execenv.GCMeta{
		Kind:        execenv.GCKindIssue,
		IssueID:     gcPlanDoneIssueID,
		WorkspaceID: "ws-plan",
		CompletedAt: time.Now().Add(-10 * 24 * time.Hour),
	})
	d.markActiveEnvRoot(f.active)
	writeFile(t, filepath.Join(f.active, "workdir", "live.bin"), 64)

	return f
}

func (f gcPlanFixture) wantActions() map[string]string {
	return map[string]string{
		f.clean:  GCPlanActionClean,
		f.keep:   GCPlanActionKeep,
		f.orphan: GCPlanActionOrphan,
		f.active: GCPlanActionActive,
	}
}

// countTree 给一棵目录树做指纹：目录数、文件数、总字节数。
func countTree(t *testing.T, root string) (dirs, files int, bytes int64) {
	t.Helper()
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			dirs++
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return dirs, files, bytes
}

// TestPlanGC_DeletesNothing 是这条能力的核心保证：预览只读。
//
// 断言目录树指纹不变，而不是只断言「目录还在」——后者漏得掉「删了里面几个文件又没删
// 干净」这一类最危险的半成品行为。
func TestPlanGC_DeletesNothing(t *testing.T) {
	d := newGCPlanTestDaemon(t)
	f := newGCPlanFixture(t, d)

	beforeDirs, beforeFiles, beforeBytes := countTree(t, d.cfg.WorkspacesRoot)

	report, err := d.PlanGC(context.Background())
	if err != nil {
		t.Fatalf("PlanGC: %v", err)
	}

	afterDirs, afterFiles, afterBytes := countTree(t, d.cfg.WorkspacesRoot)
	if afterDirs != beforeDirs || afterFiles != beforeFiles || afterBytes != beforeBytes {
		t.Fatalf("PlanGC mutated the tree: dirs %d→%d, files %d→%d, bytes %d→%d",
			beforeDirs, afterDirs, beforeFiles, afterFiles, beforeBytes, afterBytes)
	}

	got := map[string]string{}
	for _, entry := range report.Entries {
		got[entry.Path] = entry.Action
	}
	want := f.wantActions()
	for path, action := range want {
		if got[path] != action {
			t.Errorf("plan for %s = %q, want %q", filepath.Base(path), got[path], action)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("plan covered %d dirs, want %d: %+v", len(got), len(want), got)
	}

	// 目录本身还得在——指纹相同但换了一批文件的情况也要排除。
	for _, path := range []string{f.clean, f.keep, f.orphan, f.active} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("task dir should survive a preview: %v", err)
		}
	}
	if _, err := os.Stat(f.wsDir); err != nil {
		t.Errorf("workspace dir should survive a preview: %v", err)
	}
}

// TestPlanGC_PredictsRealGC 守「预览不撒谎」。
//
// 同一个 fixture 上先问预览，再真跑一次 GC，逐条核对。任何一条对不上都说明预览与真实
// GC 已经开始分叉——那时候预览比没有预览更危险。
func TestPlanGC_PredictsRealGC(t *testing.T) {
	d := newGCPlanTestDaemon(t)
	f := newGCPlanFixture(t, d)

	report, err := d.PlanGC(context.Background())
	if err != nil {
		t.Fatalf("PlanGC: %v", err)
	}
	if len(report.Entries) != 4 {
		t.Fatalf("plan covered %d dirs, want 4", len(report.Entries))
	}

	stats := &gcStats{byPattern: map[string]int{}}
	d.gcWorkspace(context.Background(), f.wsDir, stats, d.realGCApply(stats))

	for _, entry := range report.Entries {
		_, statErr := os.Stat(entry.Path)
		survived := statErr == nil
		switch entry.Action {
		case GCPlanActionClean, GCPlanActionOrphan:
			if survived {
				t.Errorf("preview promised removal of %s (%s) but it survived", filepath.Base(entry.Path), entry.Action)
			}
		default:
			if !survived {
				t.Errorf("preview promised %s would be kept (%s) but real GC removed it", filepath.Base(entry.Path), entry.Action)
			}
		}
	}
	if stats.cleaned+stats.orphaned != 2 {
		t.Fatalf("real GC removed %d dirs (cleaned=%d orphaned=%d), want 2",
			stats.cleaned+stats.orphaned, stats.cleaned, stats.orphaned)
	}
}

// TestGcWorkspace_LeavesWorkspaceDirInPlace 守一个被重构牵出来的不变式。
//
// 此前「工作区目录空了就删掉」写在 gcWorkspace 里。现在 gcWorkspace 被预览共用，那段
// 代码就得搬走——否则预览在列完清单之后会顺手删掉工作区目录，而「预览不删东西」是这
// 条能力的全部意义。删空目录归 runGC（见 gc_test.go 里那条用例）。
func TestGcWorkspace_LeavesWorkspaceDirInPlace(t *testing.T) {
	d := newGCPlanTestDaemon(t)
	f := newGCPlanFixture(t, d)

	stats := &gcStats{byPattern: map[string]int{}}
	cleaned := d.gcWorkspace(context.Background(), f.wsDir, stats, d.realGCApply(stats))
	if cleaned != 2 {
		t.Fatalf("cleaned = %d, want 2", cleaned)
	}
	if _, err := os.Stat(f.wsDir); err != nil {
		t.Fatalf("gcWorkspace must not remove the workspace dir (the preview shares this walk): %v", err)
	}
}

// TestPlanGC_Summary 守汇总数字与排序：用户真正看的是「什么会没、能腾多少」。
func TestPlanGC_Summary(t *testing.T) {
	d := newGCPlanTestDaemon(t)
	f := newGCPlanFixture(t, d)

	report, err := d.PlanGC(context.Background())
	if err != nil {
		t.Fatalf("PlanGC: %v", err)
	}

	if report.Scope == "" {
		t.Error("scope must be stated in the output: 没列出来的东西很容易被读成不会清理")
	}
	if report.TotalTaskDirs != 4 {
		t.Errorf("TotalTaskDirs = %d, want 4", report.TotalTaskDirs)
	}
	if report.WillRemoveDirs != 2 {
		t.Errorf("WillRemoveDirs = %d, want 2", report.WillRemoveDirs)
	}
	if report.ActiveDirs != 1 {
		t.Errorf("ActiveDirs = %d, want 1", report.ActiveDirs)
	}
	if report.WillRemoveBytes <= 0 {
		t.Errorf("WillRemoveBytes = %d, want > 0", report.WillRemoveBytes)
	}

	byPath := map[string]GCPlanEntry{}
	for _, entry := range report.Entries {
		byPath[entry.Path] = entry
	}
	// 整目录删除的预计释放就是这个目录的大小——它连未提交的工作一起删，不能少报。
	for _, path := range []string{f.clean, f.orphan} {
		entry := byPath[path]
		if entry.SizeBytes <= 0 {
			t.Errorf("%s: SizeBytes = %d, want > 0", filepath.Base(path), entry.SizeBytes)
		}
		if entry.RemovableBytes != entry.SizeBytes {
			t.Errorf("%s: RemovableBytes = %d, want SizeBytes = %d",
				filepath.Base(path), entry.RemovableBytes, entry.SizeBytes)
		}
		if entry.Reason == "" {
			t.Errorf("%s: reason must say why", filepath.Base(path))
		}
	}
	// 会删的排在会留的前面。
	if report.Entries[0].Action == GCPlanActionKeep || report.Entries[0].Action == GCPlanActionActive {
		t.Errorf("first entry should be one that changes, got %q", report.Entries[0].Action)
	}
}

// TestPlanGC_ArtifactActionWithNothingToReclaim 来自真机输出。
//
// 第一次在真实工作区上跑预览时，22 个目录里有 13 个被判成 clean_artifacts，而预计释放
// 全是 0——这些目录里根本没有 node_modules 那类可再生产物。照抄决策会把它们显示成
// 「会清产物」，于是用户真正想看的「什么会没」被一批什么都不做的行淹没了。预览要说
// 净效果：没有可删的东西就等于这一轮不会动。
func TestPlanGC_ArtifactActionWithNothingToReclaim(t *testing.T) {
	d := newGCPlanTestDaemon(t) // GCArtifactTTL = 12h，GCTTL = 5d

	// 父 Issue 仍在进行（不终态），但任务本身已完成超过产物 TTL → 判「只清产物」。
	// 两个目录共用同一个 Issue，区别只是有没有可再生产物。
	bare := createTaskDir(t, d.cfg.WorkspacesRoot, "ws-plan", "artifact-none", &execenv.GCMeta{
		Kind:        execenv.GCKindIssue,
		IssueID:     gcPlanOpenIssueID,
		WorkspaceID: "ws-plan",
		CompletedAt: time.Now().Add(-20 * time.Hour),
	})
	withArtifacts := createTaskDir(t, d.cfg.WorkspacesRoot, "ws-plan", "artifact-has", &execenv.GCMeta{
		Kind:        execenv.GCKindIssue,
		IssueID:     gcPlanOpenIssueID,
		WorkspaceID: "ws-plan",
		CompletedAt: time.Now().Add(-20 * time.Hour),
	})
	writeFile(t, filepath.Join(withArtifacts, "node_modules", "leftover.js"), 2048)

	report, err := d.PlanGC(context.Background())
	if err != nil {
		t.Fatalf("PlanGC: %v", err)
	}
	byPath := map[string]GCPlanEntry{}
	for _, entry := range report.Entries {
		byPath[entry.Path] = entry
	}

	if got := byPath[bare].Action; got != GCPlanActionKeep {
		t.Errorf("dir with no reclaimable artifacts = %q, want %q (%s)",
			got, GCPlanActionKeep, byPath[bare].Reason)
	}
	if byPath[bare].Reason == "" {
		t.Error("kept-because-nothing-to-reclaim must say why")
	}
	if got := byPath[withArtifacts].Action; got != GCPlanActionCleanArtifacts {
		t.Errorf("dir with artifacts = %q, want %q", got, GCPlanActionCleanArtifacts)
	}
	if byPath[withArtifacts].RemovableBytes <= 0 {
		t.Errorf("RemovableBytes = %d, want > 0", byPath[withArtifacts].RemovableBytes)
	}
	if report.WillTrimArtifacts != 1 || report.WillTrimArtifactBytes <= 0 {
		t.Errorf("summary = %d dirs / %d bytes, want 1 dir / > 0 bytes",
			report.WillTrimArtifacts, report.WillTrimArtifactBytes)
	}
}

// TestPlanGC_MissingRoot 对齐 runGC 的行为：工作区根目录不存在不是错误。
//
// 但空计划必须带上 scope——否则「什么都没列」会被读成「什么都不会被清理」。
func TestPlanGC_MissingRoot(t *testing.T) {
	d := newGCPlanTestDaemon(t)
	d.cfg.WorkspacesRoot = filepath.Join(t.TempDir(), "does-not-exist")

	report, err := d.PlanGC(context.Background())
	if err != nil {
		t.Fatalf("missing root should not be an error (runGC treats it as nothing to do): %v", err)
	}
	if len(report.Entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(report.Entries))
	}
	if report.Scope == "" {
		t.Error("empty plan must still state its scope")
	}
}

// TestGCPlanHandler_GetOnly 守 HTTP 面：预览按定义是只读的，不能给它可写的方法。
func TestGCPlanHandler_GetOnly(t *testing.T) {
	d := newGCPlanTestDaemon(t)
	_ = newGCPlanFixture(t, d)
	handler := d.gcPlanHandler()

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/gc-plan", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /gc-plan = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Errorf("Allow = %q, want GET", got)
	}

	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/gc-plan", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /gc-plan = %d, want 200", rec.Code)
	}
	var decoded GCPlanReport
	if err := json.NewDecoder(rec.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode /gc-plan response: %v", err)
	}
	if len(decoded.Entries) != 4 {
		t.Fatalf("decoded entries = %d, want 4", len(decoded.Entries))
	}
}
