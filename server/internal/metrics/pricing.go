package metrics

import (
	"regexp"
	"strings"
)

// CostUSDTicksPerUSD is the scale of provider-reported costs: xAI reports
// whole ticks of 1e-10 USD. Declared here rather than imported from pkg/agent
// (which owns the wire-format parsing) so this package keeps no dependency on
// the agent runtime for a physical unit; the two must stay equal.
const CostUSDTicksPerUSD = 10_000_000_000

type ModelPrice struct {
	Provider       string
	Model          string
	InputPerM      float64
	CacheReadPerM  float64
	CacheWritePerM float64
	OutputPerM     float64
}

var modelPrices = map[string]ModelPrice{
	// GPT-5.6 series and GPT-6 Astra (Codex `codex` provider). Official rates
	// from OpenAI's GPT-5.6 announcement (openai.com/index/previewing-gpt-5-6-sol)
	// and Astra's published $10 / $50. For 5.6+ (including Astra) cache read is
	// generally the 90%-off cached-input rate (0.1x input). Cache write is billed
	// at 1.25x the uncached input rate — unlike earlier OpenAI SKUs, which don't
	// bill cache writes separately. Codex app-server v0.147 reports cache-write
	// input separately while including it in the raw input total; the collector
	// normalizes those into mutually exclusive billing buckets.
	"openai:gpt-6-astra": {Provider: "openai", Model: "gpt-6-astra", InputPerM: 10.00, CacheReadPerM: 1.00, CacheWritePerM: 12.50, OutputPerM: 50.00},
	// Standard short-context rates from developers.openai.com/api/docs/models/.
	// GPT-6.1 Sol caches at 0.05x input, not GPT-6 Sol's 0.1x.
	"openai:gpt-6.1-sol":   {Provider: "openai", Model: "gpt-6.1-sol", InputPerM: 2.00, CacheReadPerM: 0.10, CacheWritePerM: 2.50, OutputPerM: 10.00},
	"openai:gpt-6-sol":     {Provider: "openai", Model: "gpt-6-sol", InputPerM: 2.00, CacheReadPerM: 0.20, CacheWritePerM: 2.50, OutputPerM: 10.00},
	"openai:gpt-6-luna":    {Provider: "openai", Model: "gpt-6-luna", InputPerM: 0.10, CacheReadPerM: 0.01, CacheWritePerM: 0.125, OutputPerM: 0.50},
	"openai:gpt-5.6-sol":   {Provider: "openai", Model: "gpt-5.6-sol", InputPerM: 5.00, CacheReadPerM: 0.50, CacheWritePerM: 6.25, OutputPerM: 30.00},
	"openai:gpt-5.6-terra": {Provider: "openai", Model: "gpt-5.6-terra", InputPerM: 2.50, CacheReadPerM: 0.25, CacheWritePerM: 3.125, OutputPerM: 15.00},
	"openai:gpt-5.6-luna":  {Provider: "openai", Model: "gpt-5.6-luna", InputPerM: 1.00, CacheReadPerM: 0.10, CacheWritePerM: 1.25, OutputPerM: 6.00},
	// Cache writes on these five SKUs bill as ordinary input: OpenAI publishes
	// no separate cache-write rate for them (see the gpt-6-astra note above),
	// so a cache-write token costs what an input token costs and
	// CacheWritePerM mirrors InputPerM — contrast the 5.6-and-later rows
	// ABOVE, which do bill cache writes separately at 1.25x input.
	//
	// It previously mirrored CacheReadPerM on all five, i.e. a tenth of the
	// real rate: every cached write was under-charged by 10x, and the four of
	// these that the frontend also carries disagreed with it. The signature
	// was that CacheWritePerM equalled CacheReadPerM exactly, which no
	// published rate would produce. TestFrontendPricingMatchesServerOnSharedRows now pins it.
	"openai:gpt-5.5":       {Provider: "openai", Model: "gpt-5.5", InputPerM: 5.00, CacheReadPerM: 0.50, CacheWritePerM: 5.00, OutputPerM: 30.00},
	"openai:gpt-5.4":       {Provider: "openai", Model: "gpt-5.4", InputPerM: 2.50, CacheReadPerM: 0.25, CacheWritePerM: 2.50, OutputPerM: 15.00},
	"openai:gpt-5.4-mini":  {Provider: "openai", Model: "gpt-5.4-mini", InputPerM: 0.75, CacheReadPerM: 0.075, CacheWritePerM: 0.75, OutputPerM: 4.50},
	"openai:gpt-5.3-codex": {Provider: "openai", Model: "gpt-5.3-codex", InputPerM: 1.75, CacheReadPerM: 0.175, CacheWritePerM: 1.75, OutputPerM: 14.00},
	"openai:gpt-5.2-codex": {Provider: "openai", Model: "gpt-5.2-codex", InputPerM: 1.75, CacheReadPerM: 0.175, CacheWritePerM: 1.75, OutputPerM: 14.00},
	// GPT-5 family, o-series and GPT-4o (ruel#44): the dashboard has carried
	// these all along while the Go table did not, so every one of them was a
	// row the UI priced and the budget gate called unpriced.
	//
	// `gpt-5` and `gpt-5-codex` share rates but are separate SKUs, and both are
	// priced 5x `gpt-5-mini` and 25x `gpt-5-nano`. That spread is the whole
	// reason these rules must END: `gpt-5` as a bare substring would swallow
	// `gpt-5-mini` and bill it at five times its real rate, silently. Same for
	// `gpt-4o` and `gpt-4o-mini` (16x) and `o3` / `o3-mini`.
	"openai:gpt-5":       {Provider: "openai", Model: "gpt-5", InputPerM: 1.25, CacheReadPerM: 0.125, CacheWritePerM: 1.25, OutputPerM: 10.00},
	"openai:gpt-5-codex": {Provider: "openai", Model: "gpt-5-codex", InputPerM: 1.25, CacheReadPerM: 0.125, CacheWritePerM: 1.25, OutputPerM: 10.00},
	"openai:gpt-5-mini":  {Provider: "openai", Model: "gpt-5-mini", InputPerM: 0.25, CacheReadPerM: 0.025, CacheWritePerM: 0.25, OutputPerM: 2.00},
	"openai:gpt-5-nano":  {Provider: "openai", Model: "gpt-5-nano", InputPerM: 0.05, CacheReadPerM: 0.005, CacheWritePerM: 0.05, OutputPerM: 0.40},
	"openai:o3":          {Provider: "openai", Model: "o3", InputPerM: 2.00, CacheReadPerM: 0.50, CacheWritePerM: 2.00, OutputPerM: 8.00},
	"openai:o3-mini":     {Provider: "openai", Model: "o3-mini", InputPerM: 1.10, CacheReadPerM: 0.55, CacheWritePerM: 1.10, OutputPerM: 4.40},
	"openai:o4-mini":     {Provider: "openai", Model: "o4-mini", InputPerM: 1.10, CacheReadPerM: 0.275, CacheWritePerM: 1.10, OutputPerM: 4.40},
	"openai:gpt-4o":      {Provider: "openai", Model: "gpt-4o", InputPerM: 2.50, CacheReadPerM: 1.25, CacheWritePerM: 2.50, OutputPerM: 10.00},
	"openai:gpt-4o-mini": {Provider: "openai", Model: "gpt-4o-mini", InputPerM: 0.15, CacheReadPerM: 0.075, CacheWritePerM: 0.15, OutputPerM: 0.60},
	// Anthropic's Sonnet 5 launch price is $2 / $10 through 2026-08-31. This
	// static table cannot schedule the published post-intro $3 / $15 change yet,
	// so keep the intro rate here and update the row when catalog support exists.
	"anthropic:claude-sonnet-5":   {Provider: "anthropic", Model: "claude-sonnet-5", InputPerM: 2.00, CacheReadPerM: 0.20, CacheWritePerM: 2.50, OutputPerM: 10.00},
	"anthropic:claude-fable-5-1":  {Provider: "anthropic", Model: "claude-fable-5-1", InputPerM: 10.00, CacheReadPerM: 0.25, CacheWritePerM: 12.50, OutputPerM: 50.00},
	"anthropic:claude-fable-5":    {Provider: "anthropic", Model: "claude-fable-5", InputPerM: 10.00, CacheReadPerM: 1.00, CacheWritePerM: 12.50, OutputPerM: 50.00},
	"anthropic:claude-opus-5-5":   {Provider: "anthropic", Model: "claude-opus-5-5", InputPerM: 4.00, CacheReadPerM: 0.20, CacheWritePerM: 5.00, OutputPerM: 20.00},
	"anthropic:claude-opus-5":     {Provider: "anthropic", Model: "claude-opus-5", InputPerM: 5.00, CacheReadPerM: 0.50, CacheWritePerM: 6.25, OutputPerM: 25.00},
	"anthropic:claude-opus-4.8":   {Provider: "anthropic", Model: "claude-opus-4.8", InputPerM: 5.00, CacheReadPerM: 0.50, CacheWritePerM: 6.25, OutputPerM: 25.00},
	"anthropic:claude-opus-4.7":   {Provider: "anthropic", Model: "claude-opus-4.7", InputPerM: 5.00, CacheReadPerM: 0.50, CacheWritePerM: 6.25, OutputPerM: 25.00},
	"anthropic:claude-opus-4.6":   {Provider: "anthropic", Model: "claude-opus-4.6", InputPerM: 5.00, CacheReadPerM: 0.50, CacheWritePerM: 6.25, OutputPerM: 25.00},
	"anthropic:claude-opus-4.5":   {Provider: "anthropic", Model: "claude-opus-4.5", InputPerM: 5.00, CacheReadPerM: 0.50, CacheWritePerM: 6.25, OutputPerM: 25.00},
	"anthropic:claude-sonnet-4.6": {Provider: "anthropic", Model: "claude-sonnet-4.6", InputPerM: 3.00, CacheReadPerM: 0.30, CacheWritePerM: 3.75, OutputPerM: 15.00},
	"anthropic:claude-sonnet-4.5": {Provider: "anthropic", Model: "claude-sonnet-4.5", InputPerM: 3.00, CacheReadPerM: 0.30, CacheWritePerM: 3.75, OutputPerM: 15.00},
	"anthropic:claude-haiku-4.5":  {Provider: "anthropic", Model: "claude-haiku-4.5", InputPerM: 1.00, CacheReadPerM: 0.10, CacheWritePerM: 1.25, OutputPerM: 5.00},
	// Pre-4.5 generations the dashboard has always carried and the Go table did
	// not (ruel#44). Opus 4 and 4.1 sit on the original $15 / $75 tier — nearly
	// three times Opus 4.5's — so an unanchored `claude-opus-4` rule would
	// over-bill every later Opus 4.x by 3x. That is the same tier-borrowing
	// trap as Fable 5 / 5.1, and it is why both rules end at versionEnd.
	"anthropic:claude-opus-4.1":   {Provider: "anthropic", Model: "claude-opus-4.1", InputPerM: 15.00, CacheReadPerM: 1.50, CacheWritePerM: 18.75, OutputPerM: 75.00},
	"anthropic:claude-opus-4":     {Provider: "anthropic", Model: "claude-opus-4", InputPerM: 15.00, CacheReadPerM: 1.50, CacheWritePerM: 18.75, OutputPerM: 75.00},
	"anthropic:claude-sonnet-4":   {Provider: "anthropic", Model: "claude-sonnet-4", InputPerM: 3.00, CacheReadPerM: 0.30, CacheWritePerM: 3.75, OutputPerM: 15.00},
	"anthropic:claude-haiku-3.5":  {Provider: "anthropic", Model: "claude-haiku-3.5", InputPerM: 0.80, CacheReadPerM: 0.08, CacheWritePerM: 1.00, OutputPerM: 4.00},
	"deepseek:v4-pro":             {Provider: "deepseek", Model: "v4-pro", InputPerM: 1.74, CacheReadPerM: 0.0145, CacheWritePerM: 1.74, OutputPerM: 3.48},
	"deepseek:v4-flash":           {Provider: "deepseek", Model: "v4-flash", InputPerM: 0.56, CacheReadPerM: 0.0112, CacheWritePerM: 0.56, OutputPerM: 1.12},
	"minimax:m2.7":                {Provider: "minimax", Model: "m2.7", InputPerM: 0.30, CacheReadPerM: 0.06, CacheWritePerM: 0.375, OutputPerM: 1.20},
	"minimax:m2.7-highspeed":      {Provider: "minimax", Model: "m2.7-highspeed", InputPerM: 0.60, CacheReadPerM: 0.06, CacheWritePerM: 0.375, OutputPerM: 2.40},
	"google:gemini-3-flash":       {Provider: "google", Model: "gemini-3-flash", InputPerM: 0.50, CacheReadPerM: 0.05, CacheWritePerM: 0.50, OutputPerM: 3.00},
	"google:gemini-3.1-pro":       {Provider: "google", Model: "gemini-3.1-pro", InputPerM: 2.00, CacheReadPerM: 0.20, CacheWritePerM: 2.00, OutputPerM: 12.00},
	"google:gemini-2.5-pro":       {Provider: "google", Model: "gemini-2.5-pro", InputPerM: 1.25, CacheReadPerM: 0.31, CacheWritePerM: 1.25, OutputPerM: 10.00},
	"google:gemini-2.5-flash":     {Provider: "google", Model: "gemini-2.5-flash", InputPerM: 0.30, CacheReadPerM: 0.03, CacheWritePerM: 0.30, OutputPerM: 2.50},
	// xAI Grok (docs.x.ai/developers/pricing). Short-context tier: xAI bills
	// a request at 2x once its prompt reaches 200K tokens, but a usage record
	// aggregates every model call in a turn, so it cannot say which tier any
	// individual request hit — pricing the standard tier under-estimates a
	// long-context turn by at most 50% instead of over-estimating a short one
	// by 100%. xAI publishes no separate cache-write rate (writes bill as
	// normal input), so CacheWrite mirrors Input. These rows mirror
	// packages/views/runtimes/utils.ts; keep the two tables in sync.
	// `grok-composer-*` ships in the Grok Build catalog but is absent from the
	// price sheet, so it stays unmapped rather than inheriting a guessed rate.
	"xai:grok-4.6":                     {Provider: "xai", Model: "grok-4.6", InputPerM: 2.00, CacheReadPerM: 0.50, CacheWritePerM: 2.00, OutputPerM: 6.00},
	"xai:grok-4.5":                     {Provider: "xai", Model: "grok-4.5", InputPerM: 2.00, CacheReadPerM: 0.30, CacheWritePerM: 2.00, OutputPerM: 6.00},
	"xai:grok-4.3":                     {Provider: "xai", Model: "grok-4.3", InputPerM: 1.25, CacheReadPerM: 0.20, CacheWritePerM: 1.25, OutputPerM: 2.50},
	"xai:grok-build-0.1":               {Provider: "xai", Model: "grok-build-0.1", InputPerM: 1.00, CacheReadPerM: 0.20, CacheWritePerM: 1.00, OutputPerM: 2.00},
	"xai:grok-4.20-multi-agent-0309":   {Provider: "xai", Model: "grok-4.20-multi-agent-0309", InputPerM: 1.25, CacheReadPerM: 0.20, CacheWritePerM: 1.25, OutputPerM: 2.50},
	"xai:grok-4.20-0309-reasoning":     {Provider: "xai", Model: "grok-4.20-0309-reasoning", InputPerM: 1.25, CacheReadPerM: 0.20, CacheWritePerM: 1.25, OutputPerM: 2.50},
	"xai:grok-4.20-0309-non-reasoning": {Provider: "xai", Model: "grok-4.20-0309-non-reasoning", InputPerM: 1.25, CacheReadPerM: 0.20, CacheWritePerM: 1.25, OutputPerM: 2.50},
	// Alibaba Qwen (models.dev providers/alibaba, accessed 2026-08-13;
	// sourced from alibabacloud.com model-pricing — International ≤256K
	// input tier — and the qwencloud.com model pages). qwen3.7-plus and
	// qwen3.6-flash carry the published International ≤256K rates;
	// CacheWritePerM is the Explicit Cache Creation rate (1.25x input) and
	// CacheReadPerM the Explicit Cache Read rate (0.1x input). qwen3.8-max
	// is priced at the published pay-as-you-go rate from its
	// qwencloud.com/models/qwen3.8-max page (the source models.dev cites);
	// qwen3.8-max-preview is served through the Alibaba Token Plan
	// subscription, which does not bill per token, so it stays at 0 (same
	// convention as the free GLM flash tiers). Mirror
	// packages/views/runtimes/utils.ts.
	"alibaba:qwen3.7-plus":        {Provider: "alibaba", Model: "qwen3.7-plus", InputPerM: 0.40, CacheReadPerM: 0.04, CacheWritePerM: 0.50, OutputPerM: 1.60},
	"alibaba:qwen3.6-flash":       {Provider: "alibaba", Model: "qwen3.6-flash", InputPerM: 0.25, CacheReadPerM: 0.025, CacheWritePerM: 0.3125, OutputPerM: 1.50},
	"alibaba:qwen3.8-max":         {Provider: "alibaba", Model: "qwen3.8-max", InputPerM: 2.00, CacheReadPerM: 0.17, CacheWritePerM: 2.50, OutputPerM: 6.00},
	"alibaba:qwen3.8-max-preview": {Provider: "alibaba", Model: "qwen3.8-max-preview", InputPerM: 0, CacheReadPerM: 0, CacheWritePerM: 0, OutputPerM: 0},
	// Moonshot Kimi K3 (platform.kimi.ai/docs/pricing/chat-k3 via models.dev
	// providers/moonshotai/models/kimi-k3.toml). Moonshot bills no separate
	// cache write, so CacheWritePerM mirrors Input.
	"moonshotai:kimi-k3": {Provider: "moonshotai", Model: "kimi-k3", InputPerM: 3.0, CacheReadPerM: 0.30, CacheWritePerM: 3.0, OutputPerM: 15.0},
	// Kimi K2.6 (kimi.com/resources/kimi-k2-6-pricing, the page the frontend
	// cites). CacheWrite mirrors input for the same reason K3's does:
	// Moonshot bills no separate cache-write rate.
	"moonshotai:kimi-k2.6": {Provider: "moonshotai", Model: "kimi-k2.6", InputPerM: 0.95, CacheReadPerM: 0.16, CacheWritePerM: 0.95, OutputPerM: 4.00},
	// Zhipu GLM-5 (docs.bigmodel.cn/cn/guide/start/pricing, accessed
	// 2026-10-07). Rates are the published INTERNATIONAL USD list: $1.00
	// input / $3.20 output / $0.20 cache hit per 1M tokens.
	//
	// Two caveats this row cannot express, both recorded rather than hidden:
	//   - The domestic CNY list is cheaper AND tiered by input length
	//     (4/18 CNY below 32K, 6/22 CNY at or above, cache hit 1 / 1.5), so
	//     one flat row over-reports domestic usage. That is tolerable only
	//     because the alerting rule is RELATIVE (3x the agent's own median),
	//     which cancels a constant factor; an ABSOLUTE budget cap would need
	//     the domestic rate instead.
	//   - Cache storage is currently free ("限时免费") with no published
	//     standard rate, and Zhipu publishes no separate cache-write rate, so
	//     CacheWritePerM mirrors Input — the convention the xAI and Moonshot
	//     rows use.
	//
	// This mirrors packages/views/runtimes/utils.ts, which has carried the
	// whole glm family all along; the Go table was simply missing it (see
	// TestZhipuGLM5Priced).
	"zhipu:glm-5": {Provider: "zhipu", Model: "glm-5", InputPerM: 1.00, CacheReadPerM: 0.20, CacheWritePerM: 1.00, OutputPerM: 3.20},
	// The rest of the glm family (ruel#44): same publisher, same page, same
	// two caveats as the glm-5 row above — the rates are the INTERNATIONAL
	// USD list, and Zhipu publishes no separate cache-write rate so
	// CacheWritePerM mirrors Input.
	//
	// The two flash rows are all-zero on purpose, not missing: Zhipu serves
	// them free, and a 0 here is the difference between "known-free" and
	// "unknown". Under the four-state cost model an all-zero row with real
	// tokens resolves to CostSourceZero, which is priced and belongs in a
	// median, whereas leaving the id out of the table would make it
	// CostSourceUnpriced — the state that silently disables every budget
	// gate it flows into. Same convention as alibaba:qwen3.8-max-preview.
	"zhipu:glm-5.1":        {Provider: "zhipu", Model: "glm-5.1", InputPerM: 1.40, CacheReadPerM: 0.26, CacheWritePerM: 1.40, OutputPerM: 4.40},
	"zhipu:glm-5-turbo":    {Provider: "zhipu", Model: "glm-5-turbo", InputPerM: 1.20, CacheReadPerM: 0.24, CacheWritePerM: 1.20, OutputPerM: 4.00},
	"zhipu:glm-4.7":        {Provider: "zhipu", Model: "glm-4.7", InputPerM: 0.60, CacheReadPerM: 0.11, CacheWritePerM: 0.60, OutputPerM: 2.20},
	"zhipu:glm-4.7-flashx": {Provider: "zhipu", Model: "glm-4.7-flashx", InputPerM: 0.07, CacheReadPerM: 0.01, CacheWritePerM: 0.07, OutputPerM: 0.40},
	"zhipu:glm-4.7-flash":  {Provider: "zhipu", Model: "glm-4.7-flash", InputPerM: 0, CacheReadPerM: 0, CacheWritePerM: 0, OutputPerM: 0},
	"zhipu:glm-4.6":        {Provider: "zhipu", Model: "glm-4.6", InputPerM: 0.60, CacheReadPerM: 0.11, CacheWritePerM: 0.60, OutputPerM: 2.20},
	"zhipu:glm-4.5":        {Provider: "zhipu", Model: "glm-4.5", InputPerM: 0.60, CacheReadPerM: 0.11, CacheWritePerM: 0.60, OutputPerM: 2.20},
	"zhipu:glm-4.5-x":      {Provider: "zhipu", Model: "glm-4.5-x", InputPerM: 2.20, CacheReadPerM: 0.45, CacheWritePerM: 2.20, OutputPerM: 8.90},
	"zhipu:glm-4.5-air":    {Provider: "zhipu", Model: "glm-4.5-air", InputPerM: 0.20, CacheReadPerM: 0.03, CacheWritePerM: 0.20, OutputPerM: 1.10},
	"zhipu:glm-4.5-airx":   {Provider: "zhipu", Model: "glm-4.5-airx", InputPerM: 1.10, CacheReadPerM: 0.22, CacheWritePerM: 1.10, OutputPerM: 4.50},
	"zhipu:glm-4.5-flash":  {Provider: "zhipu", Model: "glm-4.5-flash", InputPerM: 0, CacheReadPerM: 0, CacheWritePerM: 0, OutputPerM: 0},
	// Cursor Composer / Auto (cursor.com/docs/models-and-pricing,
	// cursor.com/docs/models/cursor-composer-2,
	// cursor.com/docs/models/cursor-composer-2-5). Cursor publishes no
	// cache-write rate for any of these, and cacheWrite stays 0 for a reason:
	// billing a reported cache_write_tokens off the input rate would invent
	// spend that the vendor never charged.
	//
	// Every id here is generic (`auto`, `composer-*`), and `auto` collides
	// head-on with codex — both providers report it (see
	// ModelPlaceholderValues in model_completeness.go). That is why the
	// frontend keys them `cursor/...` and why the rules below are pinned to
	// the `cursor/` prefix rather than to the bare id: a rule that resolved
	// bare `auto` would bill a codex run at Cursor's rate. Only the legacy
	// `cursor` key is unqualified, and only because it equals the provider
	// name itself and therefore cannot collide.
	"cursor:auto":              {Provider: "cursor", Model: "auto", InputPerM: 1.25, CacheReadPerM: 0.25, CacheWritePerM: 0, OutputPerM: 6.00},
	"cursor:composer-2.5-fast": {Provider: "cursor", Model: "composer-2.5-fast", InputPerM: 3.00, CacheReadPerM: 0.50, CacheWritePerM: 0, OutputPerM: 15.00},
	"cursor:composer-2.5":      {Provider: "cursor", Model: "composer-2.5", InputPerM: 0.50, CacheReadPerM: 0.20, CacheWritePerM: 0, OutputPerM: 2.50},
	"cursor:composer-2-fast":   {Provider: "cursor", Model: "composer-2-fast", InputPerM: 1.50, CacheReadPerM: 0.35, CacheWritePerM: 0, OutputPerM: 7.50},
	"cursor:composer-2":        {Provider: "cursor", Model: "composer-2", InputPerM: 0.50, CacheReadPerM: 0.20, CacheWritePerM: 0, OutputPerM: 2.50},
	"cursor:composer-1.5":      {Provider: "cursor", Model: "composer-1.5", InputPerM: 3.50, CacheReadPerM: 0.35, CacheWritePerM: 0, OutputPerM: 17.50},
	"cursor:composer-1":        {Provider: "cursor", Model: "composer-1", InputPerM: 1.25, CacheReadPerM: 0.125, CacheWritePerM: 0, OutputPerM: 10.00},
	// Legacy fallback: when neither the result event nor the configured
	// runtime model names a model, the daemon emits the literal `cursor`.
	// Priced at the current Composer 2.5 Fast default.
	"cursor:cursor": {Provider: "cursor", Model: "cursor", InputPerM: 3.00, CacheReadPerM: 0.50, CacheWritePerM: 0, OutputPerM: 15.00},
	// Volcengine Ark (ark.cn-beijing.volces.com). `ark-code-latest` is a
	// rolling alias whose target can be switched in the Volcengine console
	// (across model families), so it is not a stable model identity; the
	// daemon reports the alias itself, never the resolved model. No rate is
	// published for the alias, so it stays unmapped rather than inheriting
	// a guessed rate (same convention as xAI's `grok-composer-*`).
}

// versionEnd terminates a family rule: at most one suffix the frontend
// resolver normalizes away, and then the END of the id. Appending it keeps a
// rule from swallowing a later SKU in the same family — without it
// `claude-fable-5` also matches `claude-fable-5-1`, whose cache reads are a
// quarter of Fable 5's, so those reads bill at 4x. The same trap is why
// `gpt-5` must end here too: as a bare substring it would price `gpt-5-mini`
// at five times its real rate.
//
// It is NOT Anthropic-specific despite the Claude examples: the admitted
// suffixes are exactly what `stripContextTag` and `stripDate` remove in
// packages/views/runtimes/utils.ts before its exact-key lookup, and the
// frontend strips them for every provider, OpenAI included. A server rule
// anchored at a bare `$` therefore leaves a dated id (`gpt-5-2025-08-07`)
// unpriced on this side while the dashboard prices it — a gap in the opposite
// direction, and the harder one to notice because neither guard compares
// dated ids.
//
// (Only the suffixes: most rules here are substring matches, so a malformed
// PREFIX is out of scope.) The trailing `$` is what makes that true and is
// not optional: these rules are substring matches, so an alternative that
// merely starts a suffix still matches when arbitrary text follows it
// (`claude-fable-5-1-latest-preview`, `claude-fable-5-1[1m]junk`), which is
// the silent tier-borrowing this constant exists to prevent. The bracket form
// requires a complete tag for the same reason.
//
// A date snapshot carrying a context tag (`claude-fable-5-20260401[1m]`) is
// covered by the tag-stripping retry in PriceForModelAlias, so it does not
// need a combined alternative here.
//
// Anything else — another version digit, a `-preview`-style qualifier — is a
// distinct SKU at an unknown rate and stays unmapped until it gets a row of
// its own, the same "every catalog SKU needs its own row" rule the frontend
// table states.
const versionEnd = `(?:-20\d{6}|-20\d{2}-\d{2}-\d{2}|-latest|\[[^\]]+\])?$`

var modelAliasRules = []struct {
	re       *regexp.Regexp
	priceKey string
}{
	// Anchored exact-match: the effort is carried in a separate field, so the
	// model id is the bare slug. Anchoring to `$` keeps unknown variants
	// (`gpt-5.6-luna-pro`, `gpt-5.6-luna/x`) out of these rows. The `.` is a
	// LITERAL dot, not the `[.-]` class the older rows use — the real Codex
	// slug is always dotted (`gpt-5.6-luna`), and the frontend resolver in
	// utils.ts does NOT dash-normalize, so a dashed `gpt-5-6-luna` must surface
	// as unmapped on both sides rather than silently borrowing a tier here.
	{regexp.MustCompile(`(^|/|:)gpt-6-astra$`), "openai:gpt-6-astra"},
	{regexp.MustCompile(`(^|/|:)gpt-6\.1-sol$`), "openai:gpt-6.1-sol"},
	{regexp.MustCompile(`(^|/|:)gpt-6-sol$`), "openai:gpt-6-sol"},
	{regexp.MustCompile(`(^|/|:)gpt-6-luna$`), "openai:gpt-6-luna"},
	{regexp.MustCompile(`(^|/|:)gpt-5\.6-sol$`), "openai:gpt-5.6-sol"},
	{regexp.MustCompile(`(^|/|:)gpt-5\.6-terra$`), "openai:gpt-5.6-terra"},
	{regexp.MustCompile(`(^|/|:)gpt-5\.6-luna$`), "openai:gpt-5.6-luna"},
	{regexp.MustCompile(`(^|/|:)gpt-5[.-]5$|^gpt-5-5$`), "openai:gpt-5.5"},
	{regexp.MustCompile(`(^|/|:)gpt-5[.-]4($|-2026-03-05|-xhigh)`), "openai:gpt-5.4"},
	{regexp.MustCompile(`(^|/|:)gpt-5[.-]4-mini($|[^a-z0-9])`), "openai:gpt-5.4-mini"},
	{regexp.MustCompile(`(^|/|:)gpt-5[.-]3-codex$`), "openai:gpt-5.3-codex"},
	{regexp.MustCompile(`(^|/|:)gpt-5[.-]2-codex$`), "openai:gpt-5.2-codex"},
	// GPT-5 family, o-series and GPT-4o (ruel#44). All end at versionEnd, and
	// the `$` half is not decoration: `gpt-5` alone is a substring of
	// `gpt-5-mini` / `gpt-5-nano` / `gpt-5-codex`, which are 5x, 25x and 1x its
	// rate respectively. Dropping the anchor would silently price a nano run at
	// the flagship rate. `gpt-4o` / `gpt-4o-mini` (16x) and `o3` / `o3-mini`
	// (2x) are the same trap.
	//
	// The suffix half keeps parity with the dashboard, which strips a trailing
	// date or `latest` before looking the key up — so `gpt-5-2025-08-07` prices
	// on both sides instead of only in the UI.
	{regexp.MustCompile(`(^|/|:)gpt-5-mini` + versionEnd), "openai:gpt-5-mini"},
	{regexp.MustCompile(`(^|/|:)gpt-5-nano` + versionEnd), "openai:gpt-5-nano"},
	{regexp.MustCompile(`(^|/|:)gpt-5-codex` + versionEnd), "openai:gpt-5-codex"},
	{regexp.MustCompile(`(^|/|:)gpt-5` + versionEnd), "openai:gpt-5"},
	{regexp.MustCompile(`(^|/|:)o3-mini` + versionEnd), "openai:o3-mini"},
	{regexp.MustCompile(`(^|/|:)o3` + versionEnd), "openai:o3"},
	{regexp.MustCompile(`(^|/|:)o4-mini` + versionEnd), "openai:o4-mini"},
	{regexp.MustCompile(`(^|/|:)gpt-4o-mini` + versionEnd), "openai:gpt-4o-mini"},
	{regexp.MustCompile(`(^|/|:)gpt-4o` + versionEnd), "openai:gpt-4o"},
	{regexp.MustCompile(`claude-sonnet-5|claude-5-sonnet`), "anthropic:claude-sonnet-5"},
	// Fable 5.1 shares Fable 5's $10 / $50 and $12.50 cache write but prices
	// cache reads at 0.025x input ($0.25) instead of the standard 0.1x, so it
	// needs its own row, and both rules end at their own version
	// (versionEnd) so neither can swallow the other's ids.
	{regexp.MustCompile(`claude-fable-5[-.]1` + versionEnd), "anthropic:claude-fable-5-1"},
	{regexp.MustCompile(`claude-fable-5` + versionEnd), "anthropic:claude-fable-5"},
	// Opus 5.5 is cheaper than Opus 5 ($4 / $20) and prices cache reads at
	// 0.05x input, so the two need separate rows. Both rules end at their own
	// version (versionEnd), the same as the Fable pair above, so the
	// Opus 5 rule cannot swallow 5.5 ids and bill them at Opus 5 rates.
	{regexp.MustCompile(`claude-opus-5[-.]5` + versionEnd), "anthropic:claude-opus-5-5"},
	{regexp.MustCompile(`claude-opus-5` + versionEnd), "anthropic:claude-opus-5"},
	{regexp.MustCompile(`claude-opus-4[-.]8`), "anthropic:claude-opus-4.8"},
	{regexp.MustCompile(`claude-opus-4[-.]7`), "anthropic:claude-opus-4.7"},
	{regexp.MustCompile(`claude-opus-4[-.]6`), "anthropic:claude-opus-4.6"},
	{regexp.MustCompile(`claude-opus-4[-.]5`), "anthropic:claude-opus-4.5"},
	{regexp.MustCompile(`claude-sonnet-4[-.]6|claude-4[-.]6-sonnet`), "anthropic:claude-sonnet-4.6"},
	{regexp.MustCompile(`claude-sonnet-4[-.]5|claude-4[-.]5-sonnet`), "anthropic:claude-sonnet-4.5"},
	{regexp.MustCompile(`claude-haiku-4[-.]5`), "anthropic:claude-haiku-4.5"},
	// Pre-4.5 Anthropic (ruel#44). Every one of these ends at versionEnd, and
	// that is load-bearing rather than tidy: `claude-opus-4` unanchored would
	// match `claude-opus-4-1` through `claude-opus-4-8` and bill the whole 4.x
	// range at $15 / $75 instead of $5 / $25 — a 3x over-bill on every Opus 4.5+
	// run. `claude-sonnet-4` has the same trap against sonnet-4.5 / 4.6.
	{regexp.MustCompile(`claude-opus-4[-.]1` + versionEnd), "anthropic:claude-opus-4.1"},
	{regexp.MustCompile(`claude-opus-4` + versionEnd), "anthropic:claude-opus-4"},
	{regexp.MustCompile(`claude-sonnet-4` + versionEnd), "anthropic:claude-sonnet-4"},
	{regexp.MustCompile(`claude-haiku-3[-.]5` + versionEnd), "anthropic:claude-haiku-3.5"},
	{regexp.MustCompile(`deepseek-v4-pro`), "deepseek:v4-pro"},
	{regexp.MustCompile(`deepseek-v4-flash|^deepseek-chat$|^deepseek-reasoner$`), "deepseek:v4-flash"},
	{regexp.MustCompile(`minimax-m2[.]7.*highspeed|highspeed.*minimax-m2[.]7`), "minimax:m2.7-highspeed"},
	{regexp.MustCompile(`minimax-m2[.]7`), "minimax:m2.7"},
	{regexp.MustCompile(`gemini-3-flash`), "google:gemini-3-flash"},
	{regexp.MustCompile(`gemini-3[.]1-pro`), "google:gemini-3.1-pro"},
	{regexp.MustCompile(`gemini-2[.]5-pro`), "google:gemini-2.5-pro"},
	{regexp.MustCompile(`gemini-2[.]5-flash`), "google:gemini-2.5-flash"},
	// Anchored exact-match, dotted spelling only — same rule as the gpt-5.6
	// rows above. The frontend resolver does not dash-normalize non-Anthropic
	// ids, so a dashed `grok-4-5` must surface as unmapped on both sides
	// rather than silently borrowing a tier here.
	{regexp.MustCompile(`(^|/|:)grok-4\.6$`), "xai:grok-4.6"},
	{regexp.MustCompile(`(^|/|:)grok-4\.5$`), "xai:grok-4.5"},
	{regexp.MustCompile(`(^|/|:)grok-4\.3$`), "xai:grok-4.3"},
	{regexp.MustCompile(`(^|/|:)grok-build-0\.1$`), "xai:grok-build-0.1"},
	{regexp.MustCompile(`(^|/|:)grok-4\.20-multi-agent-0309$`), "xai:grok-4.20-multi-agent-0309"},
	{regexp.MustCompile(`(^|/|:)grok-4\.20-0309-reasoning$`), "xai:grok-4.20-0309-reasoning"},
	{regexp.MustCompile(`(^|/|:)grok-4\.20-0309-non-reasoning$`), "xai:grok-4.20-0309-non-reasoning"},
	// Alibaba Qwen. All rules are anchored so unknown suffixed variants
	// (`qwen3.7-plus-extra`, `qwen3.8-max-preview-extra`) stay unmapped;
	// an optional complete bracket tag `[…]` with at least one character
	// inside is admitted to match the frontend's behavior of stripping the
	// context tag (`\[[^\]]+\]$` in packages/views/runtimes/utils.ts), so
	// empty tags like `qwen3.7-plus[]` stay unmapped on both sides.
	// qwen3.8-max stays anchored so `qwen3.8-max-preview` (and its `[1m]`
	// variant) never borrows the GA tier.
	{regexp.MustCompile(`(^|/|:)qwen3[.-]7-plus(\[[^\]]+\])?$`), "alibaba:qwen3.7-plus"},
	{regexp.MustCompile(`(^|/|:)qwen3[.-]6-flash(\[[^\]]+\])?$`), "alibaba:qwen3.6-flash"},
	{regexp.MustCompile(`(^|/|:)qwen3[.-]8-max(\[[^\]]+\])?$`), "alibaba:qwen3.8-max"},
	{regexp.MustCompile(`(^|/|:)qwen3[.-]8-max-preview(\[[^\]]+\])?$`), "alibaba:qwen3.8-max-preview"},
	// Kimi K3. Anchored so the distinct CodeBuddy SKU `kimi-k3-1` stays
	// unmapped; `kimi-code/k3` (Kimi Code CLI) resolves via the `/k3$` form.
	{regexp.MustCompile(`(^|/|:)kimi-k3$`), "moonshotai:kimi-k3"},
	{regexp.MustCompile(`(^|/|:)k3$`), "moonshotai:kimi-k3"},
	// Kimi K2.6 (ruel#44). Placed after the K3 pair so `kimi-k3` keeps its own
	// row, and ending at versionEnd so `kimi-k2.6-preview` stays unmapped.
	{regexp.MustCompile(`(^|/|:)kimi-k2[-.]6` + versionEnd), "moonshotai:kimi-k2.6"},
	// Zhipu GLM (ruel#44). Every rule ends at versionEnd and every family is
	// ordered most-specific-first, for the same reason the OpenAI and Anthropic
	// families are: as bare substrings `glm-4.5` would swallow `glm-4.5-air`,
	// `-airx`, `-x` and `-flash`, whose rates differ by up to 4x
	// (`glm-4.5` $0.60 / $2.20 against `glm-4.5-x` $2.20 / $8.90).
	{regexp.MustCompile(`(^|/|:)glm-5[-.]1` + versionEnd), "zhipu:glm-5.1"},
	{regexp.MustCompile(`(^|/|:)glm-5-turbo` + versionEnd), "zhipu:glm-5-turbo"},
	{regexp.MustCompile(`(^|/|:)glm-5` + versionEnd), "zhipu:glm-5"},
	{regexp.MustCompile(`(^|/|:)glm-4[-.]7-flashx` + versionEnd), "zhipu:glm-4.7-flashx"},
	{regexp.MustCompile(`(^|/|:)glm-4[-.]7-flash` + versionEnd), "zhipu:glm-4.7-flash"},
	{regexp.MustCompile(`(^|/|:)glm-4[-.]7` + versionEnd), "zhipu:glm-4.7"},
	{regexp.MustCompile(`(^|/|:)glm-4[-.]6` + versionEnd), "zhipu:glm-4.6"},
	{regexp.MustCompile(`(^|/|:)glm-4[-.]5-flash` + versionEnd), "zhipu:glm-4.5-flash"},
	{regexp.MustCompile(`(^|/|:)glm-4[-.]5-x` + versionEnd), "zhipu:glm-4.5-x"},
	{regexp.MustCompile(`(^|/|:)glm-4[-.]5-airx` + versionEnd), "zhipu:glm-4.5-airx"},
	{regexp.MustCompile(`(^|/|:)glm-4[-.]5-air` + versionEnd), "zhipu:glm-4.5-air"},
	{regexp.MustCompile(`(^|/|:)glm-4[-.]5` + versionEnd), "zhipu:glm-4.5"},
	// Cursor (ruel#44). Every rule is pinned to the `cursor/` PREFIX rather
	// than anchored on the bare id, and that is the whole design: `auto` and
	// `composer-*` are generic across providers — codex reports `auto` too
	// (see ModelPlaceholderValues in model_completeness.go) — so a rule that
	// matched a bare `auto` would bill a codex run at Cursor's rate. Pinning
	// the prefix mirrors `pricingCandidates` in utils.ts, which tries the
	// provider-qualified key before the bare one, and it is what lets
	// PriceForModel("auto", "cursor") resolve while
	// PriceForModel("auto", "codex") stays unpriced.
	//
	// The composer rules are ordered most-specific-first for the usual reason:
	// `composer-2` as a prefix of `composer-2.5` (6x) and `composer-2-fast`
	// (3x) would otherwise bill both at $0.50 / $2.50.
	{regexp.MustCompile(`(^|/)cursor/auto` + versionEnd), "cursor:auto"},
	{regexp.MustCompile(`(^|/)cursor/composer-2[-.]5-fast` + versionEnd), "cursor:composer-2.5-fast"},
	{regexp.MustCompile(`(^|/)cursor/composer-2[-.]5` + versionEnd), "cursor:composer-2.5"},
	{regexp.MustCompile(`(^|/)cursor/composer-2-fast` + versionEnd), "cursor:composer-2-fast"},
	{regexp.MustCompile(`(^|/)cursor/composer-2` + versionEnd), "cursor:composer-2"},
	{regexp.MustCompile(`(^|/)cursor/composer-1[-.]5` + versionEnd), "cursor:composer-1.5"},
	{regexp.MustCompile(`(^|/)cursor/composer-1` + versionEnd), "cursor:composer-1"},
	{regexp.MustCompile(`(^|/|:)cursor` + versionEnd), "cursor:cursor"},
	// Volcengine Ark `ark-code-latest` is deliberately absent: it is a
	// console-switchable rolling alias across model families, not a stable
	// model identity, so it stays unmapped.
}

// contextTagRe matches a trailing context-window variant tag such as the
// `[1m]` Claude Code appends to the model id. A complete bracket tag with at
// least one character inside, anchored at the end — the same shape the
// frontend's `stripContextTag` strips (`\[[^\]]+\]$` in
// packages/views/runtimes/utils.ts), so empty tags (`model[]`) and non-tag
// trailing brackets (`model[`) stay unmapped on both sides.
var contextTagRe = regexp.MustCompile(`\[[^\]]+\]$`)

func matchModelAlias(model string) (ModelPrice, bool) {
	for _, rule := range modelAliasRules {
		if rule.re.MatchString(model) {
			price, ok := modelPrices[rule.priceKey]
			return price, ok
		}
	}
	return ModelPrice{}, false
}

// PriceForModel resolves a usage row's (model, provider) pair, and MUST be
// preferred over PriceForModelAlias wherever a provider is known.
//
// Why the provider is not optional: some ids are generic across providers.
// Cursor reports `auto` and `composer-*`; codex also reports `auto` (see
// ModelPlaceholderValues in model_completeness.go, which calls out exactly this
// collision). The frontend therefore keys such ids as `cursor/auto` and tries
// the provider-qualified form BEFORE the bare one. Pricing a bare `auto` here
// would bill a codex run at Cursor's rate — a cross-provider misprice, which is
// the same class of bug this whole table exists to prevent, just in a new
// direction.
//
// The qualified-first order mirrors `pricingCandidates` in
// packages/views/runtimes/utils.ts so both sides pick the same row.
func PriceForModel(model, provider string) (ModelPrice, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	p := strings.ToLower(strings.TrimSpace(provider))
	if p != "" && m != "" {
		// qualify() in utils.ts leaves an already-qualified key alone, so a
		// model that already carries its own provider does not get doubled.
		qualified := m
		if !strings.HasPrefix(m, p+"/") {
			qualified = p + "/" + m
		}
		if price, ok := PriceForModelAlias(qualified); ok {
			return price, true
		}
	}
	return PriceForModelAlias(m)
}

func PriceForModelAlias(model string) (ModelPrice, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if price, ok := matchModelAlias(model); ok {
		return price, true
	}
	// The raw id did not resolve: a harness-appended context-window tag
	// (`kimi-k3[1m]`, `grok-4.5[1m]`) is the same SKU at the same tier, so
	// retry against the bare id. The anchored Codex / Grok / Kimi rules end at
	// `$`, so without this a bracketed variant would take the unpriced branch
	// in RecordLLMUsage. Only ever turns a miss into a hit — the raw form is
	// tried first, so an explicit bracketed rule still wins.
	//
	// Exactly ONE tag, matching the frontend: `canonicalCandidates` in
	// packages/views/runtimes/utils.ts strips a single trailing tag and does
	// not re-strip the result. Retrying a doubly-tagged id would peel `[2m]`
	// off `claude-fable-5[1m][2m]` and let the leftover `[1m]` satisfy a rule
	// that already, correctly, rejected the raw form — the dashboard leaves
	// that id unmapped, so pricing it here would put two different costs on
	// one usage row. A second tag means the id is not a shape we recognise.
	if stripped := contextTagRe.ReplaceAllString(model, ""); stripped != model {
		if contextTagRe.MatchString(stripped) {
			return ModelPrice{}, false
		}
		return matchModelAlias(stripped)
	}
	return ModelPrice{}, false
}

func tokenCostUSD(tokens int64, pricePerM float64) float64 {
	if tokens <= 0 || pricePerM <= 0 {
		return 0
	}
	return float64(tokens) * pricePerM / 1_000_000
}

// CostSource says where a usage row's converted cost came from.
//
// The whole point of this enum is one distinction: `unpriced` means "we do
// not know what this cost", which is NOT "this cost nothing". A usage row
// whose model has no rate and whose provider reported no price has an unknown
// cost — rendering it as $0.00 tells the user the run was free, and worse,
// feeds a 0 into any median or budget computed over it. Those two outcomes
// are the reason the states are separate rather than two spellings of 0.
type CostSource string

const (
	// CostSourceProvider: the runtime charged the turn itself
	// (`providerTicks > 0`). Authoritative — no estimate can improve on it.
	CostSourceProvider CostSource = "provider"
	// CostSourceTable: no provider price, but the rate table knows the model.
	// An estimate, and the only kind of cost most runtimes will ever have.
	CostSourceTable CostSource = "table"
	// CostSourceZero: priced, and genuinely free. Either nothing was consumed
	// (no tokens at all) or the model's rates are all 0 (a free tier). Safe
	// to show as $0.00 and safe to feed into a median.
	CostSourceZero CostSource = "zero"
	// CostSourceUnpriced: tokens were consumed, nobody reported a price, and
	// the rate table does not know the model. MUST NOT be shown as a number
	// and MUST NOT enter an aggregate — surface it as "cannot be priced"
	// alongside the token count it applies to.
	CostSourceUnpriced CostSource = "unpriced"
)

// UsageCost is one usage row converted to USD, with the state that says
// whether the number means anything.
type UsageCost struct {
	USD    float64
	Source CostSource
}

// Priceable reports whether `c` carries a real number a caller may display
// and aggregate. `unpriced` is the one state that does not.
func (c UsageCost) Priceable() bool {
	return c.Source != CostSourceUnpriced
}

// EstimateUsageCost converts a usage row to USD, preferring the provider's own
// price and falling back to the rate table.
//
// Precedence mirrors estimateCost in packages/views/runtimes/utils.ts so the
// server-side derivation and the dashboard agree on one row. The two differ in
// one place on purpose: the client has no "unpriced" state and returns a plain
// number, leaving the caller to notice that a row with tokens priced to 0. The
// enum here exists so that noticing is not optional.
func EstimateUsageCost(model, provider string, providerTicks, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int64) UsageCost {
	if providerTicks > 0 {
		return UsageCost{USD: float64(providerTicks) / CostUSDTicksPerUSD, Source: CostSourceProvider}
	}
	total := inputTokens + outputTokens + cacheReadTokens + cacheWriteTokens
	if total <= 0 {
		// Nothing was consumed. This really is free, not unknown.
		return UsageCost{USD: 0, Source: CostSourceZero}
	}
	price, ok := PriceForModel(model, provider)
	if !ok {
		return UsageCost{USD: 0, Source: CostSourceUnpriced}
	}
	usd := tokenCostUSD(inputTokens, price.InputPerM) +
		tokenCostUSD(outputTokens, price.OutputPerM) +
		tokenCostUSD(cacheReadTokens, price.CacheReadPerM) +
		tokenCostUSD(cacheWriteTokens, price.CacheWritePerM)
	if usd <= 0 {
		// Rates are known and they are all 0 (a free tier). Known-free, not
		// unknown — it belongs in a median as a real 0.
		return UsageCost{USD: 0, Source: CostSourceZero}
	}
	return UsageCost{USD: usd, Source: CostSourceTable}
}
