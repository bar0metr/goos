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

func TestWorkerHasOnlyPublicHostIdentity(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	signerGrant, callback, public := false, false, false
	ast.Inspect(file, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			switch selector.Sel.Name {
			case "NewKeyFromSeed", "GenerateKey", "PrivateKey", "Seed":
				t.Error("worker constructs or accesses private host key", selector.Sel.Name)
			}
		}
		if kv, ok := node.(*ast.KeyValueExpr); ok {
			if name, ok := kv.Key.(*ast.Ident); ok {
				if name.Name == "HostPublicKey" {
					public = true
				}
				if name.Name == "SignHost" {
					callback = true
				}
				if name.Name == "HostKey" {
					t.Error("private host key supplied to SSH")
				}
			}
		}
		if call, ok := node.(*ast.CallExpr); ok && len(call.Args) == 1 {
			if method, ok := call.Fun.(*ast.SelectorExpr); ok && method.Sel.Name == "Handle" {
				if text, ok := call.Args[0].(*ast.BasicLit); ok && text.Value == "\"signer\"" {
					signerGrant = true
				}
			}
		}
		return true
	})
	if !signerGrant || !callback || !public {
		t.Fatal("public signing capability contract missing")
	}
}
