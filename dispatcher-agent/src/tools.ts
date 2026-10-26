// Which d4 tools a case gets, and their parameter schemas narrowed to the case (deny by default, d4 §3).
// The server refuses anything outside the case's set anyway; narrowing here keeps the model's choices honest.
import { Type, type TSchema } from "typebox";
import { type CaseT, type OptInTool, type ToolName, ToolRequests } from "./schema.ts";

const READS = ["read_issue_facts", "list_domain", "list_sessions", "session_activity"] as const satisfies readonly ToolName[];

const DESCRIPTIONS: Record<ToolName, string> = {
	read_issue_facts: "Read the typed facts of an issue named in this case.",
	list_domain: "Read a domain: its match, owner and coordinator session.",
	list_sessions: "List the project's sessions with their presence, status and assignment.",
	session_activity: "Read a session's recent activity counts.",
	list_decisions: "Read the dispatcher's recent decisions, to explain them.",
	recommend: "End the case: recommend one action, with a reason code and a short explanation for a person.",
	decide: "End the case: choose one permitted action, with a reason code and a short explanation.",
	reply: "End the case: answer the person's question.",
	abstain: "End the case without an answer, when the facts don't support one.",
	route_notice: "Deliver the notice named in this case to a domain's coordinator.",
	ask_session: "Send a session one fixed-template question about an issue.",
};

export interface CaseTool {
	name: ToolName;
	description: string;
	parameters: TSchema;
	outcome: boolean;
}

const enumOf = (values: readonly string[]) => Type.Enum([...values]);

/** The tools for one case, by kind; opt-in tools only when hello listed them, and never in chat [DM-M1]. */
export function toolsForCase(c: CaseT, optIn: readonly OptInTool[]): CaseTool[] {
	const names: ToolName[] = [...READS];
	const reasons = c.reason_codes;
	const permitted = c.permitted_actions ?? [];
	if (c.kind === "chat") names.push("list_decisions", "reply");
	if (c.kind === "advise" && reasons.length > 0) names.push("recommend");
	if (c.kind === "decide" && reasons.length > 0 && permitted.length > 0) names.push("decide");
	names.push("abstain");
	if (c.kind !== "chat") names.push(...optIn);

	return names.map((name) => {
		let parameters: TSchema = ToolRequests[name];
		if (name === "recommend") {
			parameters = Type.Object(
				{ ...ToolRequests.recommend.properties, reason_code: enumOf(reasons) },
				{ additionalProperties: false },
			);
		} else if (name === "decide") {
			parameters = Type.Object(
				{ ...ToolRequests.decide.properties, action: enumOf(permitted), reason_code: enumOf(reasons) },
				{ additionalProperties: false },
			);
		}
		const outcome = name === "recommend" || name === "decide" || name === "reply" || name === "abstain";
		return { name, description: DESCRIPTIONS[name], parameters, outcome };
	});
}

export const MAX_READ_CALLS = 8; // d4 §4, mirrored locally; outcome calls don't count
