import type { LanguageRegistration } from "shiki";

/**
 * TextMate grammar for the atl schema language.
 *
 * The three word lists come from "Reserved words" in the DSL grammar
 * reference, and the type list from the DSL types reference. Both are
 * checked against the lexer by TestEveryKeywordIsAccountedForByTheReference
 * — this file is not, so a keyword added to the language and to those pages
 * still highlights as an identifier until it is added here too.
 */

/* Cannot name a field: each one begins an entity member. */
const reserved = [
  "as", "asc", "by", "cache", "cascade", "check", "chunk_time_interval",
  "consistency", "cosine", "deferrable", "delete", "desc", "entity",
  "eventual", "expr", "for", "gin", "has_many", "has_one", "heartbeat",
  "hnsw", "hypertable", "in", "index", "input", "insert", "invalidate",
  "invalidate_on", "ip", "is", "keyless", "l2", "not", "on", "ops", "output",
  "partial", "partition", "primary", "procedure", "query", "query_timeout",
  "raw", "restrict", "self", "set", "soft_delete", "sql", "steps", "strict",
  "table", "touch_on_update", "touches", "ttl_field", "unique", "update",
  "via", "where", "write",
];

/* Field modifiers, which may also name a field; indentation separates them. */
const modifiers = ["identity", "serial", "default", "references", "backfill"];

/* Keywords only inside a block or declaration of their own. */
const contextual = [
  "args", "compensate", "enqueue", "enum", "ephemeral", "job", "queue",
  "read_through", "retries", "schedule", "state", "step", "tag", "timeout",
  "ttl", "visible_to", "workflow",
];

const types = [
  "bigint", "bit", "boolean", "box", "bytea", "char", "cidr", "circle",
  "citext", "date", "daterange", "double", "inet", "int", "int4range",
  "int8range", "interval", "json", "jsonb", "line", "lseg", "macaddr",
  "macaddr8", "money", "name", "numeric", "numrange", "path", "point",
  "polygon", "real", "smallint", "text", "time", "timestamp", "timestamptz",
  "timetz", "tsquery", "tsrange", "tstzrange", "tsvector", "uuid", "varbit",
  "varchar", "xml",
];

const alt = (words: string[]) => `\\b(?:${words.join("|")})\\b`;

export const atl: LanguageRegistration = {
  name: "atl",
  scopeName: "source.atl",
  patterns: [
    { include: "#comment" },
    { include: "#string" },
    { include: "#declaration" },
    { include: "#type" },
    { include: "#keyword" },
    { include: "#number" },
    { include: "#punctuation" },
  ],
  repository: {
    comment: {
      match: "//.*$",
      name: "comment.line.double-slash.atl",
    },
    string: {
      begin: '"',
      end: '"',
      name: "string.quoted.double.atl",
      patterns: [{ match: "\\\\.", name: "constant.character.escape.atl" }],
    },
    /* `entity Order in shop` — the declared name reads as the subject of the
       block, so it takes the brightest token rather than the keyword's. */
    declaration: {
      match: `\\b(entity|enum|job|workflow|procedure|query|table)\\s+([A-Za-z_][A-Za-z0-9_]*)`,
      captures: {
        1: { name: "keyword.control.atl" },
        2: { name: "entity.name.class.atl" },
      },
    },
    type: {
      patterns: [
        { match: alt(types), name: "support.type.atl" },
        { match: "\\b(true|false|null|now)\\b", name: "constant.language.atl" },
      ],
    },
    keyword: {
      patterns: [
        { match: alt(reserved), name: "keyword.control.atl" },
        { match: alt(contextual), name: "keyword.control.atl" },
        { match: alt(modifiers), name: "storage.modifier.atl" },
      ],
    },
    number: {
      match: "\\b\\d+(?:\\.\\d+)?\\b",
      name: "constant.numeric.atl",
    },
    punctuation: {
      match: "[{}(),.]",
      name: "punctuation.atl",
    },
  },
};
