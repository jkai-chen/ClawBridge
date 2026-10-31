import { h, visible } from "../dom";
import { copyButton, emptyBlock, titleRow, type View } from "./common";

/** Help → Logs: output the shell discarded before the preamble, plus stderr. */
export const logsView: View = ({ root, api, isCurrent }) => {
  const load = () => {
    api.logs().then((lines) => {
      if (!isCurrent()) return;
      const text = lines.map(visible).join("\n");
      root.replaceChildren(
        titleRow("Logs", "What the client printed outside its answers: shell start-up output and stderr.", [
          h("button", { class: "btn sm", type: "button", on: { click: load }, text: "Refresh" }),
          lines.length ? copyButton(text) : null,
        ]),
        lines.length ? h("pre", { class: "logs", text }) : emptyBlock("Nothing logged.", "The client has printed nothing outside its answers."),
      );
    }, (e: unknown) => {
      if (isCurrent()) root.replaceChildren(titleRow("Logs"), emptyBlock("Couldn't read the logs.", visible(e instanceof Error ? e.message : String(e))));
    });
  };
  load();
};
