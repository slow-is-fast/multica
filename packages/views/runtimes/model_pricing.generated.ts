// GENERATED FILE — DO NOT EDIT.
//
// Source of truth: server/internal/metrics/pricing.go (the rate table
// modelPrices) and pricing_frontend_keys.go (the key spellings). Regenerate
// with:
//
//     go run ./cmd/gen_model_prices
//
// from the repository root, or `make gen-prices` if that target exists.
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
  "claude-fable-5": { input: 10, output: 50, cacheRead: 1, cacheWrite: 12.5 },
  "claude-fable-5-1": { input: 10, output: 50, cacheRead: 0.25, cacheWrite: 12.5 },
  "claude-haiku-3-5": { input: 0.8, output: 4, cacheRead: 0.08, cacheWrite: 1 },
  "claude-haiku-4-5": { input: 1, output: 5, cacheRead: 0.1, cacheWrite: 1.25 },
  "claude-opus-4": { input: 15, output: 75, cacheRead: 1.5, cacheWrite: 18.75 },
  "claude-opus-4-1": { input: 15, output: 75, cacheRead: 1.5, cacheWrite: 18.75 },
  "claude-opus-4-5": { input: 5, output: 25, cacheRead: 0.5, cacheWrite: 6.25 },
  "claude-opus-4-6": { input: 5, output: 25, cacheRead: 0.5, cacheWrite: 6.25 },
  "claude-opus-4-7": { input: 5, output: 25, cacheRead: 0.5, cacheWrite: 6.25 },
  "claude-opus-4-8": { input: 5, output: 25, cacheRead: 0.5, cacheWrite: 6.25 },
  "claude-opus-5": { input: 5, output: 25, cacheRead: 0.5, cacheWrite: 6.25 },
  "claude-opus-5-5": { input: 4, output: 20, cacheRead: 0.2, cacheWrite: 5 },
  "claude-sonnet-4": { input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75 },
  "claude-sonnet-4-5": { input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75 },
  "claude-sonnet-4-6": { input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75 },
  "claude-sonnet-5": { input: 2, output: 10, cacheRead: 0.2, cacheWrite: 2.5 },
  "cursor": { input: 3, output: 15, cacheRead: 0.5, cacheWrite: 0 },
  "cursor/auto": { input: 1.25, output: 6, cacheRead: 0.25, cacheWrite: 0 },
  "cursor/composer-1": { input: 1.25, output: 10, cacheRead: 0.125, cacheWrite: 0 },
  "cursor/composer-1.5": { input: 3.5, output: 17.5, cacheRead: 0.35, cacheWrite: 0 },
  "cursor/composer-2": { input: 0.5, output: 2.5, cacheRead: 0.2, cacheWrite: 0 },
  "cursor/composer-2-fast": { input: 1.5, output: 7.5, cacheRead: 0.35, cacheWrite: 0 },
  "cursor/composer-2.5": { input: 0.5, output: 2.5, cacheRead: 0.2, cacheWrite: 0 },
  "cursor/composer-2.5-fast": { input: 3, output: 15, cacheRead: 0.5, cacheWrite: 0 },
  "deepseek-chat": { input: 0.56, output: 1.12, cacheRead: 0.0112, cacheWrite: 0.56 },
  "deepseek-reasoner": { input: 0.56, output: 1.12, cacheRead: 0.0112, cacheWrite: 0.56 },
  "deepseek-v4-flash": { input: 0.56, output: 1.12, cacheRead: 0.0112, cacheWrite: 0.56 },
  "deepseek-v4-pro": { input: 1.74, output: 3.48, cacheRead: 0.0145, cacheWrite: 1.74 },
  "gemini-2.5-flash": { input: 0.3, output: 2.5, cacheRead: 0.03, cacheWrite: 0.3 },
  "gemini-2.5-pro": { input: 1.25, output: 10, cacheRead: 0.31, cacheWrite: 1.25 },
  "gemini-3-flash": { input: 0.5, output: 3, cacheRead: 0.05, cacheWrite: 0.5 },
  "gemini-3.1-pro": { input: 2, output: 12, cacheRead: 0.2, cacheWrite: 2 },
  "glm-4.5": { input: 0.6, output: 2.2, cacheRead: 0.11, cacheWrite: 0.6 },
  "glm-4.5-air": { input: 0.2, output: 1.1, cacheRead: 0.03, cacheWrite: 0.2 },
  "glm-4.5-airx": { input: 1.1, output: 4.5, cacheRead: 0.22, cacheWrite: 1.1 },
  "glm-4.5-flash": { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
  "glm-4.5-x": { input: 2.2, output: 8.9, cacheRead: 0.45, cacheWrite: 2.2 },
  "glm-4.6": { input: 0.6, output: 2.2, cacheRead: 0.11, cacheWrite: 0.6 },
  "glm-4.7": { input: 0.6, output: 2.2, cacheRead: 0.11, cacheWrite: 0.6 },
  "glm-4.7-flash": { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
  "glm-4.7-flashx": { input: 0.07, output: 0.4, cacheRead: 0.01, cacheWrite: 0.07 },
  "glm-5": { input: 1, output: 3.2, cacheRead: 0.2, cacheWrite: 1 },
  "glm-5-turbo": { input: 1.2, output: 4, cacheRead: 0.24, cacheWrite: 1.2 },
  "glm-5.1": { input: 1.4, output: 4.4, cacheRead: 0.26, cacheWrite: 1.4 },
  "gpt-4o": { input: 2.5, output: 10, cacheRead: 1.25, cacheWrite: 2.5 },
  "gpt-4o-mini": { input: 0.15, output: 0.6, cacheRead: 0.075, cacheWrite: 0.15 },
  "gpt-5": { input: 1.25, output: 10, cacheRead: 0.125, cacheWrite: 1.25 },
  "gpt-5-codex": { input: 1.25, output: 10, cacheRead: 0.125, cacheWrite: 1.25 },
  "gpt-5-mini": { input: 0.25, output: 2, cacheRead: 0.025, cacheWrite: 0.25 },
  "gpt-5-nano": { input: 0.05, output: 0.4, cacheRead: 0.005, cacheWrite: 0.05 },
  "gpt-5.2-codex": { input: 1.75, output: 14, cacheRead: 0.175, cacheWrite: 1.75 },
  "gpt-5.3-codex": { input: 1.75, output: 14, cacheRead: 0.175, cacheWrite: 1.75 },
  "gpt-5.4": { input: 2.5, output: 15, cacheRead: 0.25, cacheWrite: 2.5 },
  "gpt-5.4-mini": { input: 0.75, output: 4.5, cacheRead: 0.075, cacheWrite: 0.75 },
  "gpt-5.5": { input: 5, output: 30, cacheRead: 0.5, cacheWrite: 5 },
  "gpt-5.6-luna": { input: 1, output: 6, cacheRead: 0.1, cacheWrite: 1.25 },
  "gpt-5.6-sol": { input: 5, output: 30, cacheRead: 0.5, cacheWrite: 6.25 },
  "gpt-5.6-terra": { input: 2.5, output: 15, cacheRead: 0.25, cacheWrite: 3.125 },
  "gpt-6-astra": { input: 10, output: 50, cacheRead: 1, cacheWrite: 12.5 },
  "gpt-6-luna": { input: 0.1, output: 0.5, cacheRead: 0.01, cacheWrite: 0.125 },
  "gpt-6-sol": { input: 2, output: 10, cacheRead: 0.2, cacheWrite: 2.5 },
  "gpt-6.1-sol": { input: 2, output: 10, cacheRead: 0.1, cacheWrite: 2.5 },
  "grok-4.20-0309-non-reasoning": { input: 1.25, output: 2.5, cacheRead: 0.2, cacheWrite: 1.25 },
  "grok-4.20-0309-reasoning": { input: 1.25, output: 2.5, cacheRead: 0.2, cacheWrite: 1.25 },
  "grok-4.20-multi-agent-0309": { input: 1.25, output: 2.5, cacheRead: 0.2, cacheWrite: 1.25 },
  "grok-4.3": { input: 1.25, output: 2.5, cacheRead: 0.2, cacheWrite: 1.25 },
  "grok-4.5": { input: 2, output: 6, cacheRead: 0.3, cacheWrite: 2 },
  "grok-4.6": { input: 2, output: 6, cacheRead: 0.5, cacheWrite: 2 },
  "grok-build-0.1": { input: 1, output: 2, cacheRead: 0.2, cacheWrite: 1 },
  "kimi-k2.6": { input: 0.95, output: 4, cacheRead: 0.16, cacheWrite: 0.95 },
  "kimi-k3": { input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3 },
  "kimi/k3": { input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3 },
  "minimax-m2.7": { input: 0.3, output: 1.2, cacheRead: 0.06, cacheWrite: 0.375 },
  "minimax-m2.7-highspeed": { input: 0.6, output: 2.4, cacheRead: 0.06, cacheWrite: 0.375 },
  "o3": { input: 2, output: 8, cacheRead: 0.5, cacheWrite: 2 },
  "o3-mini": { input: 1.1, output: 4.4, cacheRead: 0.55, cacheWrite: 1.1 },
  "o4-mini": { input: 1.1, output: 4.4, cacheRead: 0.275, cacheWrite: 1.1 },
  "qwen3.6-flash": { input: 0.25, output: 1.5, cacheRead: 0.025, cacheWrite: 0.3125 },
  "qwen3.7-plus": { input: 0.4, output: 1.6, cacheRead: 0.04, cacheWrite: 0.5 },
  "qwen3.8-max": { input: 2, output: 6, cacheRead: 0.17, cacheWrite: 2.5 },
  "qwen3.8-max-preview": { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
};
