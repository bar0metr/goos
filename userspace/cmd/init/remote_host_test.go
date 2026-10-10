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
