//go:build !goos_userspace

package main

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"testing"

	shellcmd "goos/userspace/shell"
)

func TestBootReadyToken(t *testing.T) {
	for _, token := range []string{"1", "42", "18446744073709551615"} {
		var storage shellcmd.Storage
		invocation, valid := shellcmd.ParseInvocation([]byte("__boot_ready "+token), &storage)
		if !valid {
			t.Fatal("invalid fixture invocation")
		}
		got, ready := bootReadyToken(invocation)
		if !ready || string(got) != token {
			t.Fatalf("token %q: got %q, %v", token, got, ready)
		}
	}
	for _, command := range []string{
		"__boot_ready", "__boot_ready 0", "__boot_ready 01",
		"__boot_ready -1", "__boot_ready +1", "__boot_ready 1x",
		"__boot_ready 18446744073709551616", "__boot_ready 123456789012345678901",
		"__boot_ready 1 2", "echo 1",
	} {
		var storage shellcmd.Storage
		invocation, valid := shellcmd.ParseInvocation([]byte(command), &storage)
		if !valid {
			t.Fatalf("invalid fixture invocation %q", command)
		}
		if _, ready := bootReadyToken(invocation); ready {
			t.Fatalf("accepted %q", command)
		}
	}
}

func TestBootReadyDispatchIsValidationOnlyAndNotAnExternalJob(t *testing.T) {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, "execute.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var acknowledged, external token.Pos
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "executeCommand" {
			continue
		}
		for _, statement := range function.Body.List {
			branch, ok := statement.(*ast.IfStmt)
			if !ok {
				continue
			}
			var condition bytes.Buffer
			if err := format.Node(&condition, set, branch.Cond); err != nil {
				t.Fatal(err)
			}
			ast.Inspect(branch.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch name.Name {
				case "bootReadyToken":
					if condition.String() != "validation.Enabled && !oneShot && count == 1" {
						t.Fatal("acknowledgement escaped the interactive validation-only gate")
					}
					acknowledged = call.Pos()
				case "executeExternal":
					external = call.Pos()
				}
				return true
			})
		}
	}
	if acknowledged == 0 || external == 0 || acknowledged >= external {
		t.Fatal("acknowledgement must dispatch in the shell before external jobs")
	}
}
