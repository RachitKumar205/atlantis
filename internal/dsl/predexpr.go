package dsl

import (
	"strings"
)

// Partial-index `where` predicates are validated by delegating to Postgres's
// own parser (pg_query_go), which is a cgo dependency. Because Go links a
// package as a unit, a single import of pg_query_go anywhere in package dsl
// forces every binary that touches the DSL to link libpg_query — including
// `tide`, whose plan/apply path ships raw .atl bytes to the server and never
// parses locally.
//
// So lowerPredicate lives in two build-tagged files: predexpr_cgo.go carries
// the real implementation, predexpr_nocgo.go a refusing stub. This file holds
// only the parts that are pure Go and are needed by both.
//
// The server is unaffected: it imports internal/dsl/sqlvalidate, which depends
// on pg_query_go directly, so a CGO_ENABLED=0 server build fails at compile
// time rather than silently skipping validation.

// dslToSQL rewrites a captured predicate into SQL: DSL `"..."` string literals
// become SQL `'...'` (decoding DSL escapes, doubling embedded single quotes),
// `/* */` block comments are dropped, and runs of whitespace collapse to a
// single space. SQL `'...'` strings are copied verbatim, and `"`/`'`/comment
// markers inside the other kind of quote are inert (DSL-string state takes
// precedence). The DSL has no quoted identifiers, so a `"..."` is always a
// string.
func dslToSQL(raw string) string {
	var b strings.Builder
	pendingSpace := false
	emit := func(s string) {
		if s == "" {
			return
		}
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
		b.WriteString(s)
	}

	n := len(raw)
	for i := 0; i < n; {
		c := raw[i]
		switch {
		case c == '"':
			j := i + 1
			var s strings.Builder
			for j < n && raw[j] != '"' {
				if raw[j] == '\\' && j+1 < n {
					switch raw[j+1] {
					case 'n':
						s.WriteByte('\n')
					case 't':
						s.WriteByte('\t')
					default:
						s.WriteByte(raw[j+1])
					}
					j += 2
					continue
				}
				s.WriteByte(raw[j])
				j++
			}
			if j < n {
				j++ // closing quote
			}
			emit("'" + strings.ReplaceAll(s.String(), "'", "''") + "'")
			i = j
		case c == '\'':
			j := i + 1
			for j < n {
				if raw[j] == '\'' {
					if j+1 < n && raw[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			emit(raw[i:j])
			i = j
		case c == '/' && i+1 < n && raw[i+1] == '*':
			j := i + 2
			for j+1 < n && !(raw[j] == '*' && raw[j+1] == '/') {
				j++
			}
			j += 2
			if j > n {
				j = n
			}
			pendingSpace = true
			i = j
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			pendingSpace = true
			i++
		default:
			emit(string(c))
			i++
		}
	}
	return strings.TrimSpace(b.String())
}

func dedupeCols(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
