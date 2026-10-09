package daemon

// Ruel 新增：四类历史的保留策略（P0-9 的另一半）。
//
// gc-plan 回答「下一轮会删什么」，本文件回答「各类东西能活多久」。两个问题、两条命令，
// 刻意不合并——合并后两边都变模糊：一个命令里既有「这次会删」又有「一般能活多久」，
// 用户得分清哪一列是预测、哪一列是配置，而这两列恰好长得最像。
//
// 唯一硬性约束：**展示的必须是生效值，不是默认值**。这些 TTL 全都能被 MULTICA_GC_* 环境
// 变量覆盖，展示默认值等于撒谎——用户照着它判断「我的东西还能活多久」，拿到的是另一套
// 配置。所以这一路数据全部取自 daemon 已经装载好的 d.cfg，而不是常量表。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

// GCPolicyScope 说明这份报告覆盖什么、不覆盖什么。与 gc-plan 同一条原则：「没列出来」
// 很容易被读成「不会清理」，所以边界写在输出里而不是藏在文档里。
const GCPolicyScope = "本机 daemon 侧的四类历史 + 缓存类；服务端记录不在本机，其保留策略由服务端决定，这里只标注「本机无从得知」而不是给个数"

// GCPolicyEntry 是一类历史的保留策略与当前占用。
type GCPolicyEntry struct {
	// Class 是稳定标识，给 JSON 消费者用；Label 是给人看的中文名。
	Class string `json:"class"`
	Label string `json:"label"`

	// Reclaimable 是「能不能被清」。TTL 为 0 的类是**显式禁用**，不是「永不清」——
	// 差别在于：前者是有人把它关了，后者是从来没人管过。Disabled 记录前者。
	Reclaimable bool `json:"reclaimable"`
	Disabled    bool `json:"disabled"`

	// Retention 是**生效**的保留时长，空串表示这一类没有保留策略（不是 0）。
	Retention string `json:"retention"`
	// EnvVar 是覆盖它用的环境变量名，空串表示没有对应的开关。
	EnvVar string `json:"env_var,omitempty"`
	// Overridden 表示这个环境变量当前**被显式设置过**。展示值是否可信全看这一位：
	// 没被覆盖时展示的是默认值，那正是本文件要防的「拿默认值當生效值」。
	Overridden bool `json:"overridden"`

	// SizeBytes / ItemCount 是当前占用。SizeKnown 为 false 时前两者无意义——**量不出来
	// 必须和「量出来是 0」分开**，否则一个读不到的目录会被显示成空的。
	SizeBytes  int64 `json:"size_bytes"`
	SizeKnown  bool  `json:"size_known"`
	ItemCount  int   `json:"item_count"`
	Measurable bool  `json:"measurable"`

	// Protected 是这一类里当前**受保护**的条目数：正在跑的任务目录、正在被挂载的仓库
	// 缓存。它们不会被下一轮 GC 动——不标出来的话，用户会以为「随时可能被删」。
	Protected int    `json:"protected"`
	Note      string `json:"note,omitempty"`
}

// GCPolicyReport 是一份保留策略快照。
type GCPolicyReport struct {
	GeneratedAt  time.Time       `json:"generated_at"`
	Scope        string          `json:"scope"`
	Profile      string          `json:"profile"`
	GCEnabled    bool            `json:"gc_enabled"`
	GCEnabledVar string          `json:"gc_enabled_var"`
	GCInterval   string          `json:"gc_interval"`
	Entries      []GCPolicyEntry `json:"entries"`
	TotalBytes   int64           `json:"total_bytes"`
}

// PolicyGC 汇总四类历史与缓存类的生效保留策略和当前占用。
//
// 遍历与统计**复用真实 GC 用的那几棵树**（Prune* 的根由 execenv 导出），不另拼路径。
// gc-plan 那条「预览必须走与真实 GC 同一条判断路径」在这里同样成立：报告与真实行为分叉，
// 比没有报告更糟。
func (d *Daemon) PolicyGC(ctx context.Context) (GCPolicyReport, error) {
	report := GCPolicyReport{
		GeneratedAt:  time.Now().UTC(),
		Scope:        GCPolicyScope,
		Profile:      d.cfg.Profile,
		GCEnabled:    d.cfg.GCEnabled,
		GCEnabledVar: "MULTICA_GC_ENABLED",
		GCInterval:   d.cfg.GCInterval.String(),
		Entries:      []GCPolicyEntry{},
	}
	if !d.cfg.GCEnabled {
		// 总开关关了，各类 TTL 就都不生效——这一条必须放在最前面说，否则下面每一行
		// 「24h 后可清」都在撒谎。
		report.Entries = append(report.Entries, GCPolicyEntry{
			Class: "gc_disabled",
			Label: "GC 总开关",
			Note:  "MULTICA_GC_ENABLED=false：以下所有 TTL 都不生效，什么都不会被自动清理",
		})
	}

	// ---- 工作目录 ----
	plan, err := d.PlanGC(ctx)
	if err != nil {
		return report, err
	}
	var dirBytes int64
	for _, e := range plan.Entries {
		dirBytes += e.SizeBytes
	}
	report.Entries = append(report.Entries, GCPolicyEntry{
		Class:       "workdir",
		Label:       "工作目录",
		Reclaimable: d.cfg.GCTTL > 0 || d.cfg.GCOrphanTTL > 0 || d.cfg.GCCompletedTaskTTL > 0,
		Disabled:    d.cfg.GCTTL <= 0 && d.cfg.GCOrphanTTL <= 0 && d.cfg.GCCompletedTaskTTL <= 0,
		Retention:   fmt.Sprintf("终态后 %s；孤儿 %s；已完成任务 %s", d.cfg.GCTTL, d.cfg.GCOrphanTTL, d.cfg.GCCompletedTaskTTL),
		EnvVar:      "MULTICA_GC_TTL / MULTICA_GC_ORPHAN_TTL / MULTICA_GC_COMPLETED_TASK_TTL",
		Overridden:  anyEnvSet("MULTICA_GC_TTL", "MULTICA_GC_ORPHAN_TTL", "MULTICA_GC_COMPLETED_TASK_TTL"),
		SizeBytes:   dirBytes,
		SizeKnown:   true,
		ItemCount:   plan.TotalTaskDirs,
		Measurable:  true,
		Protected:   plan.ActiveDirs,
		Note:        "唯一装着用户工作的一类：整目录删除会连未提交的工作一起删掉",
	})

	// ---- CLI 会话 ----
	codexRoot := execenv.CodexSessionStoreRoot(d.cfg.Profile)
	codexSize, codexCount := storeFootprint(codexRoot, 2)
	hermesSessRoot, sessOK := execenv.HermesSessionStoreRoot(d.cfg.Profile)
	sessSize, sessCount := storeFootprint(hermesSessRoot, 3)
	report.Entries = append(report.Entries, GCPolicyEntry{
		Class:       "cli_session",
		Label:       "CLI 会话",
		Reclaimable: d.cfg.GCCodexSessionTTL > 0 || d.cfg.GCHermesSessionTTL > 0,
		Disabled:    d.cfg.GCCodexSessionTTL <= 0 && d.cfg.GCHermesSessionTTL <= 0,
		Retention:   fmt.Sprintf("Codex %s；Hermes %s", d.cfg.GCCodexSessionTTL, d.cfg.GCHermesSessionTTL),
		EnvVar:      "MULTICA_GC_CODEX_SESSION_TTL / MULTICA_GC_HERMES_SESSION_TTL",
		Overridden:  anyEnvSet("MULTICA_GC_CODEX_SESSION_TTL", "MULTICA_GC_HERMES_SESSION_TTL"),
		SizeBytes:   codexSize + sessSize,
		SizeKnown:   sessOK,
		ItemCount:   codexCount + sessCount,
		Measurable:  true,
		Note:        "删了是可恢复的：会话重启而不是失忆，重新登录即可",
	})

	// ---- 经批准知识（执行机上的 Hermes 文件记忆）----
	//
	// 注意 Label 特意带了「执行机文件记忆」这个限定语，见下面 ruel_project_knowledge
	// 那条：PRD 5.3 给第三类保留起的名字正是「经审阅的项目知识」，而这一类扫的是
	// hermes-state/<agent>/<profile>/memories/，是 provider 原生记忆，不是项目知识。
	// 两者同名不同物，所以两行必须互相点明，否则没人分得清删的是哪个。
	memRoot, memOK := execenv.HermesMemoryStoreRoot(d.cfg.Profile)
	memSize, memCount := storeFootprint(memRoot, 2)
	report.Entries = append(report.Entries, GCPolicyEntry{
		Class:       "approved_knowledge",
		Label:       "经批准知识（执行机文件记忆）",
		Reclaimable: d.cfg.GCHermesMemoryTTL > 0,
		Disabled:    d.cfg.GCHermesMemoryTTL <= 0,
		Retention:   d.cfg.GCHermesMemoryTTL.String(),
		EnvVar:      "MULTICA_GC_HERMES_MEMORY_TTL",
		Overridden:  anyEnvSet("MULTICA_GC_HERMES_MEMORY_TTL"),
		SizeBytes:   memSize,
		SizeKnown:   memOK,
		ItemCount:   memCount,
		Measurable:  true,
		Note:        "删掉是可见的失忆，所以默认最长（90 天）。本类只扫执行机上的 Hermes 文件记忆，与「项目知识（服务端，经人审阅）」不是一回事",
	})

	// ---- 项目知识（服务端，经人审阅）----
	//
	// PRD 5.3 第三类保留的落点，存储是服务端的 ruel_project_knowledge 表（迁移 565）。
	//
	// 为什么**不给 TTL**：表在服务端，daemon 既看不到它也删不动它，而服务端目前
	// 没有任何保留/清理机制（全库搜 retention 只命中 daemon 侧）。这与上面几类
	// 的处理原则是同一条：**展示的必须是生效值**。给个 90 天会让用户以为有人会去删，
	// 而实际上没有——那比不给数更糟。
	//
	// 为什么**不并进 server_record**：那一类是泛泛的「服务端记录」，没有具体策略；
	// 这一类是 PRD 点名的保留类，且必须与 approved_knowledge 分开显示才看得出两者
	// 不是一回事。合并进 server_record 会让「项目知识」这个名字从报告里消失。
	report.Entries = append(report.Entries, GCPolicyEntry{
		Class:      "ruel_project_knowledge",
		Label:      "项目知识（服务端，经人审阅）",
		Retention:  "",
		SizeKnown:  false,
		Measurable: false,
		Note:       "与上面的「经批准知识（执行机文件记忆）」不是同一类：那一类落在 Agent 执行机的文件系统上，这一类落在服务端数据库里。本机既看不到也删不动，保留策略由服务端决定，本命令不给数",
	})

	// ---- 缓存类：bare repo 缓存 + 临时目录 ----
	usage, _ := ScanDiskUsage(d.cfg.WorkspacesRoot, d.cfg.GCArtifactPatterns)
	report.Entries = append(report.Entries, GCPolicyEntry{
		Class:       "cache",
		Label:       "缓存（仓库 / 临时目录）",
		Reclaimable: d.cfg.GCRepoTTL > 0 || d.cfg.GCTaskTempLegacyTTL > 0,
		Disabled:    d.cfg.GCRepoTTL <= 0 && d.cfg.GCTaskTempLegacyTTL <= 0,
		Retention:   fmt.Sprintf("仓库缓存 %s；历史临时目录 %s", d.cfg.GCRepoTTL, d.cfg.GCTaskTempLegacyTTL),
		EnvVar:      "MULTICA_GC_REPO_TTL / MULTICA_GC_TASK_TEMP_LEGACY_TTL",
		Overridden:  anyEnvSet("MULTICA_GC_REPO_TTL", "MULTICA_GC_TASK_TEMP_LEGACY_TTL"),
		SizeBytes:   usage.RepoCacheSizeBytes,
		SizeKnown:   true,
		ItemCount:   usage.RepoCacheCount,
		Measurable:  true,
		Note:        "临时目录一栏是 0 表示「按存活回收、不看年龄」，不是「不回收」",
	})

	// ---- 服务端记录 ----
	// 这一类**没有**对应的本机保留策略，也不在本机占地方。显式写出来，而不是省略——
	// 省略会被读成「不会清理」，而真相是「本机无从得知」。
	report.Entries = append(report.Entries, GCPolicyEntry{
		Class:      "server_record",
		Label:      "服务端记录",
		Retention:  "",
		SizeKnown:  false,
		Measurable: false,
		Note:       "Run / 评论 / 事件留在服务端，由服务端决定保留多久；daemon 侧无从得知，本命令不给数",
	})

	for _, e := range report.Entries {
		if e.SizeKnown {
			report.TotalBytes += e.SizeBytes
		}
	}
	return report, nil
}

// anyEnvSet 判断这些环境变量里有没有一个被**显式设置过**。
//
// 只看「有没有设置」，不看「值是不是等于默认值」：把环境变量设成和默认值一样，语义上仍然
// 是「这个人明确要这个值」，而更重要的是——只有这一位能区分「展示的是默认值」与「展示的
// 是生效值」，本文件开头那条约束全靠它。
func anyEnvSet(names ...string) bool {
	for _, n := range names {
		if _, ok := os.LookupEnv(n); ok {
			return true
		}
	}
	return false
}

// storeFootprint 量一棵存储树的占地与条目数。depth 是叶子的层级（codex 是 agent/issue
// 两层，hermes 会话多一层 conversation）。
//
// 读不到就返回 0 —— 调用方要用 SizeKnown 区分「量出来是 0」与「量不出来」，这一位不能
// 在这里被抹掉。
func storeFootprint(root string, depth int) (size int64, count int) {
	if root == "" {
		return 0, 0
	}
	var walk func(dir string, level int)
	walk = func(dir string, level int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			child := filepath.Join(dir, e.Name())
			if level == depth {
				count++
				size += dirSize(child)
				continue
			}
			walk(child, level+1)
		}
	}
	walk(root, 1)
	return size, count
}
