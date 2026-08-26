# atlantis

## Comments

The model is the Go standard library. Every number below is measured over
`runtime`, `sync`, `net/http`, `database/sql`, `crypto/tls` and `os`,
excluding tests, and is the target this tree is held to.

| | stdlib | atlantis at the start |
|---|---|---|
| comment lines / non-blank | 28.2% | 31.1% |
| mean block | 3.4 lines | 5.3 lines |
| blocks of 1–2 lines | 58% | 36% |
| blocks of 9+ lines | 7% | 16% |
| counterfactual lines per 100 | 0.9 | 6.6 |
| files using `// #` headings | 1.0% | 29% |

The volume was never the problem. Shape and content were.

### 1. Say what the code does not

The test is deletion: remove the comment and ask whether a competent reader
has lost a fact. If not, it should not exist.

    // A provider in this map has routes; one absent from it has none.

That is what the function is. It restates the code and goes.

### 2. Describe what is, not what isn't

No rejected alternatives. "Rather than", "instead of", "the alternative",
"would otherwise" — a comment documents the code that exists, not the designs
that lost. This is the fault that survives every other rule, because a short
argument is still an argument.

    // Registering the routes and failing at the redirect instead produces an
    // error page for an unconfigured feature, which reads as a broken
    // deployment.

A counterfactual earns its place only when it names a specific failure the
code prevents and a reader could reintroduce — "a NULL comparison yields
false, so the row would never be claimed". Never "the alternative would be
worse".

### 3. Rationale goes in the body, not the doc comment

From Go's own doc-comment standard: a doc comment says what a symbol is or
does; "explanations of design decisions belong in code comments inside
functions". Rationale above `func` is how every essay in this tree started.

Start with the symbol name. Booleans use "reports whether". Document special
cases, zero values, concurrency guarantees, and what the caller must do:
"The caller must hold worldsema."

### 4. No headings

`// # Heading` is valid Go and is a package-doc device for a large package —
`time`, `os/exec`, `io/fs`. It appears in 62 of 5,957 stdlib files. A heading
above a function, a const or a struct field means the comment became an essay;
the fix is to cut it, not to label it. Same for `-- ── box rules ──` in SQL
and `**bold**` anywhere.

### 5. No repository history

"It used to be", "an earlier draft", "a previous version of this comment".
That is `git log`. Write what is true now — and verify it still is: three
comments in this tree described behaviour that had already changed, including
two package docs.

### 6. No editorialising, no rhetoric, no people

Banned: deliberately, on purpose, the whole point, worth stating, not
tidiness, load-bearing, X is worse than Y. State the constraint and stop; a
reader cannot check an adjective.

No person as the subject of a sentence — somebody, nobody, anyone, you, the
reader, an operator. Say what the system does, not how a person feels about
it. No metaphor, no antithesis ("an error, not a default"), no aphorism.

### 7. The shape

One sentence of what, then discrete notes of one or two lines, each carrying a
single fact, separated by a blank `//`. Not a sequence of paragraphs.

    // gcMarkDoneFlushed counts the number of P's with flushed work.
    //
    // Ideally this would be a captured local in gcMarkDone, but forEachP
    // escapes its callback closure, so it can't capture anything.
    //
    // This is protected by markDoneSema.

### 8. Length is earned by data, not by importance

A block over eight lines needs a table, a byte layout, a measurement or an
error string visible inside it. Prefer a number to an adjective: "≤1000
entries, 2 bytes each" documents, "a lot" does not. Keep measurements when
cutting — the benchmark tables in migration 0024 and `objects.go` earn every
line they take.

### Rewriting is deletion, not compression

Compressing an essay yields a shorter essay and moves no metric. The observed
failure, repeatedly: delete the `// #` header, fold its text into a lead-in
sentence, leave the paragraph beneath. Header counts fell 446 → 13 while the
block-length distribution did not move at all.

Per block, ask what single fact a reader needs that the code does not state,
write that, and delete the rest. Most paragraphs go entirely.

### Checking

By reading the code beside the comment. There is no tool for this.

A grep over rules 4, 5, 6 and 8 finds headings, banned words and long blocks,
and it was tried: the files it passed still restated their code and still
argued for designs that lost. Passing a mechanical check became the reason to
stop looking, which is worse than having no check at all.

Read the comment. Read the code under it. Ask what the reader loses if the
comment goes. Write that, and nothing else.
