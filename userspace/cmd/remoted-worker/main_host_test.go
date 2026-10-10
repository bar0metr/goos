//go:build !goos_userspace

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestEntryPointDoesNotShadowSessionServer(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "userspaceMain" {
			continue
		}
		found = true
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || assignment.Tok != token.DEFINE {
				return true
			}
			for _, expression := range assignment.Lhs {
				if name, ok := expression.(*ast.Ident); ok && name.Name == "server" {
					t.Error("entry point shadows server used by output and teardown")
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("native entry point missing")
	}
}
