//go:build !goos_userspace

package main

import (
	"go/ast"
	"reflect"
	"strconv"
	"testing"
)

func TestApplicationStartupHandlesMatchGrantOrder(t *testing.T) {
	fn := contractFunction(t, "applications.go", "launchExecutable")
	var roles, grants []string
	name := func(expression ast.Expr) string {
		switch value := expression.(type) {
		case *ast.BasicLit:
			text, err := strconv.Unquote(value.Value)
			if err != nil {
				t.Fatal(err)
			}
			return text
		case *ast.SelectorExpr:
			return value.X.(*ast.Ident).Name + "." + value.Sel.Name
		default:
			t.Fatalf("unsupported handle name %T", expression)
			return ""
		}
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.CallExpr:
			if selector, ok := value.Fun.(*ast.SelectorExpr); ok {
				if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == "writer" && selector.Sel.Name == "Handle" {
					conversion := value.Args[0].(*ast.CallExpr)
					roles = append(roles, name(conversion.Args[0]))
				}
			}
			if function, ok := value.Fun.(*ast.Ident); ok && function.Name == "streamGrant" {
				grants = append(grants, name(value.Args[0]))
			}
		case *ast.CompositeLit:
			selector, ok := value.Type.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Capability" {
				break
			}
			for _, element := range value.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if ok && field.Key.(*ast.Ident).Name == "Name" {
					grants = append(grants, name(field.Value))
				}
			}
		}
		return true
	})
	if len(roles) < 5 || !reflect.DeepEqual(roles, grants) {
		t.Fatalf("startup roles %v do not match positional grants %v", roles, grants)
	}
}
