import { site, agentPrompt } from "./config";
import { hero, loop, classes, rehearsal, api, closing, session, parkSchema, parkSql } from "./content";

const fence = (lang: string, code: string) => ["```" + lang, code, "```"].join("\n");
const fenceLang = (lang: string) => (lang === "tideout" ? "text" : lang);

const classMarkdown = (rows: typeof classes.rows) =>
  rows.flatMap((r) => [`- **${r.name}.** ${r.text}`, "", fence("diff", r.diff.join("\n")), ""]);

// pageMarkdown returns the page as markdown, for /index.md and for a request
// to / with `Accept: text/markdown`.
export function pageMarkdown(): string {
  const primary = site.contact ? `[Talk to us](${site.contact})` : `[Sign up](${site.platform})`;
  return [
    `# ${site.headline}`,
    "",
    site.description,
    "",
    `${primary} · [Docs](${site.docs}) · [Getting started](${site.install})`,
    "",
    "Install the `tide` CLI:",
    "",
    fence("sh", site.installCommand),
    "",
    "A prompt to set Atlantis up with your coding agent:",
    "",
    fence("text", agentPrompt),
    "",
    `### ${hero.approval.title}`,
    "",
    hero.approval.text,
    "",
    fence("atl", parkSchema),
    "",
    fence("sql", parkSql),
    "",
    `## ${loop.heading}`,
    "",
    loop.lead,
    "",
    ...loop.steps.flatMap((s, i) => [
      `${i + 1}. **${s.title}.** ${s.text}`,
      "",
      fence(fenceLang(s.lang), `$ ${s.command}\n${s.code}`),
      "",
    ]),
    `## ${classes.heading}`,
    "",
    classes.lead,
    "",
    `**${classes.groups.applies}:**`,
    "",
    ...classMarkdown(classes.rows.slice(0, 1)),
    `**${classes.groups.waits}:**`,
    "",
    ...classMarkdown(classes.rows.slice(1)),
    `## ${rehearsal.heading}`,
    "",
    rehearsal.lead,
    "",
    ...rehearsal.points.map((p, i) => `${i + 1}. **${p.title}.** ${p.text}`),
    "",
    fence("text", session),
    "",
    `## ${api.heading}`,
    "",
    api.lead,
    "",
    fence("atl", api.schema),
    "",
    ...api.outputs.flatMap((o) => [
      `- **${o.name}** (\`${o.step}\`). ${o.text}`,
      "",
      o.rpcs ? `  Methods: ${o.rpcs.map((r) => `\`${r}\``).join(", ")}.` : fence(o.lang, o.code.join("\n")),
      "",
    ]),
    `## ${closing.heading}`,
    "",
    closing.text,
    "",
    `${primary} · [Docs](${site.docs})`,
    "",
  ].join("\n");
}
