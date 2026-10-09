package metrics

// The web UI's price table is GENERATED from this package's rate table.
//
// Two copies of a price table are two prices. They were kept in sync by hand
// and by a test that failed when they drifted (ruel#39, ruel#44) — which is
// better than nothing, but it is still two copies: every correction had to be
// made twice, and the gap between them was 36 rows in one direction and 7 in
// the other before anyone counted.
//
// Now there is one table. modelPrices holds the numbers, frontendPricingKeys
// holds the spellings, and this file renders
// packages/views/runtimes/model_pricing.generated.ts. The committed file is
// checked byte-for-byte against a fresh render by
// TestGeneratedFrontendPricingIsCommitted, so "regenerate and forget" is not
// an available move: forgetting turns CI red.
//
// What is deliberately NOT generated: the resolver in utils.ts
// (`resolvePricing`, the date/context-tag stripping, the provider-qualified
// candidate order) and its comments. Those are lookup BEHAVIOUR, they belong
// to the frontend, and their comments explain why the two sides must agree on
// which key wins. Only the data moved.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// generatedFrontendPricingPath is where the rendered table is written,
// relative to the repository root.
const generatedFrontendPricingRel = "packages/views/runtimes/model_pricing.generated.ts"

// GeneratedFrontendPricingRel exposes the path so cmd/gen_model_prices does
// not have to carry its own copy of it — a path in two places is the same
// duplicate-source problem this file exists to remove.
func GeneratedFrontendPricingRel() string { return generatedFrontendPricingRel }

// GeneratedFrontendPricing renders the frontend price table from modelPrices.
//
// Rows are emitted in one flat, sorted block. The hand-written table was
// grouped by provider with a comment per block, and that grouping carried real
// information (sources, why cacheWrite mirrors input for some vendors). It
// moved to pricing.go alongside the rows it describes, which is where a
// maintainer editing a rate will now look — a comment that has to be kept in
// sync in two files is the same duplicate-source problem in prose form.
func GeneratedFrontendPricing() (string, error) {
	rows := make([]string, 0, len(modelPrices))
	keys := make([]string, 0, len(modelPrices))
	for k := range modelPrices {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	seen := map[string]string{}
	for _, rowKey := range keys {
		price := modelPrices[rowKey]
		feKeys, ok := frontendPricingKeys[rowKey]
		if !ok || len(feKeys) == 0 {
			// A row with no key would silently vanish from the generated
			// table: the server would price the model, the dashboard would
			// show $0, and no test would compare them because there would be
			// nothing on one side to compare. Refuse to render.
			return "", fmt.Errorf("server row %q has no frontend key; add one to frontendPricingKeys", rowKey)
		}
		for _, feKey := range feKeys {
			if owner, dup := seen[feKey]; dup {
				return "", fmt.Errorf("frontend key %q is claimed by both %q and %q", feKey, owner, rowKey)
			}
			seen[feKey] = rowKey
			rows = append(rows, fmt.Sprintf("  %s: { input: %s, output: %s, cacheRead: %s, cacheWrite: %s },",
				strconv.Quote(feKey),
				num(price.InputPerM), num(price.OutputPerM),
				num(price.CacheReadPerM), num(price.CacheWritePerM)))
		}
	}
	sort.Strings(rows)

	var b strings.Builder
	b.WriteString(`// GENERATED FILE — DO NOT EDIT.
//
// Source of truth: server/internal/metrics/pricing.go (the rate table
// modelPrices) and pricing_frontend_keys.go (the key spellings). Regenerate
// with:
//
//     go run ./cmd/gen_model_prices
//
// from the repository root, or ` + "`make gen-prices`" + ` if that target exists.
//
// Editing this file by hand will be caught:
// TestGeneratedFrontendPricingIsCommitted renders the table into a buffer and
// compares it byte-for-byte with what is committed here.
//
// Why it is generated at all: for a long time the web UI carried its own copy
// of this table, and the two copies disagreed in both directions — 36 SKUs
// only the dashboard priced, 7 only the budget gate priced. A rate edited on
// one side was a rate edited wrong. Two copies of a price table are two
// prices.

export type ModelPricing = {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
};

// Pricing per million tokens (USD).
export const MODEL_PRICING: Record<string, ModelPricing> = {
`)
	b.WriteString(strings.Join(rows, "\n"))
	b.WriteString("\n};\n")
	return b.String(), nil
}

// num formats a rate the way a TypeScript literal wants it: shortest exact
// decimal, no exponent, no trailing zeros. %v would render 0.0000001 as
// 1e-07, which is valid JS but unreadable in a diff.
func num(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
