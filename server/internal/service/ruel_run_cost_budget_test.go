package service

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/gaterefusal"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// 这一组不碰数据库：闸门真正的判据（配置怎么读、钱怎么算）都能脱离库被测，因为
// db.TaskUsage 是普通结构体，可以直接造。要起库的那一半在 handler 包里。

func TestRunCostBudgetUnsetMeansNoCeiling(t *testing.T) {
	t.Setenv(RunCostBudgetEnvVar, "")
	b := RunCostBudgetFromEnv()
	if b.Configured {
		t.Fatalf("没设环境变量却判定为已配置：%#v。不设上限是**明示**的，不能悄悄按某个默认值去截断别人的 Run", b)
	}
	if b.USD != 0 || b.ParseError != "" {
		t.Fatalf("未配置时应当干净：%#v", b)
	}
}

func TestRunCostBudgetParsesUSD(t *testing.T) {
	t.Setenv(RunCostBudgetEnvVar, "2.5")
	b := RunCostBudgetFromEnv()
	if !b.Configured || b.USD != 2.5 || b.ParseError != "" {
		t.Fatalf("got %#v, want configured 2.5", b)
	}
}

// 读不懂的上限必须**暴露**出来，而不是退化成 0 或无限。退化成 0 会静默截断每一个
// Run，退化成无限会静默放行——两者都比显式报错糟。
func TestRunCostBudgetRejectsGarbageLoudly(t *testing.T) {
	for _, raw := range []string{"abc", "-1", ""} {
		if raw == "" {
			continue // 空串等于没配，单独测过
		}
		t.Setenv(RunCostBudgetEnvVar, raw)
		b := RunCostBudgetFromEnv()
		if b.Configured {
			t.Fatalf("%q 不该被当成已配置：%#v", raw, b)
		}
		if b.ParseError == "" {
			t.Fatalf("%q 读不懂却没有 ParseError，运维看不出自己配错了：%#v", raw, b)
		}
	}
}

// 区分「没配」与「配了 0」：后者是「一分钱都不许花」，是完全不同的意图。
func TestRunCostBudgetDistinguishesZeroFromUnset(t *testing.T) {
	t.Setenv(RunCostBudgetEnvVar, "0")
	b := RunCostBudgetFromEnv()
	if !b.Configured || b.USD != 0 {
		t.Fatalf("配了 0 应当被识别为已配置且上限为 0，got %#v", b)
	}
}

func usageRow(model string, in, out, cr, cw int64) db.TaskUsage {
	return db.TaskUsage{
		Model:            model,
		InputTokens:      in,
		OutputTokens:     out,
		CacheReadTokens:  cr,
		CacheWriteTokens: cw,
	}
}

// 本机一条真实用量行（glm-5：input $1.00 / output $3.20 / cache read $0.20 每百万）。
// 期望值 0.0793682 在 #39 那条公式测试里已被前端那张表独立算过一遍。
func TestRuelRunCostFromUsagePricesAKnownModel(t *testing.T) {
	acc := RuelRunCostFromUsage([]db.TaskUsage{usageRow("glm-5", 64389, 1257, 54784, 0)})
	if acc.PricedRows != 1 || acc.UnpricedRows != 0 {
		t.Fatalf("rows = priced %d / unpriced %d, want 1 / 0", acc.PricedRows, acc.UnpricedRows)
	}
	const want = 0.0793682
	if diff := acc.PricedUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("PricedUSD = %.10f, want %.10f", acc.PricedUSD, want)
	}
}

// unpriced 的行**跳过不计**且必须被数出来。按 0 计入会让累计永远追不上上限——那正
// 是 #32 踩过的坑（每条 codex 用量都 unpriced，等于纯 codex 的链成本恒为 0）。
func TestRuelRunCostFromUsageSkipsUnpricedButCountsThem(t *testing.T) {
	acc := RuelRunCostFromUsage([]db.TaskUsage{
		usageRow("glm-5", 1000000, 0, 0, 0),
		usageRow("unknown", 999999999, 0, 0, 0),
		usageRow("", 999999999, 0, 0, 0),
	})
	if acc.PricedRows != 1 || acc.UnpricedRows != 2 {
		t.Fatalf("rows = priced %d / unpriced %d, want 1 / 2", acc.PricedRows, acc.UnpricedRows)
	}
	if diff := acc.PricedUSD - 1.0; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("PricedUSD = %.10f, want 1.0（unpriced 的十亿 token 不得贡献任何金额）", acc.PricedUSD)
	}
}

// 一条可计价的行都没有时，金额是 0 但**不是「没花钱」**，而是「不知道花了多少」。
// 调用方据此放行并记「无法判定」，不能记成「未超限」。
func TestRuelRunCostFromUsageReportsNothingPriced(t *testing.T) {
	acc := RuelRunCostFromUsage([]db.TaskUsage{usageRow("unknown", 1000, 1000, 1000, 1000)})
	if acc.PricedRows != 0 {
		t.Fatalf("PricedRows = %d, want 0", acc.PricedRows)
	}
	if acc.UnpricedRows != 1 {
		t.Fatalf("UnpricedRows = %d, want 1", acc.UnpricedRows)
	}
}

// per_run 是一个新的维度，且必须能被单独认出来：它与 per_issue 的出路不同（一个
// 调额度重跑即可，一个要人自己派一次单）。维度混了，接收方会照着错的那个去处理。
func TestRunCostUsesADimensionOfItsOwn(t *testing.T) {
	if gaterefusal.DimensionRunCost == gaterefusal.DimensionBudget {
		t.Fatal("per_run 与 per_issue 用了同一个维度值，接收方分不清该走哪条出路")
	}
	if gaterefusal.DimensionRunCost == "" {
		t.Fatal("维度值为空")
	}
}
