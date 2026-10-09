package daemon

// Ruel 新增：保留策略报告的用例。
//
// 这一条要守的是本文件存在的理由：**展示的必须是生效值，不是默认值**。所有 TTL 都能被
// MULTICA_GC_* 覆盖，展示默认值等于撒谎——用户照着它判断「我的东西还能活多久」，拿到的
// 是另一套配置。所以这里的核心用例是「改一个环境变量，展示值必须跟着变」。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestPolicyGC_ShowsEffectiveValueNotDefault 是本条验收判据的直接对应。
//
// 两半，缺一半都不算证明：
//  1. 没设环境变量时，展示的是默认值，**并且要标出「默认值」**——不标的话用户无法知道
//     这个数字是不是自己那台机器的。
//  2. 设了环境变量后，展示值必须跟着变，来源标成「环境变量」。
//
// 只做第 2 半是不够的：如果实现其实是从常量表读的，只要环境变量恰好被 LoadConfig 吃进
// cfg，第 2 半照样会过。只有两半一起才锁住「读的是 cfg」这件事。
func TestPolicyGC_ShowsEffectiveValueNotDefault(t *testing.T) {
	const env = "MULTICA_GC_CODEX_SESSION_TTL"

	d := newGCPlanTestDaemon(t)
	d.cfg.WorkspacesRoot = t.TempDir()

	// 第 1 半：报告读的是 cfg，不是常量表。把 cfg 改成一个谁都不会当默认值的数，
	// 展示值必须跟着走。若实现其实是从 DefaultGCCodexSessionTTL 读的，这一半会红。
	d.cfg.GCCodexSessionTTL = 7 * 24 * time.Hour
	report, err := d.PolicyGC(context.Background())
	if err != nil {
		t.Fatalf("PolicyGC: %v", err)
	}
	entry := policyEntryFor(t, report, "cli_session")
	if !strings.Contains(entry.Retention, (7 * 24 * time.Hour).String()) {
		t.Fatalf("Retention = %q，应反映 cfg 里的 168h0m0s。展示默认值等于撒谎", entry.Retention)
	}
	// 来源标注要与「环境变量到底有没有被设置」一致，不能靠猜。
	if _, set := os.LookupEnv(env); entry.Overridden != set {
		t.Fatalf("Overridden = %v，但 %s 当前是否设置为 %v", entry.Overridden, env, set)
	}

	// 第 2 半：环境变量 → cfg → 报告，整条链必须通。
	t.Setenv(env, "1h")
	cfg2, err := LoadConfig(Overrides{})
	if err != nil {
		t.Fatalf("LoadConfig after override: %v", err)
	}
	if cfg2.GCCodexSessionTTL != time.Hour {
		t.Fatalf("LoadConfig 没吃到 %s=1h：got %v", env, cfg2.GCCodexSessionTTL)
	}
	d.cfg.GCCodexSessionTTL = cfg2.GCCodexSessionTTL

	report2, err := d.PolicyGC(context.Background())
	if err != nil {
		t.Fatalf("PolicyGC after override: %v", err)
	}
	entry2 := policyEntryFor(t, report2, "cli_session")
	if !entry2.Overridden {
		t.Errorf("设置了 %s 后 Overridden = false，来源应标为环境变量", env)
	}
	if !strings.Contains(entry2.Retention, "1h") {
		t.Errorf("Retention = %q；应反映生效值 1h", entry2.Retention)
	}
}

// TestPolicyGC_ServerRecordsAreExplicitlyUnknown 守「量不出来」不能被显示成 0。
//
// 服务端记录不在本机，既没有保留策略也没有可统计的占用。省略它会被读成「不会清理」，
// 显示 0 会被读成「空的」——两者都是假的，所以必须显式写「本机不知」。
func TestPolicyGC_ServerRecordsAreExplicitlyUnknown(t *testing.T) {
	d := newGCPlanTestDaemon(t)
	d.cfg.WorkspacesRoot = t.TempDir()

	report, err := d.PolicyGC(context.Background())
	if err != nil {
		t.Fatalf("PolicyGC: %v", err)
	}
	entry := policyEntryFor(t, report, "server_record")
	if entry.Measurable {
		t.Error("服务端记录被标为可统计；它不在本机，没有可统计的占用")
	}
	if entry.SizeKnown {
		t.Error("服务端记录的 SizeKnown = true；量不出来必须和「量出来是 0」分开")
	}
	if entry.Note == "" {
		t.Error("服务端记录没有说明；静默留空会被读成「不会清理」")
	}
	// 它不能污染合计：把一个「未知」按 0 加进去，等于声称合计是完整的。
	if report.TotalBytes < 0 {
		t.Errorf("TotalBytes = %d, want >= 0", report.TotalBytes)
	}
}

// TestPolicyGC_EveryClassHasARow 守「每一类各一行」，不漏不省。
func TestPolicyGC_EveryClassHasARow(t *testing.T) {
	d := newGCPlanTestDaemon(t)
	d.cfg.WorkspacesRoot = t.TempDir()

	report, err := d.PolicyGC(context.Background())
	if err != nil {
		t.Fatalf("PolicyGC: %v", err)
	}
	for _, class := range []string{"workdir", "cli_session", "approved_knowledge", "ruel_project_knowledge", "cache", "server_record"} {
		if e := policyEntryFor(t, report, class); e.Label == "" {
			t.Errorf("类别 %q 缺 Label", class)
		}
	}
	if report.Scope == "" {
		t.Error("报告必须写明覆盖范围：没列出来的东西会被读成不会清理")
	}
}

// TestPolicyGC_ProjectKnowledgeIsSeparateFromApprovedKnowledge 守 #47 点名的那个坑。
//
// PRD 5.3 给第三类保留起的名字是「经审阅的项目知识」（approved_knowledge_retention），
// 而上游早就有一个 approved_knowledge 类——它扫的是执行机上的
// hermes-state/<agent>/<profile>/memories/，即 Hermes 的文件型长期记忆。
// 两者**同名不同物**：合并或名字相近地并列之后，没人分得清删的是哪个。
//
// 所以三件事要同时成立：
//  1. 各占一行且 Class 不同；
//  2. 项目知识这一类**不编 TTL、不编开关**——它在服务端库里，daemon 既看不到也删不动，
//     而服务端目前没有任何保留机制。给个数字就是撒谎，比不给数更糟；
//  3. 两行的 Note 各自**逐字引用对方的 Label**。只靠 Label 不同不够——用户读的是说明，
//     说明里的引用对不上标签，等于没引用。
func TestPolicyGC_ProjectKnowledgeIsSeparateFromApprovedKnowledge(t *testing.T) {
	d := newGCPlanTestDaemon(t)
	d.cfg.WorkspacesRoot = t.TempDir()

	report, err := d.PolicyGC(context.Background())
	if err != nil {
		t.Fatalf("PolicyGC: %v", err)
	}
	pk := policyEntryFor(t, report, "ruel_project_knowledge")
	ak := policyEntryFor(t, report, "approved_knowledge")

	if pk.Class == ak.Class {
		t.Fatal("项目知识与 approved_knowledge 共用一个 Class；合并之后没人分得清删的是哪个")
	}
	// 它是服务端存储：量不出来必须和「量出来是 0」分开。
	if pk.Measurable {
		t.Error("项目知识被标为可统计；它在服务端库里，daemon 量不到")
	}
	if pk.SizeKnown {
		t.Error("项目知识的 SizeKnown = true；量不出来必须和「量出来是 0」分开")
	}
	if pk.Retention != "" {
		t.Errorf("项目知识给了一个保留时长 %q；服务端没有清理机制，展示一个不存在的策略等于撒谎", pk.Retention)
	}
	if pk.EnvVar != "" {
		t.Errorf("项目知识声明了开关 %q；不存在这个开关", pk.EnvVar)
	}
	if pk.Note == "" {
		t.Fatal("项目知识没有说明；静默留空会被读成「不会清理」")
	}
	// 两行互相点明对方所在的一侧，读者才能把名字相近的两类分开。
	//
	// 查到「逐字引用对方的 Label」这一层，而不是只查关键词。只查关键词的话，
	// 说明里的引用文字和那一行实际显示的标签可以不一致——曾经就是这样：
	// Note 写着「机器文件记忆」而 Label 是「执行机文件记忆」，读者按名字去对
	// 反而对不上。这两行存在的唯一目的就是让人分清彼此，引用必须能被对上。
	if !strings.Contains(pk.Note, ak.Label) {
		t.Errorf("项目知识的说明没有逐字引用经批准知识的标签 %q，读者按名字对不上：%q", ak.Label, pk.Note)
	}
	if !strings.Contains(ak.Note, pk.Label) {
		t.Errorf("经批准知识的说明没有逐字引用项目知识的标签 %q，读者按名字对不上：%q", pk.Label, ak.Note)
	}
}

// TestGCPolicyHandler_GetOnly 守 HTTP 面：这是只读的配置快照。
func TestGCPolicyHandler_GetOnly(t *testing.T) {
	d := newGCPlanTestDaemon(t)
	d.cfg.WorkspacesRoot = t.TempDir()
	handler := d.gcPolicyHandler()

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/gc-policy", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /gc-policy = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET" {
		t.Errorf("Allow = %q, want GET", got)
	}
}

func policyEntryFor(t *testing.T, report GCPolicyReport, class string) GCPolicyEntry {
	t.Helper()
	for _, e := range report.Entries {
		if e.Class == class {
			return e
		}
	}
	t.Fatalf("报告里没有类别 %q（现有：%s）", class, policyClasses(report))
	return GCPolicyEntry{}
}

func policyClasses(report GCPolicyReport) string {
	names := make([]string, 0, len(report.Entries))
	for _, e := range report.Entries {
		names = append(names, e.Class)
	}
	return strings.Join(names, ", ")
}
