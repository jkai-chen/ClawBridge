// The Sessions drawer's History block. Reading a session's inbox as that
// session marks its messages delivered and makes it look alive (bus inbox,
// touchReadActor), so a person's own read waits for FR46. Until then the
// block says so, for every session.
import { h } from "../dom";

export const HISTORY_OWN = "History arrives in a later build.";
export const HISTORY_OTHER = "Messages you exchanged with this session arrive in a later build.";

export function historyBlock(own: boolean): HTMLElement {
  return h("section", { class: "block", "data-block": "history", "aria-label": "History" },
    h("h3", { text: "History" }),
    h("p", { class: "note", text: own ? HISTORY_OWN : HISTORY_OTHER }),
  );
}
