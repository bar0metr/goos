//go:build !goos_userspace

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The validation stall must stay alive after VFS closes its endpoint. The
// service has no channel-create quota, so an allocated wait object makes the
// fixture exit instead of exercising PID 1's hung-writer replacement.
func TestValidationStallUsesGrantedIdleEndpoint(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stall *ast.FuncDecl
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "stall" {
			stall = function
		}
	}
	if stall == nil {
		t.Fatal("missing validation stall")
	}
	block, readable, waiting := false, false, false
	ast.Inspect(stall.Body, func(node ast.Node) bool {
		if field, ok := node.(*ast.KeyValueExpr); ok {
			if key, ok := field.Key.(*ast.Ident); ok && key.Name == "Handle" {
				id, ok := field.Value.(*ast.Ident)
				if !ok || id.Name != "blockHandle" {
					t.Error("stall waits on something other than its idle block endpoint")
				} else {
					block = true
				}
			}
			if key, ok := field.Key.(*ast.Ident); ok && key.Name == "Events" {
				selector, ok := field.Value.(*ast.SelectorExpr)
				readable = ok && selector.Sel.Name == "WaitReadable"
			}
		}
		if call, ok := node.(*ast.CallExpr); ok {
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
				if owner, ok := selector.X.(*ast.Ident); ok {
					switch owner.Name {
					case "sys":
						t.Errorf("stall calls sys.%s instead of reusing its granted endpoint", selector.Sel.Name)
					case "userwait":
						waiting = selector.Sel.Name == "Any" && len(call.Args) == 3
					}
				}
			}
		}
		return true
	})
	if !block || !readable || !waiting {
		t.Fatal("stall must wait for readability on its granted idle block endpoint")
	}
}
