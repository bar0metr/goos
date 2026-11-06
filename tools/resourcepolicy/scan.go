package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Bound is one package-level Max* constant or variable.
type Bound struct {
	Package string // directory relative to the root, slash-separated
	Name    string
	Value   string // source text of the initializer, empty when implicit
	File    string
	Line    int
}

// Key is the inventory spelling: package directory, dot, name.
func (b Bound) Key() string { return b.Package + "." + b.Name }

// scanRoots are the release-visible trees.
var scanRoots = []string{"kernel", "userspace"}

// scan parses every non-test Go file under the scan roots, whatever its build
// tags, and returns each package-level const or var whose name starts with
// "Max", sorted by key.
func scan(root string) ([]Bound, error) {
	seen := map[string]bool{}
	var out []Bound
	fset := token.NewFileSet()
	for _, top := range scanRoots {
		dir := filepath.Join(root, top)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(filepath.Dir(rel))
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
					continue
				}
				for _, spec := range gen.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if !isBound(name.Name) {
							continue
						}
						b := Bound{Package: pkg, Name: name.Name, File: filepath.ToSlash(rel), Line: fset.Position(name.Pos()).Line}
						if i < len(vs.Values) {
							v := vs.Values[i]
							b.Value = string(src[fset.Position(v.Pos()).Offset:fset.Position(v.End()).Offset])
						}
						if seen[b.Key()] {
							continue
						}
						seen[b.Key()] = true
						out = append(out, b)
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

// isBound reports whether name is an exported Max* identifier.
func isBound(name string) bool {
	return len(name) > 3 && strings.HasPrefix(name, "Max")
}
