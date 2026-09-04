// Package dsltypes reads the field-type table out of the type-mapping
// reference so tests can assert the code implements what the page promises.
//
// A DSL type is spelled out in six places that have to agree: schema.SQLType
// renders the DDL, coltype.ProtoType and coltype.GoType shape the generated
// API, coltype.ScanFragments and coltype.BindExpr move values across the wire,
// entity.setProtoType publishes the wire type from the runtime dispatcher, and
// the type-mapping reference states what a caller can expect.
//
// Unconnected, they drift: `real` and `double` were documented and parsed while
// absent from every code table, and `double` rendered as the Postgres type
// `DOUBLE`, which does not exist.
//
// Reading the page makes it the contract, and a documented type that nothing
// implements a test failure. Restating the list inside each test adds a seventh
// place to forget.
//
// A package rather than a helper per test: internal/coltype owns four
// of the mappings and internal/server/entity the dispatcher's, so a copy of the
// parser in each is the same drift one level up.
package dsltypes

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DocPath is the reference page, relative to the repository root.
//
// Front matter precedes the prose; Rows skips every line that does not begin
// with `|`, so the two YAML delimiters and the fields between them are read
// as ordinary non-table lines.
const DocPath = "web/docs/src/content/docs/reference/dsl-types.mdx"

// ArrayRowSpelling is the one documented row that is a template rather than a
// type: `[]T` maps to `T[]` for whatever T is. Callers assert arrays against a
// concrete element type instead of substituting into this row.
const ArrayRowSpelling = "[]T"

// Row is one line of a four-column type table.
//
// The page's Go column holds the proto-generated type —
// `*timestamppb.Timestamp` for a timestamp — which is not what coltype.GoType
// returns for the scan side.
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
			// Go rather than the mapping, so column count skips it.
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

	// A floor: every failure mode of the parser above returns fewer rows, and an
	// empty table turns each caller into a test that asserts nothing. The real
	// count is well above 15, which only has to be high enough that a
	// reformatted table cannot pass as a handful of rows.
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
