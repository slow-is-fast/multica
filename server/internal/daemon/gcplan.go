package daemon

// Ruel 新增：清理预览（P0-9 的「清理有预览」）。
//
// 上游的清理是 daemon 后台自动 GC，CLI 里**连清理命令都没有**——清理自动发生，用户
// 既不能触发，也不能预知。而 GC 一轮会做出五种动作（见 gc.go 的 gcAction），其中
// Clean 与 Orphan 会整个删掉任务目录，也就是 agent 的工作区：**未提交的工作会一起
// 没**。disk-usage 只回答「占多大」，不回答「下一轮会删哪些、为什么」。本文件补后者。
//
// 唯一的硬性约束：**预览必须走与真实 GC 同一条判断路径**。所以这里不重新实现任何
// 决策，而是复用 gcWorkspace 那一趟遍历，只把「应用」这一步换成记录（见 gc.go 里
// gcWorkspace 的 apply 参数）。另写一套判断的预览迟早与真实 GC 分叉，而会撒谎的预览
// 比没有预览更糟——用户照着它判断「这个目录安不安全」，得到的却是另一套逻辑的结论。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// gcPlanScope 说明预览的覆盖范围。写在输出里而不是藏在文档里，是因为「没列出来」很
// 容易被读成「不会清理」。
const gcPlanScope = "只覆盖任务目录；bare repo 缓存、codex / hermes 会话存储、临时目录不在此预览内，它们各自按 TTL 回收"

// 预览里给动作起的名。用字符串而不是 gcAction 的数字，是因为它要跨进程送到 CLI，
// 数字语义一旦重排就全乱了。
const (
	GCPlanActionKeep                  = "keep"
	GCPlanActionClean                 = "clean"
	GCPlanActionOrphan                = "orphan"
	GCPlanActionCleanArtifacts        = "clean_artifacts"
	GCPlanActionCleanManagedArtifacts = "clean_managed_artifacts"
	GCPlanActionActive                = "active"
)

// GCPlanEntry 是一个任务目录在下一轮 GC 里的命运。
type GCPlanEntry struct {
	WorkspaceShort    string `json:"workspace_short"`
	TaskShort         string `json:"task_short"`
	Path              string `json:"path"`
	Kind              string `json:"kind"`
	ParentID          string `json:"parent_id,omitempty"`
	Action            string `json:"action"`
	Reason            string `json:"reason"`
	AgeSeconds        int64  `json:"age_seconds"`
	SizeBytes         int64  `json:"size_bytes"`
	ArtifactSizeBytes int64  `json:"artifact_size_bytes"`
	// RemovableBytes 是这一条动作预计释放的字节数。只清产物的两条用的是可再生产物
	// 子集的大小，与 disk-usage 的 ARTIFACTS 列同一口径——那是上限而不是精确值。
	RemovableBytes int64 `json:"removable_bytes"`
}

// GCPlanReport 是一轮预览的结果。
type GCPlanReport struct {
	GeneratedAt           time.Time     `json:"generated_at"`
	WorkspacesRoot        string        `json:"workspaces_root"`
	Scope                 string        `json:"scope"`
	Entries               []GCPlanEntry `json:"entries"`
	TotalTaskDirs         int           `json:"total_task_dirs"`
	WillRemoveDirs        int           `json:"will_remove_dirs"`
	WillRemoveBytes       int64         `json:"will_remove_bytes"`
	WillTrimArtifacts     int           `json:"will_trim_artifacts"`
	WillTrimArtifactBytes int64         `json:"will_trim_artifact_bytes"`
	ActiveDirs            int           `json:"active_dirs"`
}

// PlanGC 算出下一轮 GC 会做什么，但不做任何事。
//
// 与 runGC 共用 gcWorkspace 的遍历与决策，差异只有两点：apply 只记录不删除，以及不
// 触发 runGC 里那些额外的回收项（repo 缓存、会话存储等）——它们没有对应的只读模式，
// 与其给一个算不准的数字，不如明确写清不在范围内（见 gcPlanScope）。
func (d *Daemon) PlanGC(ctx context.Context) (GCPlanReport, error) {
	root := d.cfg.WorkspacesRoot
	report := GCPlanReport{
		GeneratedAt:    time.Now().UTC(),
		WorkspacesRoot: root,
		Scope:          gcPlanScope,
		Entries:        []GCPlanEntry{},
	}
	if strings.TrimSpace(root) == "" {
		return report, fmt.Errorf("gc-plan: workspaces root is required")
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return report, nil
		}
		return report, fmt.Errorf("gc-plan: read workspaces root: %w", err)
	}

	// 与 disk-usage 用同一个 matcher，保证预览里的「可再生产物」与 GC 真正会删的
	// 是同一批东西。
	matcher := newArtifactMatcher(d.cfg.GCArtifactPatterns, execenv.ManagedReclaimableArtifactSubpaths())
	stats := &gcStats{byPattern: map[string]int{}}

	for _, wsEntry := range entries {
		if ctx.Err() != nil {
			break
		}
		// 与 runGC 同一套跳过规则：非目录、以及 daemon 自己的点开头缓存目录。
		if !wsEntry.IsDir() || strings.HasPrefix(wsEntry.Name(), ".") {
			continue
		}
		wsDir := filepath.Join(root, wsEntry.Name())
		d.gcWorkspace(ctx, wsDir, stats, func(taskDir string, meta *execenv.GCMeta, dec gcDecision) int {
			report.Entries = append(
				report.Entries,
				newGCPlanEntry(taskDir, wsEntry.Name(), meta, dec, matcher),
			)
			// 永远返回 0：预览不清理任何东西，返回值只用来让调用方知道「清理了几个」，
			// 这里必须是 0，否则 gcWorkspace 的调用方会以为可以删掉空的工作区目录。
			return 0
		})
	}

	summarizeGCPlan(&report)
	return report, nil
}

// newGCPlanEntry 把一次决策翻译成一行预览。
//
// 体积直接复用 buildTaskUsage：它与 disk-usage 是同一份实现，用户对着两个命令看到的
// 数字才会一致。两条命令各算一遍的话，迟早有人来问「为什么这两个数不一样」。
func newGCPlanEntry(
	taskDir string,
	workspaceShort string,
	_ *execenv.GCMeta,
	dec gcDecision,
	matcher artifactMatcher,
) GCPlanEntry {
	usage := buildTaskUsage(taskDir, workspaceShort, filepath.Base(taskDir), matcher)
	entry := GCPlanEntry{
		WorkspaceShort:    usage.WorkspaceShort,
		TaskShort:         usage.TaskShort,
		Path:              taskDir,
		Kind:              usage.Kind,
		ParentID:          usage.ParentID,
		AgeSeconds:        usage.AgeSeconds,
		SizeBytes:         usage.SizeBytes,
		ArtifactSizeBytes: usage.ArtifactSizeBytes,
	}
	entry.Action, entry.Reason, entry.RemovableBytes = gcPlanOutcome(dec, entry, hasReclaimableArtifact(taskDir, matcher))
	return entry
}

// hasReclaimableArtifact 回答「这个目录里到底有没有可再生产物可以删」。
//
// 判据是「有没有匹配上」而不是 ArtifactSizeBytes 是否大于 0：一个空的 node_modules
// 体积是 0，但 GC 照样会把它删掉。匹配规则由 matcher 唯一决定，与 taskSize、以及真正
// 动手的 cleanTaskArtifactsMatching 是同一份。
func hasReclaimableArtifact(taskDir string, matcher artifactMatcher) bool {
	if taskDir == "" {
		return false
	}
	absRoot, err := filepath.Abs(taskDir)
	if err != nil {
		return false
	}
	found := false
	_ = filepath.WalkDir(absRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || found || path == absRoot {
			return nil
		}
		// 链接一律不算：GC 也不删链接目标（见 taskSize 的同一条规则）。
		if entry.Type()&linkedDirModes != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if _, ok := matcher.matchDirectory(absRoot, path, entry); ok {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// gcPlanOutcome 把决策翻译成（动作、理由、预计释放）。理由写的是「为什么」，不是
// 「做了什么」——后者由动作本身表达。
//
// hasArtifacts 只影响那两条「只清产物」的动作：GC 判了要清产物，但目录里根本没有可再
// 生产物时，这一轮的净效果就是什么都不会发生。照抄决策把它显示成「会清产物」会污染清
// 单——真机上 22 个目录里有 13 个是这种，用户想看的「什么会没」被它们淹没了。这里说的
// 仍然是净效果，与真实 GC 会做的事一致。
func gcPlanOutcome(dec gcDecision, entry GCPlanEntry, hasArtifacts bool) (action, reason string, removable int64) {
	switch {
	case dec.active:
		return GCPlanActionActive, "正在运行，本轮 GC 会跳过", 0
	case dec.action == gcActionClean:
		return GCPlanActionClean, "父记录已终态，整目录删除（含未提交的工作）", entry.SizeBytes
	case dec.action == gcActionOrphan:
		return GCPlanActionOrphan, "无 meta 或父记录不可识别，且目录已超过孤立 TTL，整目录删除", entry.SizeBytes
	case dec.action == gcActionCleanArtifacts:
		if !hasArtifacts {
			return GCPlanActionKeep, "已过产物 TTL，但目录里没有可再生产物，本轮不会动", 0
		}
		return GCPlanActionCleanArtifacts, "任务已完成足够久，只删可再生产物（目录保留）", entry.ArtifactSizeBytes
	case dec.action == gcActionCleanManagedArtifacts:
		if !hasArtifacts {
			return GCPlanActionKeep, "已过托管产物 TTL，但目录里没有 daemon 托管的产物，本轮不会动", 0
		}
		return GCPlanActionCleanManagedArtifacts, "目录保留，只删 daemon 托管的产物", entry.ArtifactSizeBytes
	default:
		return GCPlanActionKeep, "保留", 0
	}
}

func summarizeGCPlan(report *GCPlanReport) {
	report.TotalTaskDirs = len(report.Entries)
	for i := range report.Entries {
		switch report.Entries[i].Action {
		case GCPlanActionClean, GCPlanActionOrphan:
			report.WillRemoveDirs++
			report.WillRemoveBytes += report.Entries[i].RemovableBytes
		case GCPlanActionCleanArtifacts, GCPlanActionCleanManagedArtifacts:
			report.WillTrimArtifacts++
			report.WillTrimArtifactBytes += report.Entries[i].RemovableBytes
		case GCPlanActionActive:
			report.ActiveDirs++
		}
	}
	// 会动的排前面，再按预计释放降序——用户真正想看的是「什么会没」，不是一份目录清单。
	rank := map[string]int{
		GCPlanActionClean:                 0,
		GCPlanActionOrphan:                1,
		GCPlanActionCleanArtifacts:        2,
		GCPlanActionCleanManagedArtifacts: 3,
		GCPlanActionActive:                4,
		GCPlanActionKeep:                  5,
	}
	sort.SliceStable(report.Entries, func(i, j int) bool {
		a, b := report.Entries[i], report.Entries[j]
		if ra, rb := rank[a.Action], rank[b.Action]; ra != rb {
			return ra < rb
		}
		if a.RemovableBytes != b.RemovableBytes {
			return a.RemovableBytes > b.RemovableBytes
		}
		return a.Path < b.Path
	})
}
