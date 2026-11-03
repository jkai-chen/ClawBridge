// Project integrations: the services this project works with. The landing is
// provider-neutral; each provider contributes one card and one section.
import { h } from "../dom";
import { routeArg } from "../router";
import { titleRow, type View, type ViewCtx } from "./common";
import { linearCard, linearSection } from "./linear";

interface Provider {
  id: string;
  /** The card on the landing. */
  card(ctx: ViewCtx, host: HTMLElement): void;
  /** The provider's own pages, at #/integrations/<id>[/...]. */
  section: View;
}

const PROVIDERS: Provider[] = [{ id: "linear", card: linearCard, section: linearSection }];

export const integrationsView: View = (ctx: ViewCtx) => {
  const provider = PROVIDERS.find((p) => p.id === routeArg(location.hash));
  if (provider) return provider.section(ctx);
  const cards = h("div", { class: "stack" });
  ctx.root.replaceChildren(
    titleRow("Project integrations", "The services this project works with. Read only in this build."),
    cards);
  for (const p of PROVIDERS) {
    const host = h("div", null);
    cards.append(host);
    p.card(ctx, host);
  }
};
