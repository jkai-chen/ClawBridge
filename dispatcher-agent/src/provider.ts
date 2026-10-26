// The model provider, built from the server's config only: provider, API, base URL, model and key all come from
// agent/config, and the key reaches pi through createProvider's auth resolver, never through an env variable.
import type { Model, Provider, ProviderStreams } from "@earendil-works/pi-ai";
import { createProvider } from "@earendil-works/pi-ai";

// Key-authenticated APIs only. Bedrock, Vertex and Azure read ambient cloud credentials and settings, so they are not
// offered until they have an explicit-credential path.
const APIS: Record<string, () => Promise<ProviderStreams>> = {
	"anthropic-messages": async () => (await import("@earendil-works/pi-ai/api/anthropic-messages.lazy")).anthropicMessagesApi(),
	"openai-completions": async () => (await import("@earendil-works/pi-ai/api/openai-completions.lazy")).openAICompletionsApi(),
	"openai-responses": async () => (await import("@earendil-works/pi-ai/api/openai-responses.lazy")).openAIResponsesApi(),
	"google-generative-ai": async () =>
		(await import("@earendil-works/pi-ai/api/google-generative-ai.lazy")).googleGenerativeAIApi(),
	"mistral-conversations": async () =>
		(await import("@earendil-works/pi-ai/api/mistral-conversations.lazy")).mistralConversationsApi(),
};
export const SUPPORTED_APIS: readonly string[] = Object.keys(APIS);

export interface ProviderConfig {
	id: string;
	api: string;
	base_url: string;
	model: string;
	key: string;
}

export interface BuiltProvider {
	provider: Provider;
	model: Model<string>;
}

/** Builds the provider and its one model. Throws a fixed message (never the key) on a config it can't use. */
export async function buildProvider(
	config: ProviderConfig,
	limits: { max_input_tokens_per_run: number; max_output_tokens_per_run: number },
): Promise<BuiltProvider> {
	const loadApi = APIS[config.api];
	if (!loadApi) throw new Error("provider api is not supported by this agent build");
	const url = new URL(config.base_url);
	const loopback = url.hostname === "localhost" || url.hostname === "127.0.0.1" || url.hostname === "[::1]";
	if (url.protocol !== "https:" && !(url.protocol === "http:" && loopback)) {
		throw new Error("provider base_url must use https");
	}
	const key = config.key;
	const model: Model<string> = {
		id: config.model,
		name: config.model,
		api: config.api,
		provider: config.id,
		baseUrl: config.base_url,
		reasoning: false,
		input: ["text"],
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: limits.max_input_tokens_per_run + limits.max_output_tokens_per_run,
		maxTokens: limits.max_output_tokens_per_run,
	};
	const provider = createProvider({
		id: config.id,
		baseUrl: config.base_url,
		auth: { apiKey: { name: config.id, resolve: async () => ({ auth: { apiKey: key } }) } },
		models: [model],
		api: await loadApi(),
	});
	return { provider, model };
}
