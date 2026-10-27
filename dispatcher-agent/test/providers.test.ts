// The provider list the CLI and TUI offer, and the server accepts (internal/dispatchermodel/providers.json), may name
// only what this agent can run: every entry's API is one of SUPPORTED_APIS, and every fixed base URL is one
// buildProvider accepts. A provider added on either side without the other fails CI here.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { buildProvider, SUPPORTED_APIS } from "../src/provider.ts";

const FILE = join(import.meta.dirname, "..", "..", "internal", "dispatchermodel", "providers.json");
const list = JSON.parse(readFileSync(FILE, "utf8")) as {
	version: number;
	providers: { id: string; api: string; base_url?: string; custom?: boolean }[];
};

test("every listed provider is one this agent can run", async () => {
	assert.equal(list.version, 1);
	assert.ok(list.providers.length > 0);
	for (const p of list.providers) {
		assert.ok(SUPPORTED_APIS.includes(p.api), `${p.id}: ${p.api} is not a supported API`);
		assert.equal(p.custom === true, p.base_url === undefined, `${p.id}: a custom provider has no base_url, a listed one has`);
		const config = { id: p.id, api: p.api, base_url: p.base_url ?? "https://llm.example.test/v1", model: "m", key: "k" };
		const built = await buildProvider(config, { max_input_tokens_per_run: 1000, max_output_tokens_per_run: 100 });
		assert.equal(built.model.baseUrl, config.base_url);
	}
});
