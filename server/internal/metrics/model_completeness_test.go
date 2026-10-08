package metrics

// Ruel 新增：#34 —— 模型名采集完整性指标。
//
// 三条纪律，各有用例：
//  1. 占位值的判定要覆盖已知写法，且**不能误伤真实模型 id**（`auto` 那条最容易踩）。
//  2. 分母是「落到库里的用量行」，分子是其中模型名为占位值的行。两个数字都要准。
//  3. 反向验证：用 #32 修复**前**的真实数据喂进去，指标必须能看出来；修好后必须看不出来。
//     没有这一条，本指标只是一段自洽的代码，没证明它能抓到那次真实事故。

import "testing"

func TestIsPlaceholderModelCoversKnownSpellings(t *testing.T) {
	placeholders := []string{"", "unknown", "Unknown", "UNKNOWN", "  unknown  "}
	for _, m := range placeholders {
		if !IsPlaceholderModel(m) {
			t.Errorf("IsPlaceholderModel(%q) = false, want true（这是已知的占位写法）", m)
		}
	}
}

// TestIsPlaceholderModelKeepsRealModelIDs 守的是「宁可漏报，不可误报」的那一半。
//
// `auto` 尤其要单独钉住：它是 codex 的通用模型 id，在用量落地处被用来反查 provider。
// 一旦把它当成占位值，codex 的**每一条正常用量**都会报警，这个指标就废了。
func TestIsPlaceholderModelKeepsRealModelIDs(t *testing.T) {
	real := []string{"gpt-5.6-sol", "glm-5", "auto", "claude-sonnet-4-5", "unknown-model"}
	for _, m := range real {
		if IsPlaceholderModel(m) {
			t.Errorf("IsPlaceholderModel(%q) = true, want false（这是真实模型 id，误报会让指标失效）", m)
		}
	}
}

func TestModelCompletenessCountsPerProvider(t *testing.T) {
	m := &ModelCompleteness{}
	m.Observe("codex", "gpt-5.6-sol")
	m.Observe("codex", "unknown")
	m.Observe("claude", "claude-sonnet-4-5")

	snap := m.Snapshot()
	if len(snap.Providers) != 2 {
		t.Fatalf("provider 数 = %d, want 2", len(snap.Providers))
	}
	byProvider := map[string]ProviderModelCompleteness{}
	for _, p := range snap.Providers {
		byProvider[p.Provider] = p
	}
	codex := byProvider["codex"]
	if codex.Rows != 2 || codex.MissingRows != 1 {
		t.Errorf("codex = %d 行 / %d 缺，want 2 / 1", codex.Rows, codex.MissingRows)
	}
	if ratio, ok := codex.MissingRatio(); !ok || ratio != 0.5 {
		t.Errorf("codex 缺失占比 = %v (ok=%v), want 0.5", ratio, ok)
	}
	if claude := byProvider["claude"]; claude.Rows != 1 || claude.MissingRows != 0 {
		t.Errorf("claude = %d 行 / %d 缺，want 1 / 0", claude.Rows, claude.MissingRows)
	}
	// 合计是**行数相加**，不是各 provider 占比的平均——那样会把一条样本的行和一万条
	// 样本的行等权。
	if snap.Totals.Rows != 3 || snap.Totals.MissingRows != 1 {
		t.Errorf("合计 = %d 行 / %d 缺，want 3 / 1", snap.Totals.Rows, snap.Totals.MissingRows)
	}
}

// TestModelCompletenessRatioUnknownWithoutRows 守的是「算不出来」不能伪装成「没缺失」。
//
// 0% 会被读成「这个 provider 采得很全」，而真相是「还没见过它的行」。
func TestModelCompletenessRatioUnknownWithoutRows(t *testing.T) {
	var p ProviderModelCompleteness
	if ratio, ok := p.MissingRatio(); ok || ratio != 0 {
		t.Errorf("零行时 MissingRatio() = %v (ok=%v), want 0 (ok=false)", ratio, ok)
	}
}

// TestModelCompletenessSeesTheCodexDefect 是 #34 的**反向验证**。
//
// 喂进去的是 #32 修复前的真实数据：本机那 3 条 codex 用量，模型名全是字面量 `unknown`
// （input tokens 分别 28 833 / 15 324 / 16 339）。它们当时在界面上和「价目表不认识这个
// 模型」长得一模一样，靠算账单才撞见。
//
// 本用例要证明的是：有了这个指标，那次事故不用算账单就能看见。
func TestModelCompletenessSeesTheCodexDefect(t *testing.T) {
	m := &ModelCompleteness{}

	// 修复前：3 条 codex 用量（input tokens 分别 28 833 / 15 324 / 16 339），模型名全灭。
	// token 数在本指标里不参与计算，记在这里只是为了说明这批样本就是 #32 那三条。
	for i := 0; i < 3; i++ {
		m.Observe("codex", "unknown")
	}
	// 同期 claude 侧正常，用来确认指标不是「全都报警」。
	m.Observe("claude", "claude-sonnet-4-5")

	snap := m.Snapshot()
	var codex *ProviderModelCompleteness
	for i := range snap.Providers {
		if snap.Providers[i].Provider == "codex" {
			codex = &snap.Providers[i]
		}
	}
	if codex == nil {
		t.Fatal("快照里没有 codex")
	}
	if codex.Rows != 3 || codex.MissingRows != 3 {
		t.Fatalf("codex = %d 行 / %d 缺，want 3 / 3（#32 修复前是 3/3 全灭）", codex.Rows, codex.MissingRows)
	}
	if ratio, ok := codex.MissingRatio(); !ok || ratio != 1.0 {
		t.Errorf("codex 缺失占比 = %v (ok=%v), want 1.0", ratio, ok)
	}

	// 修复后：同样 3 条，模型名变成真实值 —— 指标必须立刻转干净。
	m.Reset()
	for i := 0; i < 3; i++ {
		m.Observe("codex", "gpt-5.6-sol")
	}
	snap = m.Snapshot()
	if snap.Providers[0].MissingRows != 0 {
		t.Errorf("修复后 codex 缺失 = %d, want 0", snap.Providers[0].MissingRows)
	}
}
