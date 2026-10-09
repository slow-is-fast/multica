package metrics

// ruel#44: the coverage-gap guard says a frontend row is closed when the
// server RESOLVES it to the same rates. That is the property worth having, but
// it is coarse: it cannot tell you which alias rule did the work, and it only
// ever probes the bare table key. These tests pin the two things the guard
// cannot.
//
//  1. Reachability, in the shapes a daemon actually reports. A row with no
//     rule that reaches it is a dead row: the table looks complete, the guard
//     still lists the key as a gap, and nothing says why. So every closed SKU
//     is probed through its dotted, dashed, provider-prefixed, dated and
//     bracketed spellings — not just the bare key.
//  2. The order traps. A rule that swallows a neighbouring SKU is the failure
//     mode this table is most prone to, and it is silent: both rows exist,
//     both resolve, only the rates are wrong.

import "testing"

// TestClosedRowsResolveThroughTheirAliasRules walks every SKU closed so far and
// asserts the server reaches it from each spelling the frontend resolver would
// accept. Refs ruel#44.
//
// The dated and bracketed forms matter because the dashboard strips a trailing
// date / `latest` and a context tag before its exact-key lookup. A server rule
// anchored at a bare `$` leaves those unpriced here while the UI prices them —
// the gap in the direction nobody looks.
func TestClosedRowsResolveThroughTheirAliasRules(t *testing.T) {
	cases := []struct {
		priceKey string
		ids      []string
	}{
		// -- ruel#44 batch: Anthropic pre-4.5 --
		{"anthropic:claude-opus-4.1", []string{
			"claude-opus-4-1", "claude-opus-4.1", "anthropic/claude-opus-4-1",
			"claude-opus-4-1-latest", "claude-opus-4-1-20260101", "claude-opus-4-1-2026-01-01",
			"claude-opus-4-1[1m]",
		}},
		{"anthropic:claude-opus-4", []string{
			"claude-opus-4", "anthropic/claude-opus-4",
			"claude-opus-4-latest", "claude-opus-4-20260101", "claude-opus-4[1m]",
		}},
		{"anthropic:claude-sonnet-4", []string{
			"claude-sonnet-4", "anthropic/claude-sonnet-4",
			"claude-sonnet-4-latest", "claude-sonnet-4-20260101",
		}},
		{"anthropic:claude-haiku-3.5", []string{
			"claude-haiku-3-5", "claude-haiku-3.5", "anthropic/claude-haiku-3-5",
			"claude-haiku-3-5-latest", "claude-haiku-3-5[1m]",
		}},
		// -- ruel#44 batch: OpenAI GPT-5 family, o-series, GPT-4o --
		{"openai:gpt-5", []string{"gpt-5", "openai/gpt-5", "gpt-5-latest", "gpt-5-2025-08-07"}},
		{"openai:gpt-5-codex", []string{"gpt-5-codex", "openai/gpt-5-codex", "gpt-5-codex-latest"}},
		{"openai:gpt-5-mini", []string{"gpt-5-mini", "openai/gpt-5-mini", "gpt-5-mini-2025-08-07"}},
		{"openai:gpt-5-nano", []string{"gpt-5-nano", "openai/gpt-5-nano", "gpt-5-nano-latest"}},
		{"openai:o3", []string{"o3", "openai/o3", "o3-latest", "o3-2025-04-16"}},
		{"openai:o3-mini", []string{"o3-mini", "openai/o3-mini", "o3-mini-2025-01-31"}},
		{"openai:o4-mini", []string{"o4-mini", "openai/o4-mini", "o4-mini-2025-04-16"}},
		{"openai:gpt-4o", []string{"gpt-4o", "openai/gpt-4o", "gpt-4o-latest", "gpt-4o-2024-11-20"}},
		{"openai:gpt-4o-mini", []string{"gpt-4o-mini", "openai/gpt-4o-mini", "gpt-4o-mini-2024-07-18"}},
		// -- ruel#44 batch: Zhipu GLM, the rest of the family --
		{"zhipu:glm-5", []string{"glm-5", "zhipu/glm-5", "glm-5-latest", "glm-5-20260101", "glm-5[1m]"}},
		{"zhipu:glm-5.1", []string{"glm-5.1", "glm-5-1", "zhipu/glm-5.1", "glm-5.1-latest"}},
		{"zhipu:glm-5-turbo", []string{"glm-5-turbo", "zhipu/glm-5-turbo", "glm-5-turbo-20260101"}},
		{"zhipu:glm-4.7", []string{"glm-4.7", "glm-4-7", "zhipu/glm-4.7", "glm-4.7[1m]"}},
		{"zhipu:glm-4.7-flashx", []string{"glm-4.7-flashx", "glm-4-7-flashx", "zhipu/glm-4.7-flashx"}},
		{"zhipu:glm-4.7-flash", []string{"glm-4.7-flash", "glm-4-7-flash", "zhipu/glm-4.7-flash"}},
		{"zhipu:glm-4.6", []string{"glm-4.6", "glm-4-6", "zhipu/glm-4.6", "glm-4.6-latest"}},
		{"zhipu:glm-4.5", []string{"glm-4.5", "glm-4-5", "zhipu/glm-4.5", "glm-4.5-20260101"}},
		{"zhipu:glm-4.5-x", []string{"glm-4.5-x", "glm-4-5-x", "zhipu/glm-4.5-x"}},
		{"zhipu:glm-4.5-air", []string{"glm-4.5-air", "glm-4-5-air", "zhipu/glm-4.5-air"}},
		{"zhipu:glm-4.5-airx", []string{"glm-4.5-airx", "glm-4-5-airx", "zhipu/glm-4.5-airx"}},
		{"zhipu:glm-4.5-flash", []string{"glm-4.5-flash", "glm-4-5-flash", "zhipu/glm-4.5-flash"}},
		// -- ruel#44 batch: Moonshot K2.6 --
		{"moonshotai:kimi-k2.6", []string{"kimi-k2.6", "kimi-k2-6", "moonshotai/kimi-k2.6", "kimi-k2.6[1m]"}},
		// -- ruel#44 batch: Cursor. Every id is probed QUALIFIED, because
		// qualifying is the whole point: these names are generic across
		// providers and the bare form must NOT resolve (see
		// TestGenericIdsStayUnpricedWithoutAProvider). --
		{"cursor:auto", []string{"cursor/auto", "cursor/auto-latest", "cursor/auto[1m]"}},
		{"cursor:composer-2.5-fast", []string{"cursor/composer-2.5-fast", "cursor/composer-2-5-fast"}},
		{"cursor:composer-2.5", []string{"cursor/composer-2.5", "cursor/composer-2-5"}},
		{"cursor:composer-2-fast", []string{"cursor/composer-2-fast"}},
		{"cursor:composer-2", []string{"cursor/composer-2", "cursor/composer-2-20260101"}},
		{"cursor:composer-1.5", []string{"cursor/composer-1.5", "cursor/composer-1-5"}},
		{"cursor:composer-1", []string{"cursor/composer-1", "cursor/composer-1-latest"}},
		// The legacy fallback bucket: the daemon emits the provider name
		// itself when it has no model to name.
		{"cursor:cursor", []string{"cursor", "cursor/cursor"}},
	}

	for _, tc := range cases {
		want, ok := modelPrices[tc.priceKey]
		if !ok {
			t.Fatalf("table has no row %q — the price key in this test is wrong", tc.priceKey)
		}
		for _, id := range tc.ids {
			got, ok := PriceForModelAlias(id)
			if !ok {
				t.Errorf("%s: server cannot resolve %q — the alias rule does not reach this spelling", tc.priceKey, id)
				continue
			}
			if got.Provider+":"+got.Model != tc.priceKey {
				t.Errorf("%s: %q resolved to %s:%s instead", tc.priceKey, id, got.Provider, got.Model)
			}
			if got.InputPerM != want.InputPerM || got.OutputPerM != want.OutputPerM ||
				got.CacheReadPerM != want.CacheReadPerM || got.CacheWritePerM != want.CacheWritePerM {
				t.Errorf("%s: %q resolved to the right key but different rates", tc.priceKey, id)
			}
		}
	}
}

// TestUnknownNeighboursStayUnmapped is the discriminator the pair test below
// cannot be.
//
// Why it needs to exist: "claude-opus-4-5 resolves to opus-4.5" stays true even
// when the opus-4 rule loses its end-anchor, because the more specific rule
// happens to come first in the list and wins on ORDER. A mutation that strips
// versionEnd from five of the new rules left the pair test green — every one of
// them was saved by ordering, not by anchoring.
//
// That is a real fragility, not a theoretical one: reorder these rules and the
// base tier silently swallows the whole family. So the property worth pinning
// is the one ordering cannot fake — an id for a SKU this table does not carry
// must stay UNMAPPED rather than inherit its neighbour's rate. That is also
// what both sides already promise ("every catalog SKU needs its own row").
func TestUnknownNeighboursStayUnmapped(t *testing.T) {
	for _, id := range []string{
		// Anthropic: later 4.x SKUs do not exist in either table.
		"claude-opus-4-9", "claude-opus-4.9", "claude-sonnet-4-9", "claude-haiku-3-9",
		"claude-opus-4-1-1",
		// OpenAI: same trap, and the spread is worse (25x on nano).
		"gpt-5-turbo", "gpt-5-pro", "gpt-5-mini-plus",
		"o3-pro", "o3-mini-high", "o4-mini-high",
		"gpt-4o-plus", "gpt-4o-mini-plus",
		// Zhipu: an unknown minor or qualifier must not inherit its
		// neighbour's rate. `glm-4.5-air-free` is the sharpest one — as a
		// prefix it would otherwise land on `glm-4.5-air` ($0.20 / $1.10)
		// or `glm-4.5` ($0.60 / $2.20).
		"glm-5.2", "glm-5-preview", "glm-5-1-1",
		"glm-4.8", "glm-4.9", "glm-4-5-air-free", "glm-4.5-air-pro",
		"glm-4.6-pro", "glm-4.7-flash-pro", "glm-4.5-flashx",
		// Moonshot: K2.6 is the only K2 variant on the official price
		// sheet, so the neighbours must stay unmapped.
		"kimi-k2.5", "kimi-k2.7",
		// Cursor: an unknown composer minor must not fall back to
		// `composer-2` ($0.50 / $2.50) or to the legacy `cursor` bucket.
		"cursor/composer-3", "cursor/composer-2.6", "cursor/composer-1.6",
		"cursor/composer-2.5-pro",
	} {
		if p, ok := PriceForModelAlias(id); ok {
			t.Errorf("%q resolved to %s:%s — it is not a SKU either table carries, so it must stay unmapped instead of inheriting a neighbour's rate",
				id, p.Provider, p.Model)
		}
	}
}

// TestOrderTrapsResolveToTheirOwnTier is the guard against silent
// tier-borrowing: a shorter id whose rule is a bare substring swallows its
// longer neighbour and bills it at the wrong rate.
//
// Each pair below is one the table can actually get wrong, and the wrong
// direction is expensive in one specific way — noted per case, because "the
// rate is wrong" is not actionable, "a nano run bills at 25x" is.
func TestOrderTrapsResolveToTheirOwnTier(t *testing.T) {
	pairs := []struct {
		id       string
		wantKey  string
		borrowed string
		why      string
	}{
		// Called out by name in ruel#44's acceptance criteria.
		{"gpt-5-mini", "openai:gpt-5-mini", "openai:gpt-5", "gpt-5 is 5x gpt-5-mini"},
		{"gpt-5-nano", "openai:gpt-5-nano", "openai:gpt-5", "gpt-5 is 25x gpt-5-nano"},
		{"gpt-5-codex", "openai:gpt-5-codex", "openai:gpt-5", "a distinct SKU, not a gpt-5 variant"},
		{"gpt-4o-mini", "openai:gpt-4o-mini", "openai:gpt-4o", "gpt-4o is 16x gpt-4o-mini"},
		{"o3-mini", "openai:o3-mini", "openai:o3", "o3 is ~2x o3-mini"},
		// Anthropic 4.x: the new pre-4.5 rows sit at $15 / $75 while every
		// later Opus 4.x is $5 / $25.
		{"claude-opus-4-1", "anthropic:claude-opus-4.1", "anthropic:claude-opus-4", "opus-4 is 3x every later opus-4.x"},
		{"claude-opus-4-5", "anthropic:claude-opus-4.5", "anthropic:claude-opus-4", "opus-4 is 3x opus-4.5"},
		{"claude-opus-4-8", "anthropic:claude-opus-4.8", "anthropic:claude-opus-4", "opus-4 is 3x opus-4.8"},
		{"claude-sonnet-4-5", "anthropic:claude-sonnet-4.5", "anthropic:claude-sonnet-4", "same rate today, but a different SKU that must not share a row"},
		// The pair the constant was written for.
		{"claude-fable-5-1", "anthropic:claude-fable-5-1", "anthropic:claude-fable-5", "fable-5 cache reads are 4x fable-5.1's"},
		// Zhipu: the base minor is a PREFIX of three dearer siblings and one
		// free one, so a bare-substring rule would misprice all four.
		{"glm-4.5-x", "zhipu:glm-4.5-x", "zhipu:glm-4.5", "glm-4.5-x is 3.7x glm-4.5"},
		{"glm-4.5-airx", "zhipu:glm-4.5-airx", "zhipu:glm-4.5-air", "airx is 5x air"},
		{"glm-4.5-air", "zhipu:glm-4.5-air", "zhipu:glm-4.5", "air is 3x cheaper than glm-4.5"},
		{"glm-4.5-flash", "zhipu:glm-4.5-flash", "zhipu:glm-4.5", "flash is free; glm-4.5 is not"},
		{"glm-4.7-flash", "zhipu:glm-4.7-flash", "zhipu:glm-4.7", "flash is free; glm-4.7 is not"},
		{"glm-4.7-flashx", "zhipu:glm-4.7-flashx", "zhipu:glm-4.7-flash", "flashx bills; flash does not"},
		{"glm-5.1", "zhipu:glm-5.1", "zhipu:glm-5", "glm-5.1 is 1.4x glm-5 on input"},
		{"glm-5-turbo", "zhipu:glm-5-turbo", "zhipu:glm-5", "turbo is a distinct SKU, not a glm-5 variant"},
		// Cursor: the base minor is a prefix of both a 6x and a 3x sibling.
		{"cursor/composer-2.5", "cursor:composer-2.5", "cursor:composer-2", "composer-2 is 1x, but a different SKU"},
		{"cursor/composer-2.5-fast", "cursor:composer-2.5-fast", "cursor:composer-2.5", "2.5-fast is 6x composer-2.5"},
		{"cursor/composer-2-fast", "cursor:composer-2-fast", "cursor:composer-2", "2-fast is 3x composer-2"},
		{"cursor/composer-1.5", "cursor:composer-1.5", "cursor:composer-1", "1.5 is 2.8x composer-1"},
	}

	for _, tc := range pairs {
		got, ok := PriceForModelAlias(tc.id)
		if !ok {
			t.Errorf("%s: unresolved", tc.id)
			continue
		}
		key := got.Provider + ":" + got.Model
		if key != tc.wantKey {
			t.Errorf("%s resolved to %s, want %s — %s", tc.id, key, tc.wantKey, tc.why)
		}
		if key == tc.borrowed {
			t.Errorf("%s borrowed %s — %s", tc.id, tc.borrowed, tc.why)
		}
	}
}

// TestNewOpenAIRowsDoNotBorrowALegacyDashId: the rules added in ruel#44 are
// anchored so they cannot swallow a neighbouring legacy id. `gpt-5` must not
// reach `gpt-5-4` / `gpt-5-5`, and `gpt-5-mini` must not reach
// `gpt-5-4-mini` — each of those has its own row at its own rate.
func TestNewOpenAIRowsDoNotBorrowALegacyDashId(t *testing.T) {
	for _, tc := range []struct {
		id      string
		wantKey string
	}{
		{"gpt-5-4", "openai:gpt-5.4"},
		{"gpt-5-5", "openai:gpt-5.5"},
		{"gpt-5-4-mini", "openai:gpt-5.4-mini"},
		{"gpt-5-2-codex", "openai:gpt-5.2-codex"},
		{"gpt-5-3-codex", "openai:gpt-5.3-codex"},
	} {
		got, ok := PriceForModelAlias(tc.id)
		if !ok {
			t.Errorf("%q: unresolved", tc.id)
			continue
		}
		if key := got.Provider + ":" + got.Model; key != tc.wantKey {
			t.Errorf("%q resolved to %s, want %s", tc.id, key, tc.wantKey)
		}
	}
	// And the dot-only rows must not gain the legacy dash tolerance in the other
	// direction: a dashed spelling of an id whose canonical form carries a dot
	// is not a SKU anyone sells. (gpt-6-sol / gpt-6-luna / gpt-6-astra have no
	// dot to begin with, so their dashed form IS the canonical one.)
	for _, id := range []string{"gpt-5-6-sol", "gpt-5-6-terra", "gpt-5-6-luna", "gpt-6-1-sol"} {
		if p, ok := PriceForModelAlias(id); ok {
			t.Errorf("%q resolved to %s:%s — the 5.6+ rows are deliberately dot-only", id, p.Provider, p.Model)
		}
	}
}

// TestLegacyOpenAIDashToleranceIsFrozen pins a divergence that PREDATES ruel#44
// and is deliberately left alone: five older OpenAI rules accept a DASHED
// spelling (`gpt-5-4`) of a dotted SKU (`gpt-5.4`), while the dashboard treats
// OpenAI's separator as semantic and leaves such an id unmapped.
//
// Why frozen rather than fixed: making the server strict would flip every real
// usage row that reports the dashed form from priced to unpriced, and unpriced
// rows are exactly what silently disables a budget gate (ruel#24). Paying a
// rate is the safe direction to be wrong in; dropping to unpriced is not. So
// this is recorded and bounded, not quietly "fixed" and not quietly kept.
//
// What the freeze buys: the divergence cannot SPREAD. Adding another
// OpenAI rule with `[.-]` — or porting the tolerance to a new row — turns this
// red, because the id would resolve here while the dashboard still has no key
// for it.
func TestLegacyOpenAIDashToleranceIsFrozen(t *testing.T) {
	fe := parseFrontendPricing(t, pricingFrontendPath(t))

	known := map[string]string{
		"gpt-5-5":       "openai:gpt-5.5",
		"gpt-5-4":       "openai:gpt-5.4",
		"gpt-5-4-mini":  "openai:gpt-5.4-mini",
		"gpt-5-3-codex": "openai:gpt-5.3-codex",
		"gpt-5-2-codex": "openai:gpt-5.2-codex",
	}
	for id, wantKey := range known {
		got, ok := PriceForModelAlias(id)
		if !ok {
			t.Errorf("%q: expected the legacy dashed form to resolve (frozen behaviour)", id)
			continue
		}
		if key := got.Provider + ":" + got.Model; key != wantKey {
			t.Errorf("%q resolved to %s, want %s", id, key, wantKey)
		}
		if _, ok := fe[id]; ok {
			t.Errorf("%q: the dashboard now prices this dashed id too — the divergence is gone, so drop it from this frozen list", id)
		}
	}
}

// TestGenericIdsStayUnpricedWithoutAProvider is the guard for the new
// provider parameter on EstimateUsageCost, and it is the one test in this file
// that is about a MISPRICE rather than a coverage gap.
//
// Cursor and codex both report the literal model id `auto` — that is not a
// guess, it is called out in ModelPlaceholderValues in model_completeness.go.
// Cursor's `auto` is $1.25 / $6; codex's is whatever codex routes to, which
// this table does not know. A rule that resolved the BARE id would therefore
// bill every codex `auto` run at Cursor's rate, and the coverage guard would
// not notice: the id resolves, the rates are internally consistent, and the
// only symptom is a wrong number on someone else's invoice.
//
// So the property pinned here is directional: the same id must price under its
// own provider and stay unpriced under any other.
func TestGenericIdsStayUnpricedWithoutAProvider(t *testing.T) {
	qualified := []struct {
		model    string
		provider string
		wantKey  string
	}{
		{"auto", "cursor", "cursor:auto"},
		{"composer-1", "cursor", "cursor:composer-1"},
		{"composer-2.5", "cursor", "cursor:composer-2.5"},
	}
	for _, tc := range qualified {
		got, ok := PriceForModel(tc.model, tc.provider)
		if !ok {
			t.Errorf("PriceForModel(%q, %q): unresolved, want %s", tc.model, tc.provider, tc.wantKey)
			continue
		}
		if key := got.Provider + ":" + got.Model; key != tc.wantKey {
			t.Errorf("PriceForModel(%q, %q) = %s, want %s", tc.model, tc.provider, key, tc.wantKey)
		}
	}

	// The same ids under a provider that does not own them, plus bare.
	for _, tc := range []struct {
		model    string
		provider string
	}{
		{"auto", "codex"},
		{"auto", "openai"},
		{"auto", ""},
		{"composer-1", "openai"},
		{"composer-2.5", "codex"},
		{"composer-2.5", ""},
	} {
		if got, ok := PriceForModel(tc.model, tc.provider); ok {
			t.Errorf("PriceForModel(%q, %q) resolved to %s:%s — a generic id must stay unpriced outside the provider that owns it",
				tc.model, tc.provider, got.Provider, got.Model)
		}
	}

	// And the consequence that actually matters, stated as a cost: a codex
	// `auto` run must come back unpriced rather than picking up Cursor's rate.
	// Unpriced is the safe failure — it is counted and surfaced, and it does
	// not silently feed a wrong number into a budget gate.
	got := EstimateUsageCost("auto", "codex", 0, 100_000, 5_000, 200_000, 0)
	if got.Source != CostSourceUnpriced {
		t.Errorf("codex `auto`: source = %q, want %q", got.Source, CostSourceUnpriced)
	}
	if got.Priceable() {
		t.Error("codex `auto` is Priceable(); it must stay out of medians and budgets")
	}
	cursor := EstimateUsageCost("auto", "cursor", 0, 100_000, 5_000, 200_000, 0)
	if cursor.Source != CostSourceTable || cursor.USD <= 0 {
		t.Errorf("cursor `auto`: source = %q USD = %v, want table and > 0", cursor.Source, cursor.USD)
	}
}

// TestFreeTiersResolveToZeroNotUnpriced pins the reason the two all-zero glm
// flash rows are in the table at all. It would be "simpler" to leave an id out
// when its rates are all 0 — and it would be wrong. Under the four-state cost
// model an unknown id is CostSourceUnpriced, which is excluded from medians and
// from every budget gate; a known-free id is CostSourceZero, which is priced
// and belongs in a median. Leaving a free tier out of the table is therefore
// not neutral: it converts rows that genuinely cost nothing into rows that
// disable the gate they flow into (ruel#24's failure mode, re-entering from the
// other side).
func TestFreeTiersResolveToZeroNotUnpriced(t *testing.T) {
	const in, out, cacheRead = 43_068, 1_198, 240_128
	for _, id := range []string{"glm-4.5-flash", "glm-4.7-flash"} {
		price, ok := PriceForModelAlias(id)
		if !ok {
			t.Errorf("%q: unresolved — a free tier must be in the table, not absent from it", id)
			continue
		}
		if price.InputPerM != 0 || price.OutputPerM != 0 || price.CacheReadPerM != 0 || price.CacheWritePerM != 0 {
			t.Errorf("%q: rates are not all zero (%+v); this test is about the free tiers", id, price)
		}
		got := EstimateUsageCost(id, "", 0, in, out, cacheRead, 0)
		if got.Source != CostSourceZero {
			t.Errorf("%q: source = %q, want %q", id, got.Source, CostSourceZero)
		}
		if !got.Priceable() {
			t.Errorf("%q: not Priceable() — a known-free row belongs in a median as a real 0", id)
		}
		if got.USD != 0 {
			t.Errorf("%q: USD = %v, want 0", id, got.USD)
		}
	}
}
