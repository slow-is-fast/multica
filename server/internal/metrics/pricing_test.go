package metrics

import "testing"

func TestPriceForModelAliasAnthropicCurrentGeneration(t *testing.T) {
	cases := []struct {
		model string
		want  ModelPrice
	}{
		{
			model: "claude-sonnet-5",
			want:  ModelPrice{Provider: "anthropic", Model: "claude-sonnet-5", InputPerM: 2, CacheReadPerM: 0.2, CacheWritePerM: 2.5, OutputPerM: 10},
		},
		{
			model: "anthropic:claude-sonnet-5",
			want:  ModelPrice{Provider: "anthropic", Model: "claude-sonnet-5", InputPerM: 2, CacheReadPerM: 0.2, CacheWritePerM: 2.5, OutputPerM: 10},
		},
		{
			model: "claude-5-sonnet",
			want:  ModelPrice{Provider: "anthropic", Model: "claude-sonnet-5", InputPerM: 2, CacheReadPerM: 0.2, CacheWritePerM: 2.5, OutputPerM: 10},
		},
		{
			model: "claude-fable-5",
			want:  ModelPrice{Provider: "anthropic", Model: "claude-fable-5", InputPerM: 10, CacheReadPerM: 1, CacheWritePerM: 12.5, OutputPerM: 50},
		},
		{
			model: "anthropic/claude-fable-5",
			want:  ModelPrice{Provider: "anthropic", Model: "claude-fable-5", InputPerM: 10, CacheReadPerM: 1, CacheWritePerM: 12.5, OutputPerM: 50},
		},
		{
			model: "claude-opus-4-8",
			want:  ModelPrice{Provider: "anthropic", Model: "claude-opus-4.8", InputPerM: 5, CacheReadPerM: 0.5, CacheWritePerM: 6.25, OutputPerM: 25},
		},
		// Opus 5 sits on the same 5/25 Opus tier as 4.5-4.8.
		{
			model: "claude-opus-5",
			want:  ModelPrice{Provider: "anthropic", Model: "claude-opus-5", InputPerM: 5, CacheReadPerM: 0.5, CacheWritePerM: 6.25, OutputPerM: 25},
		},
		{
			model: "anthropic/claude-opus-5",
			want:  ModelPrice{Provider: "anthropic", Model: "claude-opus-5", InputPerM: 5, CacheReadPerM: 0.5, CacheWritePerM: 6.25, OutputPerM: 25},
		},
		// Claude Code reports the 1M-context beta with a bracketed suffix.
		{
			model: "claude-opus-5[1m]",
			want:  ModelPrice{Provider: "anthropic", Model: "claude-opus-5", InputPerM: 5, CacheReadPerM: 0.5, CacheWritePerM: 6.25, OutputPerM: 25},
		},
	}

	for _, tc := range cases {
		got, ok := PriceForModelAlias(tc.model)
		if !ok {
			t.Fatalf("PriceForModelAlias(%q) did not resolve", tc.model)
		}
		if got != tc.want {
			t.Fatalf("PriceForModelAlias(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}
}

func TestPriceForModelAliasCodexCurrentGeneration(t *testing.T) {
	// Official rates from OpenAI's GPT-5.6 announcement: cache read = 0.1x
	// input (90% cached-input discount), cache write = 1.25x input.
	cases := []struct {
		model string
		want  ModelPrice
	}{
		{
			model: "gpt-6-astra",
			want:  ModelPrice{Provider: "openai", Model: "gpt-6-astra", InputPerM: 10, CacheReadPerM: 1, CacheWritePerM: 12.5, OutputPerM: 50},
		},
		{
			model: "gpt-5.6-sol",
			want:  ModelPrice{Provider: "openai", Model: "gpt-5.6-sol", InputPerM: 5, CacheReadPerM: 0.5, CacheWritePerM: 6.25, OutputPerM: 30},
		},
		{
			model: "gpt-6.1-sol",
			want:  ModelPrice{Provider: "openai", Model: "gpt-6.1-sol", InputPerM: 2, CacheReadPerM: 0.1, CacheWritePerM: 2.5, OutputPerM: 10},
		},
		{
			model: "openai:gpt-6.1-sol[1m]",
			want:  ModelPrice{Provider: "openai", Model: "gpt-6.1-sol", InputPerM: 2, CacheReadPerM: 0.1, CacheWritePerM: 2.5, OutputPerM: 10},
		},
		{
			model: "openai:gpt-6-sol",
			want:  ModelPrice{Provider: "openai", Model: "gpt-6-sol", InputPerM: 2, CacheReadPerM: 0.2, CacheWritePerM: 2.5, OutputPerM: 10},
		},
		{
			model: "openai/gpt-6-luna",
			want:  ModelPrice{Provider: "openai", Model: "gpt-6-luna", InputPerM: 0.1, CacheReadPerM: 0.01, CacheWritePerM: 0.125, OutputPerM: 0.5},
		},
		{
			model: "openai:gpt-5.6-terra",
			want:  ModelPrice{Provider: "openai", Model: "gpt-5.6-terra", InputPerM: 2.5, CacheReadPerM: 0.25, CacheWritePerM: 3.125, OutputPerM: 15},
		},
		{
			model: "openai/gpt-5.6-luna",
			want:  ModelPrice{Provider: "openai", Model: "gpt-5.6-luna", InputPerM: 1, CacheReadPerM: 0.1, CacheWritePerM: 1.25, OutputPerM: 6},
		},
	}

	for _, tc := range cases {
		got, ok := PriceForModelAlias(tc.model)
		if !ok {
			t.Fatalf("PriceForModelAlias(%q) did not resolve", tc.model)
		}
		if got != tc.want {
			t.Fatalf("PriceForModelAlias(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}

	// Unknown suffixed variants must NOT borrow a 5.6 tier — the alias is an
	// anchored exact match, mirroring the frontend's exact-match resolver.
	// The dash-normalized ids (`gpt-5-6-luna`) must also miss: the real Codex
	// slug is always dotted and the frontend does not dash-normalize, so both
	// sides surface these as unmapped instead of silently pricing them.
	for _, model := range []string{
		"gpt-6-astra-pro",
		"gpt-6.1-sol-pro",
		"gpt-6-1-sol",
		"gpt-6-sol-high",
		"gpt-6-luna-pro",
		"gpt-6-astra/unknown",
		"gpt-6-astra-high",
		"gpt-5.6-luna-pro",
		"gpt-5.6-luna/unknown",
		"gpt-5.6-sol-high",
		"gpt-5.6-mini",
		"gpt-5-6-luna",
		"gpt-5-6-sol",
		"gpt-5-6-terra",
	} {
		if got, ok := PriceForModelAlias(model); ok {
			t.Fatalf("PriceForModelAlias(%q) unexpectedly resolved to %+v; want unmapped", model, got)
		}
	}
}

// TestPriceForModelAliasGrok pins the xAI catalog to the published rates
// (docs.x.ai/developers/pricing). Before these rows existed every Grok token
// took the unpriced branch in RecordLLMUsage, so llm_cost_usd reported zero
// Grok spend while the tokens piled up in llm_unpriced_tokens.
func TestPriceForModelAliasGrok(t *testing.T) {
	cases := []struct {
		model string
		want  ModelPrice
	}{
		{
			model: "grok-4.6",
			want:  ModelPrice{Provider: "xai", Model: "grok-4.6", InputPerM: 2, CacheReadPerM: 0.5, CacheWritePerM: 2, OutputPerM: 6},
		},
		{
			model: "xai:grok-4.6",
			want:  ModelPrice{Provider: "xai", Model: "grok-4.6", InputPerM: 2, CacheReadPerM: 0.5, CacheWritePerM: 2, OutputPerM: 6},
		},
		{
			model: "grok-4.5",
			want:  ModelPrice{Provider: "xai", Model: "grok-4.5", InputPerM: 2, CacheReadPerM: 0.3, CacheWritePerM: 2, OutputPerM: 6},
		},
		{
			model: "xai:grok-4.5",
			want:  ModelPrice{Provider: "xai", Model: "grok-4.5", InputPerM: 2, CacheReadPerM: 0.3, CacheWritePerM: 2, OutputPerM: 6},
		},
		{
			model: "xai/grok-4.5",
			want:  ModelPrice{Provider: "xai", Model: "grok-4.5", InputPerM: 2, CacheReadPerM: 0.3, CacheWritePerM: 2, OutputPerM: 6},
		},
		{
			model: "grok-4.3",
			want:  ModelPrice{Provider: "xai", Model: "grok-4.3", InputPerM: 1.25, CacheReadPerM: 0.2, CacheWritePerM: 1.25, OutputPerM: 2.5},
		},
		{
			model: "grok-build-0.1",
			want:  ModelPrice{Provider: "xai", Model: "grok-build-0.1", InputPerM: 1, CacheReadPerM: 0.2, CacheWritePerM: 1, OutputPerM: 2},
		},
		{
			model: "grok-4.20-multi-agent-0309",
			want:  ModelPrice{Provider: "xai", Model: "grok-4.20-multi-agent-0309", InputPerM: 1.25, CacheReadPerM: 0.2, CacheWritePerM: 1.25, OutputPerM: 2.5},
		},
		{
			model: "grok-4.20-0309-reasoning",
			want:  ModelPrice{Provider: "xai", Model: "grok-4.20-0309-reasoning", InputPerM: 1.25, CacheReadPerM: 0.2, CacheWritePerM: 1.25, OutputPerM: 2.5},
		},
		{
			model: "grok-4.20-0309-non-reasoning",
			want:  ModelPrice{Provider: "xai", Model: "grok-4.20-0309-non-reasoning", InputPerM: 1.25, CacheReadPerM: 0.2, CacheWritePerM: 1.25, OutputPerM: 2.5},
		},
	}

	for _, tc := range cases {
		got, ok := PriceForModelAlias(tc.model)
		if !ok {
			t.Fatalf("PriceForModelAlias(%q) did not resolve", tc.model)
		}
		if got != tc.want {
			t.Fatalf("PriceForModelAlias(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}

	// `grok-composer-*` ships in the Grok Build catalog but xAI publishes no
	// rate for it, so it must stay unmapped rather than inherit grok-4.5's.
	// Suffixed and dash-spelled variants must miss for the same reason the
	// gpt-5.6 rows do: the frontend resolver is an exact match that does not
	// dash-normalize non-Anthropic ids, so both sides agree on "unmapped".
	for _, model := range []string{
		"grok-composer-2.5-fast",
		"grok-composer-2.5",
		"grok-4.6-fast",
		"grok-4-6",
		"grok-4.5-fast",
		"grok-4-5",
		"grok-4.20-0309",
		"grok",
		"unknown",
	} {
		if got, ok := PriceForModelAlias(model); ok {
			t.Fatalf("PriceForModelAlias(%q) unexpectedly resolved to %+v; want unmapped", model, got)
		}
	}
}

// TestGrokPricingMatchesRecordedTurn re-derives the cost of a real
// grok 0.2.106 turn from the table and checks it against the costUsdTicks xAI
// returned for that same turn (1 tick = 1e-10 USD). This is the end-to-end
// proof that both the rates and the cached-input bucketing are right.
func TestGrokPricingMatchesRecordedTurn(t *testing.T) {
	// Captured payload: inputTokens 12929, cachedReadTokens 10880,
	// outputTokens 29, totalTokens 12958, costUsdTicks 75360000. Grok counts
	// the cached prefix inside inputTokens, so the uncached remainder is
	// 12929 - 10880 = 2049 (see excludeACPCachedInput in pkg/agent/hermes.go).
	const (
		uncachedInput = int64(2049)
		cacheRead     = int64(10880)
		output        = int64(29)
		wantUSD       = 75360000 / 1e10
	)

	price, ok := PriceForModelAlias("grok-4.5")
	if !ok {
		t.Fatal("grok-4.5 did not resolve")
	}
	got := tokenCostUSD(uncachedInput, price.InputPerM) +
		tokenCostUSD(cacheRead, price.CacheReadPerM) +
		tokenCostUSD(output, price.OutputPerM)

	if diff := got - wantUSD; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("recomputed cost = %.10f, want %.10f (xAI costUsdTicks)", got, wantUSD)
	}
}

// TestPriceForModelAliasAlibabaMoonshotVolcengine pins the pay-as-you-go
// rates for the Chinese-model runtimes (Qwen / Kimi) added from models.dev,
// and the transport spellings that reach them: `provider:model` (Hermes
// custom providers), `provider/model` (opencode), and bare ids. Volcengine's
// `ark-code-latest` rolling alias is covered as unmapped in
// TestPriceForModelAliasNoFalseBorrowing.
func TestPriceForModelAliasAlibabaMoonshotVolcengine(t *testing.T) {
	cases := []struct {
		model string
		want  ModelPrice
	}{
		{
			model: "qwen3.7-plus",
			want:  ModelPrice{Provider: "alibaba", Model: "qwen3.7-plus", InputPerM: 0.40, CacheReadPerM: 0.04, CacheWritePerM: 0.50, OutputPerM: 1.60},
		},
		{
			model: "alibaba-coding-plan:qwen3.7-plus",
			want:  ModelPrice{Provider: "alibaba", Model: "qwen3.7-plus", InputPerM: 0.40, CacheReadPerM: 0.04, CacheWritePerM: 0.50, OutputPerM: 1.60},
		},
		{
			model: "qwen3.6-flash",
			want:  ModelPrice{Provider: "alibaba", Model: "qwen3.6-flash", InputPerM: 0.25, CacheReadPerM: 0.025, CacheWritePerM: 0.3125, OutputPerM: 1.50},
		},
		{
			model: "alibaba-coding-plan:qwen3.8-max",
			want:  ModelPrice{Provider: "alibaba", Model: "qwen3.8-max", InputPerM: 2.00, CacheReadPerM: 0.17, CacheWritePerM: 2.50, OutputPerM: 6.00},
		},
		{
			model: "custom:qwen3.8-max-preview[1m]",
			want:  ModelPrice{Provider: "alibaba", Model: "qwen3.8-max-preview", InputPerM: 0, CacheReadPerM: 0, CacheWritePerM: 0, OutputPerM: 0},
		},
		{
			model: "kimi-coding:kimi-k3",
			want:  ModelPrice{Provider: "moonshotai", Model: "kimi-k3", InputPerM: 3.0, CacheReadPerM: 0.30, CacheWritePerM: 3.0, OutputPerM: 15.0},
		},
		{
			// Kimi Code CLI reports `kimi-code/k3`.
			model: "kimi-code/k3",
			want:  ModelPrice{Provider: "moonshotai", Model: "kimi-k3", InputPerM: 3.0, CacheReadPerM: 0.30, CacheWritePerM: 3.0, OutputPerM: 15.0},
		},
		{
			// `custom:anthropic/claude-opus-4.7` (provider prefix + nested
			// slash path) must still resolve to the anthropic Opus tier via
			// substring matching, mirroring the frontend stripProvider
			// regression case.
			model: "custom:anthropic/claude-opus-4.7",
			want:  ModelPrice{Provider: "anthropic", Model: "claude-opus-4.7", InputPerM: 5.00, CacheReadPerM: 0.50, CacheWritePerM: 6.25, OutputPerM: 25.00},
		},
	}

	for _, tc := range cases {
		got, ok := PriceForModelAlias(tc.model)
		if !ok {
			t.Fatalf("PriceForModelAlias(%q) did not resolve", tc.model)
		}
		if got != tc.want {
			t.Fatalf("PriceForModelAlias(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}
}

// TestPriceForModelAliasNoFalseBorrowing guards the anchored rules: a preview
// SKU must not inherit the GA tier, a distinct CodeBuddy SKU must not inherit
// Kimi K3, unknown suffixed variants must stay unmapped, empty bracket tags
// (`qwen3.7-plus[]` etc.) must stay unmapped to match the frontend's
// `\[[^\]]+\]$` tag stripping, and the Volcengine `ark-code-latest` rolling
// alias must stay unmapped.
func TestPriceForModelAliasNoFalseBorrowing(t *testing.T) {
	for _, model := range []string{
		"qwen3.8-max-preview",
		"qwen3.8-max-preview[1m]",
		"kimi-k3-1",
		"qwen3.8-max-extra",
		"qwen3.8-max[",
		"qwen3.8-max[1m]-extra",
		"qwen3.8-max-preview[1m]-extra",
	} {
		got, ok := PriceForModelAlias(model)
		if !ok {
			continue
		}
		if got.Model == "qwen3.8-max" || got.Model == "kimi-k3" {
			t.Fatalf("PriceForModelAlias(%q) borrowed %s; want the SKU's own tier or unmapped", model, got.Model)
		}
	}

	// A distinct SKU that borrows nothing must resolve to its own row.
	for _, tc := range []struct {
		model     string
		wantModel string
	}{
		{"qwen3.8-max-preview[1m]", "qwen3.8-max-preview"},
		{"qwen3.8-max-preview[context]", "qwen3.8-max-preview"},
		{"qwen3.8-max[1m]", "qwen3.8-max"},
	} {
		got, ok := PriceForModelAlias(tc.model)
		if !ok || got.Model != tc.wantModel {
			t.Fatalf("PriceForModelAlias(%q) = %+v (ok=%v); want %s", tc.model, got, ok, tc.wantModel)
		}
	}

	for _, model := range []string{
		"qwen3.8-max-extra",
		"kimi-k3-1",
		"qwen3.7-plus-extra",
		"qwen3.6-flash-extra",
		"qwen3.8-max-preview-extra",
		"custom:ark-code-latest",
		// Empty bracket tags: the frontend's `\[[^\]]+\]$` tag stripper
		// leaves these unmapped, so the backend must too.
		"qwen3.7-plus[]",
		"qwen3.6-flash[]",
		"qwen3.8-max[]",
		"qwen3.8-max-preview[]",
		// GLM-5's own id is a prefix of its neighbours, so the neighbours
		// must not borrow its row: they now have rows of their own
		// (ruel#44) at different rates, and `-preview` is a distinct SKU
		// at an unknown rate.
		"glm-5-preview",
		"glm-5.2",
		// Same trap one family down: `glm-4.5` is a prefix of `glm-4.5-air`
		// (3x cheaper), and `glm-4.7` is a prefix of `glm-4.7-flash` (free).
		"glm-4.8",
		"glm-4.5-air-free",
		// Cursor's generic ids must not resolve bare — `auto` is also what
		// codex reports, so resolving it here would be a cross-provider
		// misprice rather than a coverage win.
		"auto",
		"composer-1",
		"composer-2.5",
		"codex/auto",
		"openai/auto",
	} {
		if _, ok := PriceForModelAlias(model); ok {
			t.Fatalf("PriceForModelAlias(%q) unexpectedly resolved", model)
		}
	}
}

// TestZhipuGLM5Priced is the #24 regression: the Go rate table was missing the
// entire glm family that packages/views/runtimes/utils.ts already carried, so
// every local turn — 21 of 24 usage rows on this machine — fell into the
// unpriced bucket and converted to $0.00. A conversion that is always zero is
// worse than no conversion: the cost alert in PRD 6.6.1 sits on top of it and
// can never fire.
//
// The case also pins the shape that matters most here: cache_read dominates
// this machine's usage (5.06M cache-read tokens against 1.00M input over the
// same window), so a row with the wrong cache rate under-prices by more than
// the input row can correct.
func TestZhipuGLM5Priced(t *testing.T) {
	price, ok := PriceForModelAlias("glm-5")
	if !ok {
		t.Fatal("PriceForModelAlias(\"glm-5\") did not resolve; the glm family is missing from the Go table again")
	}
	want := ModelPrice{Provider: "zhipu", Model: "glm-5", InputPerM: 1.00, CacheReadPerM: 0.20, CacheWritePerM: 1.00, OutputPerM: 3.20}
	if price != want {
		t.Fatalf("PriceForModelAlias(\"glm-5\") = %+v, want %+v", price, want)
	}

	// A context tag is the same SKU at the same tier.
	if got, ok := PriceForModelAlias("glm-5[1m]"); !ok || got != want {
		t.Fatalf("PriceForModelAlias(\"glm-5[1m]\") = %+v (ok=%v); want %+v", got, ok, want)
	}
	// So is a provider-prefixed form, mirroring the Alibaba/Qwen cases.
	if got, ok := PriceForModelAlias("custom:glm-5"); !ok || got != want {
		t.Fatalf("PriceForModelAlias(\"custom:glm-5\") = %+v (ok=%v); want %+v", got, ok, want)
	}

	// A median-sized real turn must convert to a non-zero cost. These three
	// numbers are the p50 of the 21 `glm-5` usage rows on this machine
	// (2026-10-07): p50 input 43,068 / output 1,198 / cache-read 240,128.
	const (
		input     = 43_068
		output    = 1_198
		cacheRead = 240_128
	)
	got := tokenCostUSD(input, price.InputPerM) +
		tokenCostUSD(output, price.OutputPerM) +
		tokenCostUSD(cacheRead, price.CacheReadPerM)
	if got <= 0 {
		t.Fatalf("median turn cost = %v; want non-zero", got)
	}
	// Guard the cache term specifically: it is the largest component, so a
	// row that silently priced cache reads at 0 would still pass a loose
	// "non-zero" check on the input term alone.
	cacheTerm := tokenCostUSD(cacheRead, price.CacheReadPerM)
	if cacheTerm <= 0 {
		t.Fatalf("cache-read term = %v; want non-zero", cacheTerm)
	}
	if cacheTerm <= tokenCostUSD(input, price.InputPerM) {
		t.Fatalf("cache-read term %v should exceed the input term %v at this machine's cache:input ratio",
			cacheTerm, tokenCostUSD(input, price.InputPerM))
	}
}

// TestPriceForModelAliasContextTagStripping pins the `[1m]` context-variant
// suffix normalization across every rule, including the anchored Codex / Grok /
// Kimi rules that do not carry a per-rule optional bracket group. Claude Code
// (and other harnesses) append a context-window tag such as `[1m]` to the
// model id; it is the same SKU at the same tier, so the row must price instead
// of falling into the unpriced bucket in RecordLLMUsage. Mirrors the frontend's
// `stripContextTag` (`\[[^\]]+\]$`) in packages/views/runtimes/utils.ts.
func TestPriceForModelAliasContextTagStripping(t *testing.T) {
	cases := []struct {
		model string
		want  ModelPrice
	}{
		{
			model: "grok-4.5[1m]",
			want:  ModelPrice{Provider: "xai", Model: "grok-4.5", InputPerM: 2.00, CacheReadPerM: 0.30, CacheWritePerM: 2.00, OutputPerM: 6.00},
		},
		{
			model: "gpt-5.6-luna[1m]",
			want:  ModelPrice{Provider: "openai", Model: "gpt-5.6-luna", InputPerM: 1.00, CacheReadPerM: 0.10, CacheWritePerM: 1.25, OutputPerM: 6.00},
		},
		{
			model: "kimi-k3[1m]",
			want:  ModelPrice{Provider: "moonshotai", Model: "kimi-k3", InputPerM: 3.0, CacheReadPerM: 0.30, CacheWritePerM: 3.0, OutputPerM: 15.0},
		},
	}

	for _, tc := range cases {
		got, ok := PriceForModelAlias(tc.model)
		if !ok {
			t.Fatalf("PriceForModelAlias(%q) did not resolve", tc.model)
		}
		if got != tc.want {
			t.Fatalf("PriceForModelAlias(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}

	// The tag stripper is anchored at end-of-string with a non-empty tag, so it
	// must not turn these misses into hits: a trailing bracket that is not a
	// complete end-of-string tag, and an empty tag, both stay unmapped — the
	// same guard the frontend keeps.
	for _, model := range []string{
		"grok-4.5[1m]-extra",
		"gpt-5.6-luna[]",
		"kimi-k3[",
	} {
		if got, ok := PriceForModelAlias(model); ok {
			t.Fatalf("PriceForModelAlias(%q) unexpectedly resolved to %+v; want unmapped", model, got)
		}
	}
}

func TestPriceForModelAliasAnthropicFable51(t *testing.T) {
	// Fable 5.1 is its own SKU on the same Mythos-class tier as Fable 5, but
	// with cache reads at 0.025x input ($0.25) instead of the usual 0.1x. A
	// Fable 5 alias that did not stop at the version would swallow the `-1`
	// suffix and bill those reads at 4x, so every spelling below must land on
	// the Fable 5.1 row specifically.
	fable51 := ModelPrice{Provider: "anthropic", Model: "claude-fable-5-1", InputPerM: 10, CacheReadPerM: 0.25, CacheWritePerM: 12.5, OutputPerM: 50}
	fable5 := ModelPrice{Provider: "anthropic", Model: "claude-fable-5", InputPerM: 10, CacheReadPerM: 1, CacheWritePerM: 12.5, OutputPerM: 50}
	cases := []struct {
		model string
		want  ModelPrice
	}{
		{model: "claude-fable-5-1", want: fable51},
		{model: "anthropic/claude-fable-5-1", want: fable51},
		{model: "anthropic:claude-fable-5-1", want: fable51},
		// Copilot reports Claude models dotted.
		{model: "claude-fable-5.1", want: fable51},
		// Claude Code reports the 1M-context variant with a bracketed suffix.
		{model: "claude-fable-5-1[1m]", want: fable51},
		// Fable 5 must keep resolving to its own row, including its 1M form.
		{model: "claude-fable-5", want: fable5},
		{model: "claude-fable-5[1m]", want: fable5},
		// The frontend resolver strips a trailing date snapshot / `-latest`
		// before its exact-key lookup (`stripDate` in
		// packages/views/runtimes/utils.ts), so these forms price there. Both
		// rules have to admit them too, otherwise the dashboard and
		// RecordLLMUsage disagree on the same id.
		{model: "claude-fable-5-20260401", want: fable5},
		{model: "claude-fable-5-2026-04-01", want: fable5},
		{model: "claude-fable-5-latest", want: fable5},
		{model: "claude-fable-5-20260401[1m]", want: fable5},
		{model: "claude-fable-5-1-20260901", want: fable51},
		{model: "claude-fable-5-1-latest", want: fable51},
		{model: "claude-fable-5-1-20260901[1m]", want: fable51},
	}

	for _, tc := range cases {
		got, ok := PriceForModelAlias(tc.model)
		if !ok {
			t.Fatalf("PriceForModelAlias(%q) did not resolve", tc.model)
		}
		if got != tc.want {
			t.Fatalf("PriceForModelAlias(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}

	// A later Fable minor is a distinct SKU at an unknown rate: it must stay
	// unmapped and surface in the unpriced diagnostic rather than borrow a
	// neighbour's tier. This is the same failure the `-1` suffix had against
	// the Fable 5 rule, so guard it on the 5.1 rule as well.
	for _, model := range []string{
		"claude-fable-5-2",
		"claude-fable-5.2",
		"claude-fable-5-10",
		"claude-fable-5.10",
		"claude-fable-5-1x",
	} {
		if got, ok := PriceForModelAlias(model); ok {
			t.Errorf("PriceForModelAlias(%q) resolved to %+v; want unmapped", model, got)
		}
	}

	// An admitted suffix only counts when it ENDS the id. These rules are
	// substring matches, so a terminator whose alternatives are not anchored
	// still fires on anything that merely starts with one — an unknown
	// qualifier would silently borrow the tier of whichever row it prefixed,
	// while the frontend (which anchors both `stripDate` and the bracket tag)
	// leaves it unmapped. Same id, two different costs.
	for _, model := range []string{
		"claude-fable-5-1-latest-preview",
		"claude-fable-5-1-20260901x",
		"claude-fable-5-1-2026-09-01-preview",
		"claude-fable-5-1[1m]junk",
		"claude-fable-5-latest-preview",
		"claude-fable-5-20260401-preview",
		"claude-fable-5[1m]junk",
	} {
		if got, ok := PriceForModelAlias(model); ok {
			t.Errorf("PriceForModelAlias(%q) resolved to %+v; want unmapped", model, got)
		}
	}

	// A doubly-tagged id must not sneak back in through the tag-stripping
	// retry: peeling `[2m]` leaves `[1m]`, which the rule above rejected on
	// the raw form for good reason. The frontend strips one tag and does not
	// re-strip, so pricing these here would put two different costs on one
	// usage row.
	for _, model := range []string{
		"claude-fable-5[1m][2m]",
		"claude-fable-5-1[1m][2m]",
	} {
		if got, ok := PriceForModelAlias(model); ok {
			t.Errorf("PriceForModelAlias(%q) resolved to %+v; want unmapped", model, got)
		}
	}
}

func TestPriceForModelAliasAnthropicOpus55(t *testing.T) {
	// Opus 5.5 is its own SKU at $4 / $20 with cache reads at 0.05x input
	// ($0.20). An Opus 5 alias that did not stop at the version would swallow
	// the `-5` suffix and bill 5.5 at Opus 5's 5/25 tier, so every spelling
	// below must land on the Opus 5.5 row specifically.
	opus55 := ModelPrice{Provider: "anthropic", Model: "claude-opus-5-5", InputPerM: 4, CacheReadPerM: 0.2, CacheWritePerM: 5, OutputPerM: 20}
	opus5 := ModelPrice{Provider: "anthropic", Model: "claude-opus-5", InputPerM: 5, CacheReadPerM: 0.5, CacheWritePerM: 6.25, OutputPerM: 25}
	cases := []struct {
		model string
		want  ModelPrice
	}{
		{model: "claude-opus-5-5", want: opus55},
		{model: "anthropic/claude-opus-5-5", want: opus55},
		{model: "anthropic:claude-opus-5-5", want: opus55},
		// Copilot reports Claude models dotted.
		{model: "claude-opus-5.5", want: opus55},
		// Claude Code reports the 1M-context variant with a bracketed suffix.
		{model: "claude-opus-5-5[1m]", want: opus55},
		{model: "claude-opus-5-5-20260901", want: opus55},
		{model: "claude-opus-5-5-latest", want: opus55},
		{model: "claude-opus-5-5-20260901[1m]", want: opus55},
		// Opus 5 must keep resolving to its own row in every suffix form the
		// frontend resolver also strips.
		{model: "claude-opus-5", want: opus5},
		{model: "claude-opus-5[1m]", want: opus5},
		{model: "claude-opus-5-20260401", want: opus5},
		{model: "claude-opus-5-latest", want: opus5},
	}

	for _, tc := range cases {
		got, ok := PriceForModelAlias(tc.model)
		if !ok {
			t.Fatalf("PriceForModelAlias(%q) did not resolve", tc.model)
		}
		if got != tc.want {
			t.Fatalf("PriceForModelAlias(%q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}

	// Another Opus 5 minor is a distinct SKU at an unknown rate: it must stay
	// unmapped rather than borrow Opus 5's or 5.5's tier, matching the
	// frontend's exact-key lookup.
	for _, model := range []string{
		"claude-opus-5-1",
		"claude-opus-5.6",
		"claude-opus-5-55",
		"claude-opus-5-5x",
		"claude-opus-5-5-latest-preview",
		"claude-opus-5-5[1m]junk",
		"claude-opus-5[1m][2m]",
		"claude-opus-5-5[1m][2m]",
	} {
		if got, ok := PriceForModelAlias(model); ok {
			t.Errorf("PriceForModelAlias(%q) resolved to %+v; want unmapped", model, got)
		}
	}
}

// TestEstimateUsageCostKeepsUnknownApartFromZero pins the distinction this
// whole file's cost path exists for: a row we cannot price must not come back
// as the same 0 as a row that is genuinely free. Collapsing them is what makes
// a cost median silently wrong — every unpriced row drags it toward 0, which
// in turn makes a "3x the median" alert fire on ordinary runs.
//
// The glm-5 numbers are the real p50 of the 21 glm-5 rows on this machine
// (2026-10-07), and the `unknown` row is one of the 3 codex rows whose adapter
// reported no model at all: 28,833 input / 969 output / 122,880 cache read.
func TestEstimateUsageCostKeepsUnknownApartFromZero(t *testing.T) {
	cases := []struct {
		name  string
		model string
		// provider is the reporter, and it is part of the price lookup: some
		// ids (`auto`) are only meaningful once you know who sent them.
		// Empty means "no provider recorded", which is what the older rows
		// look like and what the bare-model fallback exists for.
		provider   string
		ticks      int64
		in, out    int64
		cr, cw     int64
		wantSource CostSource
		wantZero   bool
	}{
		{
			name:       "provider price wins over the table",
			model:      "glm-5",
			ticks:      1_234_000_000,
			in:         43_068, out: 1_198, cr: 240_128,
			wantSource: CostSourceProvider,
		},
		{
			name:       "rate table estimate",
			model:      "glm-5",
			in:         43_068, out: 1_198, cr: 240_128,
			wantSource: CostSourceTable,
		},
		{
			name:       "no model, no price, real tokens: unknown",
			model:      "unknown",
			in:         28_833, out: 969, cr: 122_880,
			wantSource: CostSourceUnpriced, wantZero: true,
		},
		{
			name:       "no tokens at all: genuinely free",
			model:      "glm-5",
			wantSource: CostSourceZero, wantZero: true,
		},
	}

	for _, tc := range cases {
		got := EstimateUsageCost(tc.model, tc.provider, tc.ticks, tc.in, tc.out, tc.cr, tc.cw)
		if got.Source != tc.wantSource {
			t.Errorf("%s: source = %q, want %q", tc.name, got.Source, tc.wantSource)
		}
		if tc.wantZero && got.USD != 0 {
			t.Errorf("%s: USD = %v, want 0", tc.name, got.USD)
		}
		if !tc.wantZero && got.USD <= 0 {
			t.Errorf("%s: USD = %v, want > 0", tc.name, got.USD)
		}
	}

	// The two 0s are different: only one of them may enter an aggregate.
	unknown := EstimateUsageCost("unknown", "codex", 0, 28_833, 969, 122_880, 0)
	free := EstimateUsageCost("glm-5", "", 0, 0, 0, 0, 0)
	if unknown.Priceable() {
		t.Error("unpriced row is Priceable(); it must be excluded from medians and budgets")
	}
	if !free.Priceable() {
		t.Error("genuinely free row is not Priceable(); a real 0 belongs in the median")
	}
	// And the reason it matters, stated as a number: the 24 real rows price to
	// a p50 of $0.1008 with the 3 unpriced rows excluded. Folding them in as 0
	// moves p50 to $0.0901 and drops the 3x bar from $0.3025 to $0.2703 — a
	// 10% lower trip line, paid for entirely by rows whose cost is unknown.
	const p50PricedOnly, p50WithUnknowns = 0.1008, 0.0901
	if p50WithUnknowns >= p50PricedOnly {
		t.Fatalf("test constant drifted: unknowns must lower the median")
	}
}

// TestEstimateUsageCostPricesTheModelCodexNowReports pins the payoff of ruel
// #32. Before it, every codex usage row reached this function as the literal
// "unknown" even though its token counts were right there, so the row came
// back unpriced and dropped out of medians and budgets.
//
// The numbers are real: the first codex Run after the adapter started
// reporting a model (2026-10-07), 14,512 input / 773 output / 109,184 cache
// read, recorded with model "gpt-5.6-sol" instead of "unknown".
func TestEstimateUsageCostPricesTheModelCodexNowReports(t *testing.T) {
	const codexIn, codexOut, codexCacheRead = 14_512, 773, 109_184

	before := EstimateUsageCost("unknown", "codex", 0, codexIn, codexOut, codexCacheRead, 0)
	if before.Source != CostSourceUnpriced {
		t.Fatalf("pre-fix codex row: source = %q, want %q", before.Source, CostSourceUnpriced)
	}
	if before.Priceable() {
		t.Fatal("pre-fix codex row is Priceable(); it must stay out of medians and budgets")
	}

	after := EstimateUsageCost("gpt-5.6-sol", "codex", 0, codexIn, codexOut, codexCacheRead, 0)
	if after.Source != CostSourceTable {
		t.Fatalf("post-fix codex row: source = %q, want %q", after.Source, CostSourceTable)
	}
	if after.USD <= 0 {
		t.Fatalf("post-fix codex row: USD = %v, want > 0", after.USD)
	}

	// Two rows with identical tokens; only the model name differs. That one
	// field is the whole of the gap #32 closed, so state it as the number it
	// is worth rather than as a comment someone has to trust.
	t.Logf("same codex tokens: unknown prices to %v (source %q), gpt-5.6-sol to %.4f USD (source %q)",
		before.USD, before.Source, after.USD, after.Source)
}
