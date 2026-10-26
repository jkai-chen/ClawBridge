// pi-ai falls back to environment variables for provider keys, ambient cloud credentials, endpoints and proxies.
// The agent's key, endpoint and egress come only from the server's config (agent/config), so it refuses to start
// while any variable pi-ai would read is set: a stray OPENAI_API_KEY must never be what a request sends. It also
// refuses Node's own TLS, CA and preload variables, which would weaken every connection it makes.
// test/env-guard.test.ts fails when the pinned pi-ai reads a variable missing from this list.
export const PI_ENV_VARS: readonly string[] = [
	// env-api-keys.ts: provider keys
	"AI_GATEWAY_API_KEY", "ANT_LING_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN",
	"AZURE_OPENAI_API_KEY", "BASETEN_API_KEY", "CEREBRAS_API_KEY", "CLOUDFLARE_API_KEY", "DEEPSEEK_API_KEY",
	"FIREWORKS_API_KEY", "GEMINI_API_KEY", "GOOGLE_CLOUD_API_KEY", "GROQ_API_KEY", "HF_TOKEN", "KIMI_API_KEY",
	"META_API_KEY", "MINIMAX_API_KEY", "MINIMAX_CN_API_KEY", "MISTRAL_API_KEY", "MOONSHOT_API_KEY", "NVIDIA_API_KEY",
	"OPENAI_API_KEY", "OPENCODE_API_KEY", "OPENROUTER_API_KEY", "QWEN_TOKEN_PLAN_API_KEY", "QWEN_TOKEN_PLAN_CN_API_KEY",
	"TOGETHER_API_KEY", "XAI_API_KEY", "XIAOMI_API_KEY", "XIAOMI_TOKEN_PLAN_AMS_API_KEY", "XIAOMI_TOKEN_PLAN_CN_API_KEY",
	"XIAOMI_TOKEN_PLAN_SGP_API_KEY", "ZAI_API_KEY", "ZAI_CODING_CN_API_KEY",
	// env-api-keys.ts: Anthropic federation identity
	"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE", "ANTHROPIC_ORGANIZATION_ID",
	"ANTHROPIC_SERVICE_ACCOUNT_ID", "ANTHROPIC_WORKSPACE_ID",
	// ambient cloud credentials and their settings (Bedrock, Vertex, Azure)
	"AWS_ACCESS_KEY_ID", "AWS_BEARER_TOKEN_BEDROCK", "AWS_BEDROCK_FORCE_CACHE", "AWS_BEDROCK_FORCE_HTTP1",
	"AWS_BEDROCK_SKIP_AUTH", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	"AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_REGION", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
	"AWS_WEB_IDENTITY_TOKEN_FILE", "AZURE_OPENAI_API_VERSION", "AZURE_OPENAI_BASE_URL", "AZURE_OPENAI_DEPLOYMENT_NAME_MAP",
	"AZURE_OPENAI_RESOURCE_NAME", "GCLOUD_PROJECT", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_LOCATION",
	"GOOGLE_CLOUD_PROJECT",
	// request behaviour
	"PI_CACHE_RETENTION",
	// proxies: egress goes through the proxy the launcher configures, never an ambient one (either case)
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy",
	// Node itself: these would turn off TLS checks, trust another CA, or load code before the agent starts
	"NODE_TLS_REJECT_UNAUTHORIZED", "NODE_EXTRA_CA_CERTS", "NODE_OPTIONS",
];

/** The names of the forbidden variables that are set (non-empty), sorted. Values are never read out. */
export function forbiddenEnv(env: NodeJS.ProcessEnv): string[] {
	return PI_ENV_VARS.filter((name) => !!env[name]).sort();
}
