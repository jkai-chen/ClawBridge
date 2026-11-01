import { h } from "../dom";
import { emptyBlock, titleRow, type View } from "./common";

export interface PlaceholderSpec {
  title: string;
  /** The screen's one question (GUI-D-SPEC §5). */
  question: string;
}

/** A screen that arrives in a later slice. Every write stays off until P3. */
export function placeholder(spec: PlaceholderSpec): View {
  return ({ root }) => {
    root.replaceChildren(
      titleRow(spec.title),
      h("p", { class: "placeholder-q", text: spec.question }),
      h("div", { class: "section" }, emptyBlock("Not in this build yet.")),
    );
  };
}
