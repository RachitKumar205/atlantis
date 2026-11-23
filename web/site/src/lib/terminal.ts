import type { ShikiTransformer } from "shiki";

// lineBlocks drops the newline text between Shiki's line spans, so CSS can set
// each line as a block and the flow demo can hide a line until it prints.
export const lineBlocks: ShikiTransformer = {
  name: "line-blocks",
  code(node) {
    node.children = node.children.filter((child) => !(child.type === "text" && child.value === "\n"));
  },
};

// marked sets data-mark on every line that starts with prefix.
export function marked(prefix: string): ShikiTransformer {
  return {
    name: "marked",
    line(node, line) {
      if (this.source.split("\n")[line - 1]?.startsWith(prefix)) node.properties["data-mark"] = "";
    },
  };
}

// pins numbers the first line that starts with each prefix, from 1, in a
// data-pin attribute.
export function pins(prefixes: string[]): ShikiTransformer {
  return {
    name: "pins",
    line(node, line) {
      const lines = this.source.split("\n");
      const pin = prefixes.findIndex((p) => lines.findIndex((l) => l.startsWith(p)) === line - 1);
      if (pin >= 0) node.properties["data-pin"] = String(pin + 1);
    },
  };
}

// gitDiff marks lines with the classes git's colour output uses: bold file
// headers, a coloured hunk header, and removed and added lines.
export const gitDiff: ShikiTransformer = {
  name: "git-diff",
  line(node, line) {
    const text = this.source.split("\n")[line - 1] ?? "";
    let mark = "";
    if (/^(diff --git |index |--- |\+\+\+ )/.test(text)) mark = "diff-meta";
    else if (text.startsWith("@@")) mark = "diff-hunk";
    else if (text.startsWith("-")) mark = "diff-del";
    else if (text.startsWith("+")) mark = "diff-add";
    if (mark) this.addClassToHast(node, mark);
  },
};
