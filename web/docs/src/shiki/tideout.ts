import type { LanguageRegistration } from "shiki";

/**
 * TextMate grammar for captured `tide` output.
 *
 * The glyph vocabulary is the one the CLI prints and the tide reference
 * documents under "Output": `✔`, `✖` and `ℹ` prefix status lines, table
 * output prints bare rows, and the diff marks carry the plan class —
 * `+` additive, `~` backfill, `✗` destructive.
 *
 * A fence tagged `tideout` is a transcript, not a command to run. Leading
 * whitespace is significant in every block that uses it: `tide history`
 * indents its timeline by two and continues a node by seven.
 */
export const tideout: LanguageRegistration = {
  name: "tideout",
  scopeName: "source.tideout",
  // Every rule is a top-level match, so there is nothing to name here.
  repository: {},
  patterns: [
    /* Section rules and the timeline's uprights. */
    { match: "─+", name: "comment.line.rule.tide" },
    { match: "[│├└┌┐┘┬┴┼]", name: "comment.line.rule.tide" },

    /* Timeline nodes: filled is an applied version, hollow is a seed. */
    { match: "●", name: "markup.inserted.tide" },
    { match: "◌", name: "comment.line.rule.tide" },

    /* Status glyphs. */
    { match: "[✔✓]", name: "markup.inserted.tide" },
    { match: "[✖✗✘]", name: "markup.deleted.tide" },
    { match: "ℹ", name: "constant.other.reference.tide" },
    { match: "[→⇒]|->", name: "comment.line.rule.tide" },

    /* Diff marks, at the head of a line and after the indent. */
    { match: "^\\s*\\+.*$", name: "markup.inserted.tide" },
    { match: "^\\s*-(?!-).*$", name: "markup.deleted.tide" },
    { match: "^\\s*~.*$", name: "markup.changed.tide" },

    /* Counts in a summary line: `+4 additive  ~1 backfill  ✗1 destructive`. */
    { match: "\\+\\d+\\s+\\w+", name: "markup.inserted.tide" },
    { match: "~\\d+\\s+\\w+", name: "markup.changed.tide" },

    /* A column header row is all caps and at least two columns wide. */
    { match: "^[A-Z][A-Z_ ]{6,}$", name: "markup.heading.tide" },

    /* The title line a subcommand prints above its output. */
    {
      match: "^\\s*(?:Schema History|Blame|Rollback preview|diff)\\b.*$",
      name: "markup.heading.tide",
    },

    /* Schema versions, which the prose refers to by these names. */
    { match: "\\bv\\d+\\b", name: "constant.other.reference.tide" },
  ],
};
