// Command commands builds the command index of the system image from the
// manifests the executables under a directory declare in their source.
//
//	commands SOURCE_DIR OUTPUT
//
// Every subdirectory of SOURCE_DIR is one command, named after the directory.
// It must declare exactly one package-level
//
//	var manifest = commands.Manifest{...}
//
// whose fields are literals, commands.Unbounded or commands grants joined by
// |. Reading the declaration from the syntax tree keeps the source the only
// place a command is described, without compiling it for the host.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"goos/userspace/commands"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: commands SOURCE_DIR OUTPUT")
		os.Exit(2)
	}
	index, err := build(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "commands:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(os.Args[2], index, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "commands:", err)
		os.Exit(1)
	}
}

// build returns the index for every command under root, in name order so the
// image is reproducible.
func build(root string) ([]byte, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("%s: no commands", root)
	}
	var index []byte
	for _, name := range names {
		manifest, err := readManifest(filepath.Join(root, name))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if manifest.Usage != name && !strings.HasPrefix(manifest.Usage, name+" ") {
			return nil, fmt.Errorf("%s: usage %q does not start with the command name", name, manifest.Usage)
		}
		var ok bool
		if index, ok = commands.Append(index, name, manifest); !ok {
			return nil, fmt.Errorf("%s: manifest out of bounds or not printable ASCII", name)
		}
	}
	if len(index) > commands.MaxIndex {
		return nil, fmt.Errorf("index is %d bytes, over the %d a reader holds", len(index), commands.MaxIndex)
	}
	return index, nil
}

func readManifest(directory string) (commands.Manifest, error) {
	files, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		return commands.Manifest{}, err
	}
	var found []*ast.CompositeLit
	fileSet := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		syntax, err := parser.ParseFile(fileSet, file, nil, parser.SkipObjectResolution)
		if err != nil {
			return commands.Manifest{}, err
		}
		for _, declaration := range syntax.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, spec := range general.Specs {
				value := spec.(*ast.ValueSpec)
				for index, name := range value.Names {
					if name.Name != "manifest" || index >= len(value.Values) {
						continue
					}
					literal, ok := value.Values[index].(*ast.CompositeLit)
					if !ok || !isSelector(literal.Type, "commands", "Manifest") {
						return commands.Manifest{}, fmt.Errorf("manifest is not a commands.Manifest literal")
					}
					found = append(found, literal)
				}
			}
		}
	}
	if len(found) != 1 {
		return commands.Manifest{}, fmt.Errorf("want one var manifest = commands.Manifest{...}, found %d", len(found))
	}
	return decode(found[0])
}

func decode(literal *ast.CompositeLit) (commands.Manifest, error) {
	var manifest commands.Manifest
	seen := map[string]bool{}
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return manifest, fmt.Errorf("manifest fields must be named")
		}
		key, ok := field.Key.(*ast.Ident)
		if !ok || seen[key.Name] {
			return manifest, fmt.Errorf("manifest field repeated or unnamed")
		}
		seen[key.Name] = true
		var err error
		switch key.Name {
		case "Usage":
			manifest.Usage, err = stringValue(field.Value)
		case "Summary":
			manifest.Summary, err = stringValue(field.Value)
		case "Options":
			manifest.Options, err = stringValue(field.Value)
		case "MinArgs":
			manifest.MinArgs, err = intValue(field.Value)
		case "MaxArgs":
			manifest.MaxArgs, err = intValue(field.Value)
		case "Grants":
			manifest.Grants, err = grantValue(field.Value)
		default:
			err = fmt.Errorf("unknown manifest field %s", key.Name)
		}
		if err != nil {
			return manifest, fmt.Errorf("%s: %w", key.Name, err)
		}
	}
	return manifest, nil
}

func stringValue(expression ast.Expr) (string, error) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", fmt.Errorf("want a string literal")
	}
	return strconv.Unquote(literal.Value)
}

func intValue(expression ast.Expr) (int, error) {
	if isSelector(expression, "commands", "Unbounded") {
		return commands.Unbounded, nil
	}
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.INT {
		return 0, fmt.Errorf("want an integer literal or commands.Unbounded")
	}
	return strconv.Atoi(literal.Value)
}

func grantValue(expression ast.Expr) (commands.Grant, error) {
	if binary, ok := expression.(*ast.BinaryExpr); ok && binary.Op == token.OR {
		left, err := grantValue(binary.X)
		if err != nil {
			return 0, err
		}
		right, err := grantValue(binary.Y)
		return left | right, err
	}
	switch {
	case isSelector(expression, "commands", "GrantVFS"):
		return commands.GrantVFS, nil
	case isSelector(expression, "commands", "GrantBlockCheck"):
		return commands.GrantBlockCheck, nil
	case isSelector(expression, "commands", "GrantTerminal"):
		return commands.GrantTerminal, nil
	case isSelector(expression, "commands", "GrantLaunch"):
		return commands.GrantLaunch, nil
	case isSelector(expression, "commands", "GrantNetworkPing"):
		return commands.GrantNetworkPing, nil
	case isSelector(expression, "commands", "GrantNetworkAdmin"):
		return commands.GrantNetworkAdmin, nil
	case isSelector(expression, "commands", "GrantResolve"):
		return commands.GrantResolve, nil
	case isSelector(expression, "commands", "GrantResolveAdmin"):
		return commands.GrantResolveAdmin, nil
	case isSelector(expression, "commands", "GrantUDP"):
		return commands.GrantUDP, nil
	case isSelector(expression, "commands", "GrantTCP"):
		return commands.GrantTCP, nil
	default:
		return 0, fmt.Errorf("want a commands grant")
	}
}

func isSelector(expression ast.Expr, pkg, name string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && identifier.Name == pkg
}
