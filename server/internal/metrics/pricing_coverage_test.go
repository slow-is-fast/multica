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
