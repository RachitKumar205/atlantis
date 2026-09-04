import type { ThemeRegistration } from "shiki";

/**
 * The nine code colours from web/shared/datum.css, as a Shiki theme.
 *
 * The values are duplicated here as hex because Shiki resolves a theme at
 * build time and writes the colour into the token's inline style; a
 * `var(--code-keyword)` would reach the page as that literal string. If a
 * colour changes in datum.css, change it here.
 *
 * Every foreground below clears 4.5 against #0F1417, which is the ground
 * `pre.astro-code` paints in both themes.
 */

const bg = "#0F1417";
const ink = "#DCE3DD";
const comment = "#7C8D81";
const keyword = "#8FB4D4";
const type = "#A8C47E";
const string = "#D8A96B";
const number = "#C9946B";
const entity = "#EDF2EC";
const ref = "#7FC4C0";
const punct = "#8A948C";

export const datumCodeTheme: ThemeRegistration = {
  name: "datum",
  type: "dark",
  colors: {
    "editor.background": bg,
    "editor.foreground": ink,
  },
  tokenColors: [
    { scope: ["comment", "punctuation.definition.comment"], settings: { foreground: comment } },
    {
      scope: ["keyword", "storage", "storage.type", "keyword.control", "keyword.operator.word"],
      settings: { foreground: keyword },
    },
    {
      scope: ["support.type", "entity.name.type", "storage.type.primitive", "constant.language"],
      settings: { foreground: type },
    },
    { scope: ["string", "punctuation.definition.string"], settings: { foreground: string } },
    { scope: ["constant.numeric"], settings: { foreground: number } },
    {
      scope: ["entity.name.class", "entity.name.function", "entity.name.tag", "support.class"],
      settings: { foreground: entity },
    },
    { scope: ["variable", "variable.other", "meta.definition.variable"], settings: { foreground: ink } },
    { scope: ["support.function", "entity.other.attribute-name"], settings: { foreground: ref } },
    { scope: ["punctuation", "meta.brace", "keyword.operator"], settings: { foreground: punct } },

    /* Captured CLI output. The glyph vocabulary is documented under "Output"
       in the tide reference: ✔ and ✖ and ℹ prefix status lines, and the
       diff marks carry the plan class. */
    { scope: ["markup.inserted.tide"], settings: { foreground: "#647345" } },
    { scope: ["markup.deleted.tide"], settings: { foreground: "#B4453C" } },
    { scope: ["markup.changed.tide"], settings: { foreground: "#9A6B1E" } },
    { scope: ["markup.heading.tide"], settings: { foreground: entity } },
    { scope: ["comment.line.rule.tide"], settings: { foreground: punct } },
    { scope: ["constant.other.reference.tide"], settings: { foreground: ref } },
  ],
};
