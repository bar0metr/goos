//go:build !goos_userspace

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

func TestRemoteShellGrantsMatchStartupContract(t *testing.T) {
	parse := func(path string) *ast.File {
		t.Helper()
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	literal := func(expression ast.Expr) string {
		value, ok := expression.(*ast.BasicLit)
		if !ok || value.Kind != token.STRING {
			return ""
		}
		text, _ := strconv.Unquote(value.Value)
		return text
	}
	required := make(map[string]bool)
	for _, declaration := range parse("../shell/main.go").Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "userspaceMain" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || len(assignment.Lhs) != 2 || len(assignment.Rhs) != 1 {
				return true
			}
			if name, ok := assignment.Lhs[1].(*ast.Ident); !ok || name.Name == "_" {
				return true
			}
			call, ok := assignment.Rhs[0].(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			method, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || method.Sel.Name != "Handle" {
				return true
			}
			if receiver, ok := method.X.(*ast.Ident); ok && receiver.Name == "startupData" {
				required[literal(call.Args[0])] = true
			}
			return true
		})
	}
	granted := make(map[string]bool)
	for _, declaration := range parse("remote.go").Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "launchRemoteShell" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if field, ok := node.(*ast.KeyValueExpr); ok {
				if name, ok := field.Key.(*ast.Ident); ok && name.Name == "Name" {
					granted[literal(field.Value)] = true
				}
			}
			return true
		})
	}
	if len(required) == 0 || len(granted) == 0 {
		t.Fatal("startup or launch contract missing")
	}
	for name := range required {
		if !granted[name] {
			t.Errorf("remote shell lacks required startup grant %q", name)
		}
	}
}

func nativeFunction(t *testing.T, path, name string) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == name {
			return function
		}
	}
	t.Fatalf("missing %s in %s", name, path)
	return nil
}

func TestRemoteExecRejectClosesHandlesBeforeSnapshotMutation(t *testing.T) {
	function := nativeFunction(t, "remote.go", "pollRemoteWorker")
	cleanup := false
	var check, snapshot token.Pos
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if deferred, ok := node.(*ast.DeferStmt); ok {
			if name, ok := deferred.Call.Fun.(*ast.Ident); ok && name.Name == "closeMessageHandles" {
				cleanup = true
			}
		}
		if call, ok := node.(*ast.CallExpr); ok {
			if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "copyRemoteCommand" {
				snapshot = call.Pos()
			}
			if method, ok := call.Fun.(*ast.SelectorExpr); ok && method.Sel.Name == "CheckOpen" {
				check = call.Pos()
			}
		}
		return true
	})
	if !cleanup {
		t.Fatal("rejected request can retain transferred command handle")
	}
	if check == 0 || snapshot == 0 || check >= snapshot {
		t.Fatal("duplicate Open can overwrite pending command")
	}
}

func TestLauncherPreservesSessionOwnedExecInput(t *testing.T) {
	application := nativeFunction(t, "applications.go", "launchExecutable")
	captured, installed := false, false
	ast.Inspect(application.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			return true
		}
		if name, ok := assignment.Lhs[0].(*ast.Ident); ok && name.Name == "inputBorrowed" {
			if shared, ok := assignment.Rhs[0].(*ast.SelectorExpr); ok && shared.Sel.Name == "Shared" {
				captured = true
			}
		}
		if field, ok := assignment.Lhs[0].(*ast.SelectorExpr); ok && field.Sel.Name == "inputBorrowed" {
			if name, ok := assignment.Rhs[0].(*ast.Ident); ok && name.Name == "inputBorrowed" {
				installed = true
			}
		}
		return true
	})
	guarded := false
	ast.Inspect(nativeFunction(t, "launcher.go", "closeLauncher").Body, func(node ast.Node) bool {
		if expression, ok := node.(*ast.UnaryExpr); ok && expression.Op == token.NOT {
			if field, ok := expression.X.(*ast.SelectorExpr); ok && field.Sel.Name == "inputBorrowed" {
				guarded = true
			}
		}
		return true
	})
	if !captured || !installed || !guarded {
		t.Fatal("launcher may close session-owned exec stdin")
	}
}
