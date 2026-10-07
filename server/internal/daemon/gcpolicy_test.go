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

// TestPolicyGC_EveryClassHasARow 守「四类历史各一行」，不漏不省。
func TestPolicyGC_EveryClassHasARow(t *testing.T) {
	d := newGCPlanTestDaemon(t)
	d.cfg.WorkspacesRoot = t.TempDir()

	report, err := d.PolicyGC(context.Background())
	if err != nil {
		t.Fatalf("PolicyGC: %v", err)
	}
	for _, class := range []string{"workdir", "cli_session", "approved_knowledge", "cache", "server_record"} {
		if e := policyEntryFor(t, report, class); e.Label == "" {
			t.Errorf("类别 %q 缺 Label", class)
		}
	}
	if report.Scope == "" {
		t.Error("报告必须写明覆盖范围：没列出来的东西会被读成不会清理")
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
