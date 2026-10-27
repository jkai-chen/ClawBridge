// Writes internal/dispatchermodel/models.json, the model hints `dispatcher model setup` shows, from the pinned
// pi-ai's own catalogue. `--check` fails if the committed file differs.
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { getModels } from "@earendil-works/pi-ai/compat";

export const MODELS_PATH = fileURLToPath(new URL("../../internal/dispatchermodel/models.json", import.meta.url));
const PROVIDERS_PATH = fileURLToPath(new URL("../../internal/dispatchermodel/providers.json", import.meta.url));
const PI_AI_PACKAGE = fileURLToPath(new URL("../node_modules/@earendil-works/pi-ai/package.json", import.meta.url));

interface Provider {
	id: string;
	api: string;
	base_url?: string;
	custom?: boolean;
}

interface Hint {
	id: string;
	name: string;
	context_window: number;
	// List prices in US dollars per million tokens, as the catalogue has them.
	input_per_mtok: number;
	output_per_mtok: number;
}

const trim = (url: string | undefined) => (url ?? "").replace(/\/+$/, "");

// The model IDs setup accepts (internal/dispatchermodel's modelPattern; a Go test fails on any listed ID it refuses).
// OpenRouter's floating "~…-latest" aliases are left out: a dispatcher runs one pinned model, the one its setup test
// checked for tool use. So is a router with no price of its own (OpenRouter's "auto", priced -1,000,000).
const MODEL_ID = /^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$/;

/** The hints document, as the Go package embeds it. */
export function modelsDocument(): string {
	const providers: Provider[] = JSON.parse(readFileSync(PROVIDERS_PATH, "utf8")).providers;
	const piAI = JSON.parse(readFileSync(PI_AI_PACKAGE, "utf8"));
	const models: Record<string, Hint[]> = {};
	for (const p of providers) {
		if (p.custom) {
			continue;
		}
		// Only what this provider entry can call: pi sets the API and base URL per model, and some providers
		// (OpenRouter) serve some models on another API.
		const callable = (getModels(p.id as never) ?? []).filter(
			(m) => m.api === p.api && trim(m.baseUrl) === trim(p.base_url) && MODEL_ID.test(m.id) && m.cost.input >= 0 && m.cost.output >= 0,
		);
		models[p.id] = callable
			.map((m) => ({
				id: m.id,
				name: m.name,
				context_window: m.contextWindow,
				input_per_mtok: m.cost.input,
				output_per_mtok: m.cost.output,
			}))
			.sort((a, b) => a.id.localeCompare(b.id));
	}
	const doc = {
		version: 1,
		description:
			"Generated from the pinned @earendil-works/pi-ai catalogue by dispatcher-agent/scripts/write-models.ts; " +
			"run npm run models in dispatcher-agent, do not edit. Prices are list prices in USD per million tokens.",
		source: `@earendil-works/pi-ai ${piAI.version}`,
		models,
	};
	return `${JSON.stringify(doc, null, "\t")}\n`;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
	const next = modelsDocument();
	if (process.argv.includes("--check")) {
		let current = "";
		try {
			current = readFileSync(MODELS_PATH, "utf8");
		} catch {}
		if (current !== next) {
			console.error("internal/dispatchermodel/models.json is out of date: run npm run models in dispatcher-agent");
			process.exit(1);
		}
		console.log("models.json matches the pinned pi-ai catalogue");
	} else {
		writeFileSync(MODELS_PATH, next);
		console.log("wrote internal/dispatchermodel/models.json");
	}
}
