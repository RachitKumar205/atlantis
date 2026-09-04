# Documentation style

This standard governs every page under `docs/` and the repository
`README.md`. Pages under `ops/` are exempt from the voice rules, never from
accuracy.

## Page types

Every page is exactly one of four types. Do not mix types within a page: a
how-to that needs a paragraph of theory links to a concept page instead.

| Type | Job | Title | Structure |
|---|---|---|---|
| Get started | Shortest path from nothing to one working result | — | Prerequisites → numbered steps → verify the result → next steps. Minimum setup, most general case. Judged by how fast a reader succeeds, not by coverage. |
| How-to | Complete one task the reader has already chosen | Verb phrase ("Add a new entity"), no "How to" prefix | Prerequisites → numbered steps with exact commands and UI labels → literal output in code blocks → verification → next steps. |
| Concept | Build the mental model: what something is, why it works that way, where its boundaries are | Noun phrase ("Schema as code") | Prose, short paragraphs, one idea per section. Boundaries as flat bullets. Opinion is allowed here ("use X for Y") — nowhere else. No steps. |
| Reference | Complete, neutral lookup | The surface's name ("`tide` CLI", "DSL grammar") | Quick-reference table first, then entries in a fixed fact order. Literal values, never "reasonable" or "a few". Describes, never instructs. Nothing hidden behind prose. |

Section `index.md` files are navigation: one line per page, saying what the
reader finds there. They are updated in the same commit as any page add,
move, or delete.

## Voice

- Second person, present tense, active voice. "You declare an entity; the
  server plans a migration."
- No marketing openers. "This page describes", "Learn more about", and
  "In this guide" are banned — start with the substance.
- No history. "Used to be", "an earlier version", "before X existed" is
  `git log`, not documentation. Write what is true now.
- No rejected alternatives. "Rather than", "instead of", "the alternative
  would" argue for a design; documentation states the one that exists. A
  counterfactual survives only when it names a concrete failure the reader
  could cause.
- No self-congratulation. "Deliberately", "the whole point", "load-bearing",
  "by design" assert importance without information. State the constraint
  and stop.

## Naming

- The product is `atlantis`, lowercase, in running prose. "Atlantis Cloud"
  may open a page or name the hosted platform; inline references stay
  lowercase.
- Customer-facing flows show only `tide`. `tidectl` and the `cloud` CLI are
  internal and appear only under `ops/`.
- Platform limits are stated as numbers ("up to 100 sandboxes per user",
  "a 30-day retention window"), never as server environment variables. A
  customer cannot set the server's `ATL_*` or `ATLANTIS_*` variables on a
  hosted deployment, so those names appear only under `ops/`. The `tide`
  CLI's own client-side variables (`ATL_CALLER`, `ATL_ORG`,
  `ATL_ENROLL_URL`, `ATLANTIS_HOME`, …) are customer surface and belong in
  the CLI reference.

## Verifiability

Every CLI flag, exit code, grammar production, limit, error message, and
behaviour claim is checked against the source before it is written: the flag
definitions in `cmd/tide/*.go`, the parser in `internal/dsl/`, the console
routes in `internal/console/server.go`. A claim that cannot be verified is
cut, not hedged.

Three test suites pin doc content to the code — treat their contracts as
part of the file format:

- `internal/dsl/grammar_doc_test.go` and `grammar_doc_total_test.go` read
  `docs/reference/dsl-grammar.md`: the heading `## Reserved words` must
  exist with exactly three fenced word-list blocks under it, together
  naming every lexer keyword. Fenced blocks containing `{`, `}`, or `"`
  are treated as examples and skipped.
- `internal/dsl/check_placement_test.go` holds a byte-identical copy of the
  "Where a `check` binds" example from the grammar reference. Changing the
  example means changing the test in the same commit.
- `internal/coltype/documented_types_test.go` reads the tables in
  `docs/reference/dsl-types.md`, whose path is pinned by
  `dsltypes.DocPath`.

## Links

Relative links only, checked by the `docs links` CI job (offline lychee,
including `#anchor` fragments). Renaming a heading breaks every link to its
anchor; search for the anchor before renaming.

## Review

Every new or rewritten page gets two reviews before it lands: a technical
grounding pass that checks each claim against source, and a style pass that
enforces this document. Small in-place corrections (a typo, a renamed flag)
need neither.
