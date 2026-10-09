package metrics

// ruel#44, last step: the web UI's price table is rendered from the server's.
// These tests are what makes that safe rather than merely convenient.
//
// The failure mode a generated file invites is not drift — it is AMNESIA:
// somebody edits a rate, nobody runs the generator, and the two copies quietly
// disagree again, which is the exact state this whole Issue was opened to end.
// TestGeneratedFrontendPricingIsCommitted makes the committed file a cache
// that must be refreshed, and TestFrontendPricingKeysResolveBackToTheirRow
// makes a declared key prove it is reachable.

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// TestGeneratedFrontendPricingIsCommitted is the amnesia guard: render the
// table into memory and compare it byte-for-byte with what is on disk.
//
// It compares with CRLF normalized because Windows checkouts rewrite line
// endings. A guard that fails for a reason unrelated to the rates gets muted
// or skipped, and a muted guard is worse than none — it looks like it is
// working.
func TestGeneratedFrontendPricingIsCommitted(t *testing.T) {
	rendered, err := GeneratedFrontendPricing()
	if err != nil {
		t.Fatalf("GeneratedFrontendPricing: %v", err)
	}
	path := pricingFrontendPath(t)
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.ReplaceAll(string(committed), "\r\n", "\n") != rendered {
		t.Fatalf("%s is stale.\n\nRun `go run ./cmd/gen_model_prices` from the repository root and commit the result.\n"+
			"A rate that exists in one table and not the other is the bug this file exists to prevent.", path)
	}
}

// TestFrontendPricingKeysResolveBackToTheirRow pins the property a generated
// table cannot guarantee by construction: that the key it emits is a key the
// server's own resolver can REACH.
//
// The generator writes whatever frontendPricingKeys says. A typo there produces
// a perfectly well-formed table whose rows the server cannot resolve — the
// dashboard would price the model, the budget gate would call it unpriced, and
// both tables would look complete. That is the same coverage gap ruel#44
// closed, re-entering through the metadata instead of the data.
func TestFrontendPricingKeysResolveBackToTheirRow(t *testing.T) {
	if len(frontendPricingKeys) == 0 {
		t.Fatal("frontendPricingKeys is empty; the generated table would be empty too")
	}

	rows := make([]string, 0, len(frontendPricingKeys))
	for k := range frontendPricingKeys {
		rows = append(rows, k)
	}
	sort.Strings(rows)

	for _, row := range rows {
		want, ok := modelPrices[row]
		if !ok {
			t.Errorf("frontendPricingKeys names %q, which is not a row in modelPrices", row)
			continue
		}
		keys := frontendPricingKeys[row]
		if len(keys) == 0 {
			t.Errorf("%q declares no frontend key; the row would vanish from the generated table", row)
			continue
		}
		for _, key := range keys {
			got, ok := PriceForModelAlias(key)
			if !ok {
				t.Errorf("%q: frontend key %q does not resolve through the alias rules — the dashboard would price it and the server would not", row, key)
				continue
			}
			if got.Provider+":"+got.Model != row {
				t.Errorf("%q: frontend key %q resolves to %s:%s instead", row, key, got.Provider, got.Model)
			}
			if got.InputPerM != want.InputPerM || got.OutputPerM != want.OutputPerM ||
				got.CacheReadPerM != want.CacheReadPerM || got.CacheWritePerM != want.CacheWritePerM {
				t.Errorf("%q: frontend key %q resolves to the right row but different rates", row, key)
			}
		}
	}

	// The other half: a row with no key at all. The generator refuses to
	// render, but that turns a silent gap into a build failure only if
	// somebody runs it — assert it here too so it fails in CI.
	var missing []string
	for k := range modelPrices {
		if len(frontendPricingKeys[k]) == 0 {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d server row(s) have no frontend key and would be dropped: %v", len(missing), missing)
	}
}

// TestGeneratedTableCoversEveryFrontendRow closes the loop in the direction
// the byte comparison does not: the committed file must contain one entry per
// declared key, no more and no fewer. A generator that deduplicated two rows
// onto one key, or emitted a key twice, still renders fine — the difference
// only shows up as a missing row in the UI.
func TestGeneratedTableCoversEveryFrontendRow(t *testing.T) {
	fe := parseFrontendPricing(t, pricingFrontendPath(t))

	want := map[string]bool{}
	for _, keys := range frontendPricingKeys {
		for _, k := range keys {
			want[k] = true
		}
	}
	if len(fe) != len(want) {
		t.Errorf("generated table has %d rows, want %d", len(fe), len(want))
	}
	for k := range want {
		if _, ok := fe[k]; !ok {
			t.Errorf("generated table is missing key %q", k)
		}
	}
	for k := range fe {
		if !want[k] {
			t.Errorf("generated table has key %q, which no server row declares", k)
		}
	}
}
