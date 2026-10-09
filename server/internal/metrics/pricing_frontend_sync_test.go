package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file pins the server price table in pricing.go against the one the web
// UI reads, packages/views/runtimes/utils.ts. Two copies of a price table are
// two prices: a Run can show one number on screen while the budget gate bills
// another, and nothing notices. pricing.go asks maintainers to "keep the two
// tables in sync" in comments; this turns that into a test.
//
// Refs ruel#39.

const pricingFrontendRel = "packages/views/runtimes/utils.ts"

// pricingFrontendPath resolves the frontend table from server/internal/metrics.
// A guard that cannot find what it guards must fail, not skip: skipping is how
// a sync check quietly stops checking.
func pricingFrontendPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", "..", ".."))
	p := filepath.Join(root, pricingFrontendRel)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("frontend pricing table not found at %s: %v", p, err)
	}
	return p
}

type frontendPrice struct {
	input      float64
	output     float64
	cacheRead  float64
	cacheWrite float64
}

// Matches one MODEL_PRICING row, e.g.
//
//	"gpt-5.6-sol": { input: 5, output: 30, cacheRead: 0.50, cacheWrite: 6.25 },
var frontendPriceLine = regexp.MustCompile(`^\s*"([^"]+)"\s*:\s*\{\s*input:\s*([0-9.]+),\s*output:\s*([0-9.]+),\s*cacheRead:\s*([0-9.]+),\s*cacheWrite:\s*([0-9.]+)\s*\},?\s*$`)

func parseFrontendPricing(t *testing.T, path string) map[string]frontendPrice {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")

	start := -1
	for i, l := range lines {
		if strings.Contains(l, "const MODEL_PRICING") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("MODEL_PRICING not found in %s — the guard's parser no longer matches the file", path)
	}

	out := map[string]frontendPrice{}
	num := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatalf("parse %q in %s: %v", s, path, err)
		}
		return v
	}
	for i := start; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "};" {
			break
		}
		m := frontendPriceLine.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		out[m[1]] = frontendPrice{
			input:      num(m[2]),
			output:     num(m[3]),
			cacheRead:  num(m[4]),
			cacheWrite: num(m[5]),
		}
	}
	return out
}

// frontendKeyCandidates maps a server row onto the spellings the frontend table
// may use for the same SKU. The two tables disagree on separators: the Go table
// keeps Anthropic's dotted version (claude-opus-4.5) while the frontend
// normalizes it to dashes (claude-opus-4-5); and the frontend carries a family
// prefix on some ids (deepseek-v4-pro) that the Go Model field drops (v4-pro).
// Without these candidates the guard would silently compare nothing for those
// SKUs — a sync check that compares nothing is worse than no check, because it
// looks like it is working.
//
// The slash-qualified pair was added in ruel#44 and is not decoration: the
// frontend keys an id whose bare name is generic ACROSS PROVIDERS as
// `<provider>/<model>` (`cursor/auto`, `kimi/k3`) precisely so it cannot
// collide — see `resolvePricing` in packages/views/runtimes/utils.ts. A row
// like cursor:auto therefore has no bare frontend key at all, and without this
// candidate the guard would report it as server-only while the dashboard is in
// fact pricing it.
func frontendKeyCandidates(p ModelPrice) []string {
	dashed := strings.ReplaceAll(p.Model, ".", "-")
	return []string{
		p.Model,
		dashed,
		p.Provider + "-" + p.Model,
		p.Provider + "-" + dashed,
		p.Provider + "/" + p.Model,
		p.Provider + "/" + dashed,
	}
}

// TestFrontendPricingMatchesServerOnSharedRows is the drift guard: wherever
// both tables price a model, they must price it identically.
func TestFrontendPricingMatchesServerOnSharedRows(t *testing.T) {
	path := pricingFrontendPath(t)
	fe := parseFrontendPricing(t, path)
	if len(fe) < 40 {
		t.Fatalf("parsed only %d rows from %s — expected the full table; the parser is broken and this guard is now vacuous", len(fe), path)
	}

	keys := make([]string, 0, len(modelPrices))
	for k := range modelPrices {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	shared := 0
	var onlyServer []string
	var conflicts []string
	for _, k := range keys {
		sp := modelPrices[k]
		var hit frontendPrice
		var used string
		found := false
		for _, c := range frontendKeyCandidates(sp) {
			if v, ok := fe[c]; ok {
				hit, used, found = v, c, true
				break
			}
		}
		if !found {
			onlyServer = append(onlyServer, fmt.Sprintf("%s (tried %v)", k, frontendKeyCandidates(sp)))
			continue
		}
		shared++

		var diffs []string
		check := func(field string, got, want float64) {
			if got != want {
				diffs = append(diffs, fmt.Sprintf("%s: server=%.4f frontend=%.4f", field, got, want))
			}
		}
		check("input", sp.InputPerM, hit.input)
		check("output", sp.OutputPerM, hit.output)
		check("cacheRead", sp.CacheReadPerM, hit.cacheRead)
		check("cacheWrite", sp.CacheWritePerM, hit.cacheWrite)
		if len(diffs) > 0 {
			conflicts = append(conflicts, fmt.Sprintf("  %s  (frontend key %q)\n    %s", k, used, strings.Join(diffs, "\n    ")))
		}
	}

	if shared < 20 {
		t.Fatalf("only %d of %d server rows matched a frontend row — the key candidates no longer line up, so this guard is not comparing what it claims to", shared, len(modelPrices))
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		t.Fatalf("%d SKU(s) are priced differently on the two sides:\n%s\n\nSame model, two prices: the UI shows one number and the budget gate bills another.", len(conflicts), strings.Join(conflicts, "\n"))
	}
	t.Logf("compared %d shared SKUs; %d server rows have no frontend row yet", shared, len(onlyServer))
	for _, s := range onlyServer {
		t.Logf("  server-only: %s", s)
	}
}

// TestFrontendPricingCoverageGapIsOnlyTheKnownSet freezes the set of models
// only one side can price. Both directions are real gaps: a frontend-only model
// means the UI shows a cost the server calls unpriced (so it never reaches a
// budget gate), and a server-only row means the gate bills a model the UI shows
// as $0. Neither is closed here — that needs a provider and an alias rule per
// row, which is a data job (ruel#44). What this test buys is that the gap
// cannot GROW unnoticed: adding a row to either side without porting it turns
// this red.
//
// ## Why "frontend-only" measures RESOLVABILITY, not table keys
//
// The first version of this guard asked whether any server row's NAME lined up
// with the frontend key. That reported 36 gaps, but 3 of them were artifacts:
// `deepseek-chat`, `deepseek-reasoner` and `kimi/k3` are the frontend's own
// spellings of SKUs the server already prices through its alias rules — and at
// identical rates. The server was right; the guard's key matching was wrong.
//
// What matters is not whether the two tables contain a row with a matching
// name, but whether both sides arrive at the same dollars for the same model
// id. So this asks PriceForModelAlias directly: can the server price this id,
// and does it price it the same way? A row the server reaches through an alias
// is not a gap — duplicating it as its own row would create two rows for one
// SKU, which is its own drift risk.
func TestFrontendPricingCoverageGapIsOnlyTheKnownSet(t *testing.T) {
	path := pricingFrontendPath(t)
	fe := parseFrontendPricing(t, path)

	feKeys := make([]string, 0, len(fe))
	for k := range fe {
		feKeys = append(feKeys, k)
	}
	sort.Strings(feKeys)

	var onlyFrontend, conflicts []string
	aligned := 0
	for _, k := range feKeys {
		fp := fe[k]
		sp, ok := PriceForModelAlias(k)
		if !ok {
			onlyFrontend = append(onlyFrontend, k)
			continue
		}
		if sp.InputPerM == fp.input && sp.OutputPerM == fp.output &&
			sp.CacheReadPerM == fp.cacheRead && sp.CacheWritePerM == fp.cacheWrite {
			aligned++
			continue
		}
		// Resolvable but at different rates is a bug, not a gap: the shared-row
		// guard above never sees these, because it only compares rows whose
		// names line up. Freezing them into a "known gap" list would hide a
		// real disagreement behind a list that is supposed to shrink.
		conflicts = append(conflicts, fmt.Sprintf("  %s -> %s:%s\n    frontend i=%.4f o=%.4f cr=%.4f cw=%.4f\n    server   i=%.4f o=%.4f cr=%.4f cw=%.4f",
			k, sp.Provider, sp.Model,
			fp.input, fp.output, fp.cacheRead, fp.cacheWrite,
			sp.InputPerM, sp.OutputPerM, sp.CacheReadPerM, sp.CacheWritePerM))
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		t.Fatalf("%d frontend key(s) resolve on the server to DIFFERENT rates:\n%s\n\nThese are disagreements, not coverage gaps — fix the rates, not the frozen list.", len(conflicts), strings.Join(conflicts, "\n"))
	}
	if aligned < 20 {
		t.Fatalf("only %d frontend keys resolve on the server — the alias rules or the frontend table parser broke; this guard is no longer measuring what it says", aligned)
	}

	// Server-only stays key-based: the frontend looks a model up BY KEY, so a
	// server row the frontend has no key for genuinely cannot be priced there.
	var onlyServer []string
	for _, sp := range modelPrices {
		found := false
		for _, c := range frontendKeyCandidates(sp) {
			if _, ok := fe[c]; ok {
				found = true
				break
			}
		}
		if !found {
			onlyServer = append(onlyServer, sp.Provider+":"+sp.Model)
		}
	}
	sort.Strings(onlyServer)

	assertSet(t, "frontend-only", onlyFrontend, knownFrontendOnly)
	assertSet(t, "server-only", onlyServer, knownServerOnly)
}

// Frozen as of ruel#44 (was 36/7 under the key-based guard; the three aliases
// the server already covered dropped out when the guard started measuring
// resolvability). Shrinking either list is a win and just needs the constant
// updated; growing it is exactly what this test exists to stop.
// Empty as of the GLM / Cursor / K2.6 batch of ruel#44 (was 20). Every
// frontend row the dashboard prices is now reachable on the server at the same
// rates, which is the acceptance criterion the Issue set out. The list stays
// rather than being deleted: the reverse direction is still open (see
// knownServerOnly), and a future frontend row with no server counterpart turns
// this red instead of silently widening the gap.
var knownFrontendOnly = []string{}

// Still 7 as of the server-side batch of ruel#44: the four Gemini rows, the
// two MiniMax rows and gpt-5.2-codex. Closed in the next commit, which ports
// them to packages/views/runtimes/utils.ts.
var knownServerOnly = []string{
	"google:gemini-2.5-flash", "google:gemini-2.5-pro",
	"google:gemini-3-flash", "google:gemini-3.1-pro",
	"minimax:m2.7", "minimax:m2.7-highspeed",
	"openai:gpt-5.2-codex",
}

// TestEstimateUsageCostAgreesWithFrontendFormula closes the loop the table
// comparison cannot: the same usage row must cost the same dollars on both
// sides. Comparing rates only says the two tables agree; comparing an actual
// row proves the RESOLVER picks the same row and the arithmetic matches.
//
// The token counts are real rows from the local instance (see
// docs/reference/multica-m5-retrospective.md) — the two SKUs this machine
// actually runs — plus the five rows whose rates this change corrected.
func TestEstimateUsageCostAgreesWithFrontendFormula(t *testing.T) {
	fe := parseFrontendPricing(t, pricingFrontendPath(t))

	cases := []struct {
		model                                string
		frontendKey                          string
		input, output, cacheRead, cacheWrite int64
	}{
		{"gpt-5.6-sol", "gpt-5.6-sol", 14817, 825, 133632, 0},
		{"glm-5", "glm-5", 64389, 1257, 54784, 0},
		// Rows this change corrected. gpt-5.2-codex is included even though
		// only the server carries it: it had the same cacheWrite == cacheRead
		// defect, fixed for consistency, and this pins the resulting rate.
		{"gpt-5.5", "gpt-5.5", 10000, 1000, 50000, 20000},
		{"gpt-5.4", "gpt-5.4", 10000, 1000, 50000, 20000},
		{"gpt-5.4-mini", "gpt-5.4-mini", 10000, 1000, 50000, 20000},
		{"gpt-5.3-codex", "gpt-5.3-codex", 10000, 1000, 50000, 20000},
		{"deepseek-v4-flash", "deepseek-v4-flash", 10000, 1000, 50000, 20000},
	}

	for _, tc := range cases {
		fp, ok := fe[tc.frontendKey]
		if !ok {
			t.Fatalf("frontend table has no key %q — parser or table changed", tc.frontendKey)
		}
		server := EstimateUsageCost(tc.model, "", 0, tc.input, tc.output, tc.cacheRead, tc.cacheWrite)
		if server.Source == CostSourceUnpriced {
			t.Fatalf("%s: server resolved no rate — the alias rules no longer reach this row", tc.model)
		}
		// estimateCost in utils.ts, with no provider-reported cost and no
		// uncosted_* split (an older backend), i.e. the whole row estimated:
		want := (float64(tc.input)*fp.input +
			float64(tc.output)*fp.output +
			float64(tc.cacheRead)*fp.cacheRead +
			float64(tc.cacheWrite)*fp.cacheWrite) / 1e6
		if diff := server.USD - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: server $%.10f vs frontend $%.10f (diff %.10f) — same row, two prices",
				tc.model, server.USD, want, diff)
		}
	}
}

func assertSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s set changed: now %d rows, frozen at %d\n  now:   %v\n  frozen: %v\n\nPort the new row to the other side (ruel#44), or explain the gap here.",
			label, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s set changed at position %d: got %q, frozen %q\n  now:   %v\n  frozen: %v",
				label, i, got[i], want[i], got, want)
		}
	}
}
