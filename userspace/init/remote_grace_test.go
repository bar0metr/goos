package supervisor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"goos/kernel/handle"
	"goos/userspace/remoteapi"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGraceCloseRetainsWorkerAndAdmission(t *testing.T) {
	var registry RemoteRegistry
	id, result := registry.Admit(1, 1)
	if result != remoteapi.ResultOK || registry.Authenticate(id, 1, 1) != remoteapi.ResultOK {
		t.Fatal("admission")
	}
	geometry, _ := remoteapi.MakeGeometry(80, 24, []byte("xterm"))
	if registry.BeginOpen(id, 1, geometry) != remoteapi.ResultOK || registry.CommitOpen(id, 1) != remoteapi.ResultOK {
		t.Fatal("open")
	}
	shell := RemoteResources{Mapping: 0x400000, MappingSize: 4096, Handles: [10]handle.ID{1, 2, 3, 4, 5, 6}}
	worker := RemoteResources{Handles: [10]handle.ID{7, 8, 9, 10}}
	var grace RemoteGrace
	if !grace.Begin(id, 37, 100, 500) || registry.BeginClose(id, 1) != remoteapi.ResultOK {
		t.Fatal("grace")
	}
	runner := remoteReleaseTest{}
	if !shell.Release(&runner) || shell != (RemoteResources{}) || len(runner.handles) != 6 {
		t.Fatal("shell cleanup")
	}
	if worker.Handles[0] != 7 || registry.Authenticated() != 1 {
		t.Fatal("worker or admission released before SSH drain")
	}
	next, result := registry.Admit(2, 1)
	if result != remoteapi.ResultOK || registry.Authenticate(next, 2, 1) != remoteapi.ResultLimit {
		t.Fatal("closing slot lost authenticated capacity")
	}
	if grace.Expired(id, 599) || grace.Expired(id+1, 600) || !grace.Expired(id, 600) {
		t.Fatal("deadline or generation")
	}
	if grace.Begin(id, 0, 200, 500) || grace.Begin(next, 0, 200, 500) || grace.Deadline != 600 || grace.Code != 37 {
		t.Fatal("repeated exit extended or changed grace")
	}
	if grace.Delivered(next) || !grace.NoticePending || !grace.Delivered(id) || grace.NoticePending {
		t.Fatal("notice ownership")
	}
	if !worker.Release(&runner) || registry.FinishClose(id, 1) != remoteapi.ResultOK || registry.Authenticate(next, 2, 1) != remoteapi.ResultOK {
		t.Fatal("final cleanup")
	}
	if len(runner.handles) != 10 || worker != (RemoteResources{}) {
		t.Fatal("worker resources leaked")
	}
}

// The syscall-backed event loop cannot run in host tests. Check its scheduling
// boundary so permanently ready traffic cannot bypass the tested expiry state.
func TestGraceExpiryRunsBeforeReadyInterestSnapshot(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source location")
	}
	source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(file), "..", "cmd", "init", "main.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range source.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "userspaceMain" {
			continue
		}
		for _, statement := range function.Body.List {
			loop, ok := statement.(*ast.ForStmt)
			if !ok {
				continue
			}
			guard, ok := loop.Body.List[0].(*ast.IfStmt)
			if !ok {
				t.Fatal("deadline poll must precede wait interest snapshot")
			}
			condition, ok := guard.Cond.(*ast.Ident)
			if !ok || condition.Name != "supervisionComplete" {
				t.Fatal("deadline poll depends on wait readiness")
			}
			for _, statement := range guard.Body.List {
				expression, ok := statement.(*ast.ExprStmt)
				if !ok {
					continue
				}
				call, ok := expression.X.(*ast.CallExpr)
				if !ok {
					continue
				}
				name, ok := call.Fun.(*ast.Ident)
				if ok && name.Name == "expireRemoteFills" {
					return
				}
			}
			t.Fatal("remote deadlines omitted before interest snapshot")
		}
	}
	t.Fatal("init event loop missing")
}

func TestGraceRejectsInvalidDeadlineAndOwner(t *testing.T) {
	for _, test := range []struct{ session, now, timeout uint64 }{{0, 1, 1}, {1, 1, 0}, {1, ^uint64(0) - 1, 2}} {
		var grace RemoteGrace
		if grace.Begin(test.session, 0, test.now, test.timeout) || grace != (RemoteGrace{}) {
			t.Fatal("invalid grace accepted", test)
		}
	}
}
