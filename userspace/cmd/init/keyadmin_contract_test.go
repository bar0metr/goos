//go:build !goos_userspace

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func contractFunction(t *testing.T, path, name string) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatal("missing function", path, name)
	return nil
}
func TestKeyCandidateReadsUseScopedReaderNotControllerUnion(t *testing.T) {
	fn := contractFunction(t, "../remoted/main.go", "reloadCandidate")
	reads := 0
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Read" {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok || receiver.Name != "keyReader" {
			t.Error("candidate read bypassed scoped reader")
		}
		reads++
		return true
	})
	if reads != 2 {
		t.Fatal("candidate capture/precommit ownership checks", reads)
	}
}
func TestKeyReloadUsesRetainedIdentityWithoutHostFileAccess(t *testing.T) {
	fn := contractFunction(t, "../remoted/keyadmin.go", "keyReloadHost")
	retained := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok {
			t.Error("unexpected retained identity call")
			return true
		}
		switch receiver.Name + "." + selector.Sel.Name {
		case "state.RetainedHost":
			retained = true
		case "identities.Current", "errors.New":
		default:
			t.Error("key reload accessed a non-retained resource", receiver.Name, selector.Sel.Name)
		}
		return true
	})
	if !retained {
		t.Fatal("retained public identity not used")
	}
}
func TestKeyControlTimeoutCancellationAndLateReplyReconciliation(t *testing.T) {
	calls := func(fn *ast.FuncDecl, name string) bool {
		found := false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
					found = true
				}
			}
			return true
		})
		return found
	}
	rpc := contractFunction(t, "../remoted/keyadmin.go", "keyControlCall")
	if !calls(rpc, "abandonKeyCall") || !calls(rpc, "queueKeyEnd") || !calls(rpc, "reconcileKeyReply") {
		t.Fatal("uncertain Begin/End not reconciled")
	}
	notice := contractFunction(t, "../remoted/main.go", "notice")
	worker := contractFunction(t, "../remoted/main.go", "workerCall")
	if !calls(notice, "reconcileKeyReply") || !calls(worker, "reconcileKeyReply") {
		t.Fatal("late lease replies lost in shared control receiver")
	}
	begin := contractFunction(t, "keyadmin.go", "remoteKeyControl")
	knownToken := false
	ast.Inspect(begin.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "begin" {
			return true
		}
		argument, ok := call.Args[0].(*ast.SelectorExpr)
		if ok && argument.Sel.Name == "Correlation" {
			knownToken = true
		}
		return true
	})
	if !knownToken {
		t.Fatal("Begin token unavailable until reply")
	}
}
