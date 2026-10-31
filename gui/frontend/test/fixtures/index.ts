// Synthetic RPC replies (example.test only) for the tests and the dev mock.
//
// TODO: internal/guibackend/testdata/rpc is being written on gui/backend-core
// (P2-1). When that has landed, point these imports at the shared files and
// delete this directory. The shapes follow what the backend returns for the
// TUI's list_sessions (`sessions[]`) and doctor.Report (`status`,
// `generated_at`, `checks[]`).
import hello from "./hello.json";
import sessionsList from "./sessions.list.json";
import sessionsEmpty from "./sessions.empty.json";
import sessionsHostile from "./sessions.hostile.json";
import doctorChecks from "./doctor.checks.json";

export const fixtures = { hello, sessionsList, sessionsEmpty, sessionsHostile, doctorChecks };
