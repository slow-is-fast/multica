package metrics

// This file carries the one thing that cannot be derived from the rate table:
// the exact KEY the web UI looks a model up under.
//
// modelPrices is the single source for the NUMBERS — the generated frontend
// table is produced from it, so a rate can no longer be edited on one side
// only. The keys are a different matter: the two sides never agreed on a
// spelling, and the difference is not cosmetic.
//
//   - Anthropic ships dotted versions (claude-opus-4.5); the dashboard
//     normalizes them to dashes (claude-opus-4-5), because Claude Code reports
//     one and GitHub Copilot reports the other and they are the same SKU.
//   - OpenAI keeps the dot (gpt-5.6-sol) — there the separator IS semantic,
//     `gpt-5.4` and `gpt-5-4` are different ids.
//   - DeepSeek carries a family prefix the Go Model field drops (v4-pro ->
//     deepseek-v4-pro), plus two legacy aliases (deepseek-chat,
//     deepseek-reasoner) that are the same SKU under older names.
//   - A generic id belongs to whatever provider reported it, so it is keyed
//     `<provider>/<model>` (cursor/auto, kimi/k3) — see PriceForModel.
//
// A row may therefore own several keys, and a key must not be guessed: a
// generated key the alias rules cannot reach is a dead key, which is exactly
// the "looks complete, prices nothing" failure this table is prone to.
// TestGeneratedFrontendPricingIsCommitted checks both directions, and
// TestFrontendPricingKeysResolveBackToTheirRow checks each key individually.
var frontendPricingKeys = map[string][]string{
	"alibaba:qwen3.6-flash": {"qwen3.6-flash"},
	"alibaba:qwen3.7-plus": {"qwen3.7-plus"},
	"alibaba:qwen3.8-max": {"qwen3.8-max"},
	"alibaba:qwen3.8-max-preview": {"qwen3.8-max-preview"},
	"anthropic:claude-fable-5": {"claude-fable-5"},
	"anthropic:claude-fable-5-1": {"claude-fable-5-1"},
	"anthropic:claude-haiku-3.5": {"claude-haiku-3-5"},
	"anthropic:claude-haiku-4.5": {"claude-haiku-4-5"},
	"anthropic:claude-opus-4": {"claude-opus-4"},
	"anthropic:claude-opus-4.1": {"claude-opus-4-1"},
	"anthropic:claude-opus-4.5": {"claude-opus-4-5"},
	"anthropic:claude-opus-4.6": {"claude-opus-4-6"},
	"anthropic:claude-opus-4.7": {"claude-opus-4-7"},
	"anthropic:claude-opus-4.8": {"claude-opus-4-8"},
	"anthropic:claude-opus-5": {"claude-opus-5"},
	"anthropic:claude-opus-5-5": {"claude-opus-5-5"},
	"anthropic:claude-sonnet-4": {"claude-sonnet-4"},
	"anthropic:claude-sonnet-4.5": {"claude-sonnet-4-5"},
	"anthropic:claude-sonnet-4.6": {"claude-sonnet-4-6"},
	"anthropic:claude-sonnet-5": {"claude-sonnet-5"},
	"cursor:auto": {"cursor/auto"},
	"cursor:composer-1": {"cursor/composer-1"},
	"cursor:composer-1.5": {"cursor/composer-1.5"},
	"cursor:composer-2": {"cursor/composer-2"},
	"cursor:composer-2-fast": {"cursor/composer-2-fast"},
	"cursor:composer-2.5": {"cursor/composer-2.5"},
	"cursor:composer-2.5-fast": {"cursor/composer-2.5-fast"},
	"cursor:cursor": {"cursor"},
	"deepseek:v4-flash": {"deepseek-chat", "deepseek-reasoner", "deepseek-v4-flash"},
	"deepseek:v4-pro": {"deepseek-v4-pro"},
	"google:gemini-2.5-flash": {"gemini-2.5-flash"},
	"google:gemini-2.5-pro": {"gemini-2.5-pro"},
	"google:gemini-3-flash": {"gemini-3-flash"},
	"google:gemini-3.1-pro": {"gemini-3.1-pro"},
	"minimax:m2.7": {"minimax-m2.7"},
	"minimax:m2.7-highspeed": {"minimax-m2.7-highspeed"},
	"moonshotai:kimi-k2.6": {"kimi-k2.6"},
	"moonshotai:kimi-k3": {"kimi-k3", "kimi/k3"},
	"openai:gpt-4o": {"gpt-4o"},
	"openai:gpt-4o-mini": {"gpt-4o-mini"},
	"openai:gpt-5": {"gpt-5"},
	"openai:gpt-5-codex": {"gpt-5-codex"},
	"openai:gpt-5-mini": {"gpt-5-mini"},
	"openai:gpt-5-nano": {"gpt-5-nano"},
	"openai:gpt-5.2-codex": {"gpt-5.2-codex"},
	"openai:gpt-5.3-codex": {"gpt-5.3-codex"},
	"openai:gpt-5.4": {"gpt-5.4"},
	"openai:gpt-5.4-mini": {"gpt-5.4-mini"},
	"openai:gpt-5.5": {"gpt-5.5"},
	"openai:gpt-5.6-luna": {"gpt-5.6-luna"},
	"openai:gpt-5.6-sol": {"gpt-5.6-sol"},
	"openai:gpt-5.6-terra": {"gpt-5.6-terra"},
	"openai:gpt-6-astra": {"gpt-6-astra"},
	"openai:gpt-6-luna": {"gpt-6-luna"},
	"openai:gpt-6-sol": {"gpt-6-sol"},
	"openai:gpt-6.1-sol": {"gpt-6.1-sol"},
	"openai:o3": {"o3"},
	"openai:o3-mini": {"o3-mini"},
	"openai:o4-mini": {"o4-mini"},
	"xai:grok-4.20-0309-non-reasoning": {"grok-4.20-0309-non-reasoning"},
	"xai:grok-4.20-0309-reasoning": {"grok-4.20-0309-reasoning"},
	"xai:grok-4.20-multi-agent-0309": {"grok-4.20-multi-agent-0309"},
	"xai:grok-4.3": {"grok-4.3"},
	"xai:grok-4.5": {"grok-4.5"},
	"xai:grok-4.6": {"grok-4.6"},
	"xai:grok-build-0.1": {"grok-build-0.1"},
	"zhipu:glm-4.5": {"glm-4.5"},
	"zhipu:glm-4.5-air": {"glm-4.5-air"},
	"zhipu:glm-4.5-airx": {"glm-4.5-airx"},
	"zhipu:glm-4.5-flash": {"glm-4.5-flash"},
	"zhipu:glm-4.5-x": {"glm-4.5-x"},
	"zhipu:glm-4.6": {"glm-4.6"},
	"zhipu:glm-4.7": {"glm-4.7"},
	"zhipu:glm-4.7-flash": {"glm-4.7-flash"},
	"zhipu:glm-4.7-flashx": {"glm-4.7-flashx"},
	"zhipu:glm-5": {"glm-5"},
	"zhipu:glm-5-turbo": {"glm-5-turbo"},
	"zhipu:glm-5.1": {"glm-5.1"},
}
