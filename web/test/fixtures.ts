// The RPC answers the Go backend's own tests replay
// (internal/guibackend/testdata/rpc). The page is tested against the very
// same files, so the two cannot drift apart. Synthetic data only.
import { readFileSync } from "node:fs";
import { join } from "node:path";

const DIR = join(__dirname, "..", "..", "internal", "guibackend", "testdata", "rpc");

function result(name: string): unknown {
  const doc = JSON.parse(readFileSync(join(DIR, name), "utf8")) as { response: { result?: unknown } };
  return doc.response.result;
}

export const fixtures = {
  hello: result("hello.json"),
  sessionsList: result("sessions-list.json"),
  doctorChecks: result("doctor-checks.json"),
  needsList: result("needs-list.json"),
  updatesStatus: result("updates-status.json"),
  updatesCheck: result("updates-check.json"),
};

/** A hello with every field the shell shows (the fixture has the minimum). */
export const fullHello = {
  client_version: "1.0.0-test",
  protocol: 1,
  mode: "cloud",
  platform: "linux",
  profile: { workspace: "Northwind Labs", project: "atlas-api", waiting_for_project: false, principal: "mara@example.test", device: "mara-mbp" },
};

export const sessionsEmpty = { source: "local-authority", data: { sessions: [] } };

export const sessionsHostile = {
  source: "local-authority",
  data: {
    sessions: [
      {
        id: "ses_hostile1",
        name: "<img src=x onerror=alert(1)>",
        owner_principal: "evil‮@example.test",
        host: "host\u001b[31mred\u001b[0m",
        agent_kind: "claude-code",
        status: "active",
        presence: "online",
        stale: false,
        assignment: { state: "confirmed", issue_identifier: "<script>alert(2)</script>" },
        updated_at: "2026-10-31T10:46:00Z",
      },
    ],
  },
};
