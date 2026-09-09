// Holds the join between a sink and the values that report through it.
//
// Everything below this — the projections, the catalogue, the store's report
// path — passes with no sink at all: report returns at its first line when
// Store.sink is nil, and Capture is a method on an interface a nil field never
// reaches. A process that builds no sink is a green test suite reporting
// nothing.
package analytics_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sinkWiring maps a command to the number of values it hands a sink to.
//
// cmd/provisioner has two: the store, whose audit rows carry the projected
// events, and the worker, which captures the rest. cmd/cloud is absent because
// its sink is built inside server.New, where Server.Close covers it.
var sinkWiring = map[string]int{
	"../../cmd/provisioner": 2,
}

// sinkClosedAfter names the call each command's sink must be closed after.
//
// enqueue selects on done before sending, so a Capture that follows Close
// counts a drop. Closing before the loop that produces the events leaves the
// wiring in place and delivers none of them.
var sinkClosedAfter = map[string]string{
	"../../cmd/provisioner": "Run",
}

// TestEveryReportingProcessWiresAndClosesItsSink reads each command for the
// calls that hand a sink out and the one that flushes it.
func TestEveryReportingProcessWiresAndClosesItsSink(t *testing.T) {
	for dir, want := range sinkWiring {
		wired, closed, marks := sinkCalls(t, dir)

		if len(wired) != want {
			t.Errorf("%s hands a sink to %d values, want %d — a reporting path is wired to nothing",
				dir, len(wired), want)
		}
		for _, name := range wired {
			if name != wired[0] {
				t.Errorf("%s wires two different sinks, %q and %q; the events would be split",
					dir, wired[0], name)
			}
		}
		if len(wired) == 0 {
			continue
		}
		closedAt, ok := closed[wired[0]]
		if !ok {
			t.Errorf("%s never closes %q, so the last batch is dropped on shutdown",
				dir, wired[0])
			continue
		}

		after := sinkClosedAfter[dir]
		producedAt, ok := marks[after]
		if !ok {
			t.Errorf("%s has no call to %s, which is what this expects the close to follow",
				dir, after)
			continue
		}
		if closedAt.Filename != producedAt.Filename || closedAt.Offset < producedAt.Offset {
			t.Errorf("%s closes the sink at %s, before %s at %s; every event that call "+
				"captures would be dropped", dir, closedAt, after, producedAt)
		}
	}
}

// sinkCalls returns the identifiers passed to UseAnalytics, where Close is
// called on each identifier, and where the calls named in sinkClosedAfter are.
func sinkCalls(t *testing.T, dir string) (
	wired []string, closed map[string]token.Position, marks map[string]token.Position,
) {
	t.Helper()
	closed = map[string]token.Position{}
	marks = map[string]token.Position{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "UseAnalytics":
				if len(call.Args) != 1 {
					return true
				}
				if id, ok := call.Args[0].(*ast.Ident); ok {
					wired = append(wired, id.Name)
				}
			case "Close":
				if id, ok := sel.X.(*ast.Ident); ok {
					if _, seen := closed[id.Name]; !seen {
						closed[id.Name] = fset.Position(call.Pos())
					}
				}
			}
			if _, watched := marks[sel.Sel.Name]; !watched {
				for _, after := range sinkClosedAfter {
					if sel.Sel.Name == after {
						marks[sel.Sel.Name] = fset.Position(call.Pos())
					}
				}
			}
			return true
		})
	}
	return wired, closed, marks
}

// groupIdentifyCallers are the files allowed to set organisation group
// properties. internal/analytics is where the method is declared.
var groupIdentifyCallers = map[string]bool{
	"../../internal/provisioner/worker.go": true,
}

// TestOnlyTheProvisionerIdentifiesGroups finds every call that sets group
// properties.
//
// $group_set merges the keys it is given and leaves the rest, which is what
// purgeOne relies on when it sends state and purged_at alone. One caller is
// what keeps the union of keys a group carries equal to the four
// internal/analytics declares, and reviewable in one place.
func TestOnlyTheProvisionerIdentifiesGroups(t *testing.T) {
	found := 0
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if strings.HasPrefix(path, "../../internal/analytics/") {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "GroupIdentify" {
					return true
				}
				found++
				if !groupIdentifyCallers[path] {
					t.Errorf("%s sets group properties and is not a declared caller; "+
						"a partial map here clears what the provisioner wrote",
						fset.Position(call.Pos()))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if found == 0 {
		t.Fatal("no group-property call was found, so this asserts nothing — " +
			"either the walk is broken or nothing sets them any more")
	}
}
