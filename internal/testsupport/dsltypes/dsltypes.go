// Package dsltypes reads the field-type table out of the type-mapping
// reference so tests can assert the code implements what the page promises.
//
// # Why the doc is the source
//
// A DSL type is spelled out in six places that must agree: schema.SQLType
// renders the DDL, coltype.ProtoType and coltype.GoType shape the caller's
// generated API, coltype.ScanFragments and coltype.BindExpr move values across
// the wire, entity.setProtoType publishes the same wire type from the runtime
// dispatcher, and docs/reference/dsl-types.md tells the customer what to
// expect. Nothing connected them and they drifted: `real` and `double` were
// documented, parsed, and implemented in NONE of the code tables. `double`
// rendered as the Postgres type `DOUBLE`, which does not exist.
//
// Restating the type list inside each test would add a seventh place to
// forget. Reading the page instead makes it the contract, and makes a type
// documented-but-unimplemented a test failure rather than a support ticket.
//
// # Why a package rather than a helper per test
//
// Two packages need these rows — internal/codegen/coltype, which owns four of
// the mappings, and internal/server/entity, which owns the dispatcher's. A
// copy of the parser in each is the same drift this exists to prevent, one
// level up.
package dsltypes

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DocPath is the reference page, relative to the repository root.
const DocPath = "docs/reference/dsl-types.md"

// ArrayRowSpelling is the one documented row that is a template rather than a
// type: `[]T` maps to `T[]` for whatever T is. Callers assert arrays against a
// concrete element type instead of substituting into this row.
const ArrayRowSpelling = "[]T"

// Row is one line of a four-column type table. The Go column is carried
// because a caller may want it, but note the page describes the
// proto-generated Go type there — `*timestamppb.Timestamp` for a timestamp —
// which is not what coltype.GoType returns for the scan side.
type Row struct {
	ATL   string
	PG    string
	Proto string
	Go    string
}

var (
	cellSplit  = regexp.MustCompile(`\s*\|\s*`)
	backticked = regexp.MustCompile("`([^`]+)`")
)

// Rows parses every four-column type table on the reference page.
//
// Returns an error rather than taking a *testing.T so the caller decides
// between Fatal and Skip, and so this package does not import testing.
func Rows() ([]Row, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, DocPath)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w — this page is the only thing "+
			"keeping the type mappings in agreement", path, err)
	}

	var out []Row
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := cellSplit.Split(strings.Trim(line, "|"), -1)
		if len(cells) != 4 {
			// The nullability table is three columns and describes generated
			// Go rather than the mapping; skipping it by shape is deliberate.
			continue
		}
		// A cell carries prose outside the backticks — "`vector(N)` (pgvector)"
		// — so the backticked span is the value and the rest is commentary. A
		// cell with no backticks at all is a header or separator row.
		vals := make([]string, 4)
		ok := true
		for i, c := range cells {
			m := backticked.FindStringSubmatch(c)
			if m == nil {
				ok = false
				break
			}
			vals[i] = m[1]
		}
		if !ok {
			continue
		}
		out = append(out, Row{ATL: vals[0], PG: vals[1], Proto: vals[2], Go: vals[3]})
	}

	// A floor, because every failure mode of the parser above is "returns
	// fewer rows" and a silently empty table would turn every caller into a
	// test that asserts nothing. The real count is comfortably above this;
	// the number only has to be high enough that a reformatted table cannot
	// slip through as a handful of rows.
	if len(out) < 15 {
		return nil, fmt.Errorf("parsed only %d rows from %s — the table format "+
			"changed and every test built on this is now checking almost nothing",
			len(out), path)
	}
	return out, nil
}

// Substitute replaces the page's parameter letters with a sample's numbers, so
// the documented `VARCHAR(N)` can be compared against the `VARCHAR(255)` the
// code emits. Word-bounded, so the N in `VARCHAR(N)` is replaced and the N in
// `NUMERIC` is not.
func Substitute(s string, placeholders map[string]string) string {
	for letter, val := range placeholders {
		s = regexp.MustCompile(`\b`+regexp.QuoteMeta(letter)+`\b`).ReplaceAllString(s, val)
	}
	return s
}

// SQLTypeEqual compares Postgres type spellings. Case is insignificant to
// Postgres, and the page writes `NUMERIC(p,s)` where the emitter writes
// `NUMERIC(12, 2)` — the space is formatting, not a difference.
func SQLTypeEqual(a, b string) bool {
	norm := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(s, " ", ""))
	}
	return norm(a) == norm(b)
}

// repoRoot walks up from the working directory to the module root, so a test
// in any package finds the same page without hard-coding its own depth.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
