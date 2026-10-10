package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestSignerPeerDeathClosesItsWorker(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "serviceSigners" {
			continue
		}
		for _, statement := range function.Body.List {
			loop, ok := statement.(*ast.RangeStmt)
			if !ok {
				continue
			}
			for _, statement := range loop.Body.List {
				branch, ok := statement.(*ast.IfStmt)
				if !ok || !isSignerPeerDeath(branch.Cond) {
					continue
				}
				body := branch.Body.List
				if len(body) != 3 || !workerCallIs(body[0], "revokeSigner") || !workerCallIs(body[1], "closeWorker") {
					t.Fatal("signer peer death must revoke signing and close the same worker")
				}
				end, ok := body[2].(*ast.BranchStmt)
				if !ok || end.Tok != token.CONTINUE {
					t.Fatal("signer peer death must stop processing the closed worker")
				}
				return
			}
		}
		t.Fatal("serviceSigners has no peer-death branch")
	}
	t.Fatal("serviceSigners not found")
}

func isSignerPeerDeath(expression ast.Expr) bool {
	comparison, ok := expression.(*ast.BinaryExpr)
	if !ok || comparison.Op != token.EQL {
		return false
	}
	status, ok := comparison.X.(*ast.Ident)
	if !ok || status.Name != "status" {
		return false
	}
	value, ok := comparison.Y.(*ast.SelectorExpr)
	if !ok || value.Sel.Name != "StatusPeerDied" {
		return false
	}
	packageName, ok := value.X.(*ast.Ident)
	return ok && packageName.Name == "kcall"
}

func workerCallIs(statement ast.Stmt, name string) bool {
	expression, ok := statement.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expression.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	function, ok := call.Fun.(*ast.Ident)
	if !ok || function.Name != name {
		return false
	}
	worker, ok := call.Args[0].(*ast.Ident)
	return ok && worker.Name == "w"
}
