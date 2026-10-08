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
func frontendKeyCandidates(p ModelPrice) []string {
	dashed := strings.ReplaceAll(p.Model, ".", "-")
	return []string{
		p.Model,
		dashed,
		p.Provider + "-" + p.Model,
		p.Provider + "-" + dashed,
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

// TestFrontendPricingCoverageGapIsOnlyTheKnownSet freezes the set of SKUs only
// one side prices. Both directions are real gaps: a frontend-only row means the
// UI shows a cost the server calls unpriced (so it never reaches a budget
// gate), and a server-only row means the gate bills a model the UI shows as $0.
// Neither is fixed here — closing them needs a provider and an alias rule per
// row, which is a data job (ruel#44). What this test buys is that the gap
// cannot GROW unnoticed: adding a row to either table without porting it turns
// this red.
func TestFrontendPricingCoverageGapIsOnlyTheKnownSet(t *testing.T) {
	path := pricingFrontendPath(t)
	fe := parseFrontendPricing(t, path)

	matched := map[string]bool{}
	for _, sp := range modelPrices {
		for _, c := range frontendKeyCandidates(sp) {
			matched[c] = true
		}
	}

	var onlyFrontend []string
	for k := range fe {
		if !matched[k] {
			onlyFrontend = append(onlyFrontend, k)
		}
	}
	sort.Strings(onlyFrontend)

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

// Frozen as of ruel#39. Shrinking either list is a win and just needs the
// constant updated; growing it is exactly what this test exists to stop.
var knownFrontendOnly = []string{
	"claude-haiku-3-5", "claude-opus-4", "claude-opus-4-1", "claude-sonnet-4",
	"cursor", "cursor/auto", "cursor/composer-1", "cursor/composer-1.5",
	"cursor/composer-2", "cursor/composer-2-fast", "cursor/composer-2.5",
	"cursor/composer-2.5-fast",
	"deepseek-chat", "deepseek-reasoner",
	"glm-4.5", "glm-4.5-air", "glm-4.5-airx", "glm-4.5-flash", "glm-4.5-x",
	"glm-4.6", "glm-4.7", "glm-4.7-flash", "glm-4.7-flashx",
	"glm-5-turbo", "glm-5.1",
	"gpt-4o", "gpt-4o-mini", "gpt-5", "gpt-5-codex", "gpt-5-mini", "gpt-5-nano",
	"kimi-k2.6", "kimi/k3", "o3", "o3-mini", "o4-mini",
}

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
		server := EstimateUsageCost(tc.model, 0, tc.input, tc.output, tc.cacheRead, tc.cacheWrite)
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
