package admin

import (
	"go/ast"
	"strings"
	"testing"
)

// mustScope names the RPCs that read or move rows in atlantis.jobs and
// atlantis.jobs_dead, and therefore have to consult jobReadScope or jobOwner.
//
// The list is checked for staleness below, so a renamed RPC fails here rather
// than falling off silently and taking its scoping requirement with it.
// StartWorkflow is on the list because it INSERTs a job row, and it is the
// reason the completeness test below exists: it was missed when the other four
// were scoped, and no behavioural test noticed. A workflow's first step would
// have carried an empty owner_caller — readable by operators and by nobody
// else, including the caller that started the workflow.
var mustScope = []string{"SubmitJob", "GetJobStatus", "ListDeadJobs", "RetryDeadJob", "StartWorkflow"}

// TestJobRPCsScopeToTheCaller mirrors TestMutatingRPCsGateOnChangePolicy.
//
// The behavioural tests in job_scope_pg_test.go prove the scoping works on the
// three paths they drive. This is what stops a FOURTH path being added beside
// them — which is exactly how this package came to have two RPCs selecting job
// rows by id alone while CAPABILITY_JOBS_READ sat in the bundle every caller
// receives. Nothing failed to compile and no test went red; the RPCs were
// simply written without the predicate, and a job's args and error text are
// the most revealing thing in the table.
//
// An AST check rather than a behavioural one because the failure it guards
// against is a function that does not exist yet.
func TestJobRPCsScopeToTheCaller(t *testing.T) {
	_, files := parsePackageSources(t)

	found := map[string]bool{}
	declared := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || !contains(mustScope, fn.Name.Name) {
				continue
			}
			if receiverTypeName(fn) != "Service" {
				continue
			}
			declared[fn.Name.Name] = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if sel.Sel.Name == "jobReadScope" || sel.Sel.Name == "jobOwner" {
						found[fn.Name.Name] = true
					}
				}
				return true
			})
		}
	}

	for _, name := range mustScope {
		if !declared[name] {
			t.Fatalf("mustScope names %s, which this package does not declare on "+
				"*Service — the list is stale", name)
		}
		if !found[name] {
			t.Errorf("%s touches job rows but never calls jobReadScope or jobOwner. "+
				"Every registered caller holds CAPABILITY_JOBS_READ, so an unscoped "+
				"query here reads every other caller's job args.", name)
		}
	}
}

// TestEveryJobTableQueryIsInTheScopedSet is the other half: the list above is
// only as good as its coverage of the file.
//
// It scans every *Service method in the package for a string mentioning
// atlantis.jobs or atlantis.jobs_dead, and requires each one to be in
// mustScope. A fifth job RPC added to this package therefore fails here on the
// day it is written, rather than the day someone thinks to extend the list —
// which is the failure mode a hand-maintained list always eventually has.
func TestEveryJobTableQueryIsInTheScopedSet(t *testing.T) {
	_, files := parsePackageSources(t)

	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil || receiverTypeName(fn) != "Service" {
				continue
			}
			if contains(mustScope, fn.Name.Name) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok {
					return true
				}
				sql := lit.Value
				if !strings.Contains(sql, "atlantis.jobs") {
					return true
				}
				// The dispatcher's claim/heartbeat path runs as the server
				// itself rather than on behalf of a caller, and has no request
				// identity to scope by. Named explicitly so the exemption is a
				// decision someone made rather than a gap in a regexp.
				if strings.HasPrefix(fn.Name.Name, "Worker") || fn.Name.Name == "SetDispatcher" {
					return true
				}
				t.Errorf("%s queries atlantis.jobs but is not in mustScope, so nothing "+
					"requires it to scope by owner_caller. Add it to the list and give "+
					"it a predicate, or record here why it is exempt.", fn.Name.Name)
				return false
			})
		}
	}
}
