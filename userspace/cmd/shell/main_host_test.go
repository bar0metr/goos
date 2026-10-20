//go:build !goos_userspace

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestBuiltinOutputFlushPrecedesExternalLaunch(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "pipeline.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var flushed, submitted token.Pos
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "executeExternal" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if name.Name == "flushOutput" {
				flushed = call.Pos()
			}
			if name.Name == "sendExec" {
				submitted = call.Pos()
			}
			return true
		})
	}
	if flushed == 0 || submitted == 0 || flushed >= submitted {
		t.Fatal("buffered builtin stdout can follow completed external stdout")
	}
}

func TestVFSUsesSharedBoundedTransaction(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "session.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	shared := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if selector.Sel.Name == "IPCWaitReceive" {
			t.Fatal("shell VFS owns an unbounded receive loop again")
		}
		if owner, ok := selector.X.(*ast.Ident); ok && owner.Name == "filesystemRequests" && selector.Sel.Name == "Call" && len(call.Args) == 5 {
			shared = true
		}
		return true
	})
	if !shared {
		t.Fatal("shell VFS bypasses shared transaction ownership")
	}
}
