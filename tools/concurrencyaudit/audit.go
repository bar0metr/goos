package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Var is one package-level variable. A name declared in several files of a
// package, under different build tags, is one Var at its first position.
type Var struct {
	Package string // import path, for example goos/userspace/utility
	Name    string
	File    string // slash-separated, relative to the root
	Line    int
}

// Row is one line of the audit table.
type Row struct {
	ID       string
	Package  string
	Name     string
	Owner    string
	Contract string
	Test     string
	Line     int
}

// columns is the header of the audit table.
var columns = []string{"id", "package", "symbol", "owner", "contract", "test"}

var (
	idPattern = regexp.MustCompile(`^CA-[0-9]{4}$`)
	owners    = map[string]bool{"process": true, "thread": true, "goroutine": true, "session": true, "call": true}
	contracts = map[string]bool{
		"immutable":  true, // written before a second thread or goroutine exists, then only read
		"atomic":     true, // sync/atomic words
		"lock":       true, // a lock.Mutex or the owning package's own lock
		"owned":      true, // one owner at a time, documented on the API
		"per-thread": true, // per-M scratch owned by runtime/sys
		"runtime":    true, // runtime and collector state with its own protocol
	}
)

// scan returns every package-level variable in non-test Go files under
// root/userspace, sorted by package and name.
func scan(root string) ([]Var, error) {
	module, err := modulePath(root)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var vars []Var
	fset := token.NewFileSet()
	base := filepath.Join(root, "userspace")
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != base && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		pkg := module + "/" + filepath.ToSlash(filepath.Dir(rel))
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					if name.Name == "_" {
						continue
					}
					key := pkg + "\t" + name.Name
					if seen[key] {
						continue
					}
					seen[key] = true
					vars = append(vars, Var{Package: pkg, Name: name.Name, File: rel, Line: fset.Position(name.Pos()).Line})
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(vars, func(i, j int) bool {
		if vars[i].Package != vars[j].Package {
			return vars[i].Package < vars[j].Package
		}
		return vars[i].Name < vars[j].Name
	})
	return vars, nil
}

func modulePath(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("%s/go.mod: no module line", root)
}

// parseAudit reads the tab-separated audit table. Blank lines and lines
// starting with # are skipped; the first other line is the header.
func parseAudit(r io.Reader) ([]Row, []string) {
	var rows []Row
	var problems []string
	ids := map[string]int{}
	keys := map[string]int{}
	scanner := bufio.NewScanner(r)
	header := false
	for n := 1; scanner.Scan(); n++ {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if !header {
			header = true
			if strings.Join(fields, "\t") != strings.Join(columns, "\t") {
				problems = append(problems, fmt.Sprintf("audit:%d: header must be %q", n, strings.Join(columns, "\t")))
			}
			continue
		}
		if len(fields) != len(columns) {
			problems = append(problems, fmt.Sprintf("audit:%d: %d fields, want %d", n, len(fields), len(columns)))
			continue
		}
		row := Row{ID: fields[0], Package: fields[1], Name: fields[2], Owner: fields[3], Contract: fields[4], Test: fields[5], Line: n}
		if !idPattern.MatchString(row.ID) {
			problems = append(problems, fmt.Sprintf("audit:%d: id %q is not CA-NNNN", n, row.ID))
		}
		if prev, ok := ids[row.ID]; ok {
			problems = append(problems, fmt.Sprintf("audit:%d: id %s already used on line %d", n, row.ID, prev))
		}
		ids[row.ID] = n
		key := row.Package + "\t" + row.Name
		if prev, ok := keys[key]; ok {
			problems = append(problems, fmt.Sprintf("audit:%d: %s.%s already audited on line %d", n, row.Package, row.Name, prev))
		}
		keys[key] = n
		if !owners[row.Owner] {
			problems = append(problems, fmt.Sprintf("audit:%d: %s: owner %q is not one of process, thread, goroutine, session, call", n, row.ID, row.Owner))
		}
		if !contracts[row.Contract] {
			problems = append(problems, fmt.Sprintf("audit:%d: %s: contract %q is not one of immutable, atomic, lock, owned, per-thread, runtime", n, row.ID, row.Contract))
		}
		if strings.TrimSpace(row.Test) == "" {
			problems = append(problems, fmt.Sprintf("audit:%d: %s: empty test column; name a test or write none", n, row.ID))
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		problems = append(problems, "audit: "+err.Error())
	}
	if !header {
		problems = append(problems, "audit: empty table")
	}
	return rows, problems
}

// compare reports variables without a row and rows without a variable.
func compare(vars []Var, rows []Row) []string {
	var problems []string
	audited := map[string]bool{}
	for _, row := range rows {
		audited[row.Package+"\t"+row.Name] = true
	}
	present := map[string]bool{}
	for _, v := range vars {
		key := v.Package + "\t" + v.Name
		present[key] = true
		if !audited[key] {
			problems = append(problems, fmt.Sprintf("%s:%d: %s.%s has no audit row", v.File, v.Line, v.Package, v.Name))
		}
	}
	for _, row := range rows {
		if !present[row.Package+"\t"+row.Name] {
			problems = append(problems, fmt.Sprintf("audit:%d: %s names %s.%s, which no longer exists", row.Line, row.ID, row.Package, row.Name))
		}
	}
	return problems
}
