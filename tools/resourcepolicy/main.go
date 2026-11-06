// resourcepolicy checks that every Max* bound in the kernel and userspace has
// an entry in the resource-policy inventory
// (docs/architecture/resource-policy.md, ADR 0215).
//
//	resourcepolicy [-root DIR] [-doc FILE] [-list]
//
// It parses every non-test Go file under DIR/kernel and DIR/userspace,
// whatever its build tags, and collects each package-level const or var whose
// name starts with "Max". With -list it prints them as
// "package.Name<TAB>value<TAB>file:line" and exits. Without it, it reads the
// inventory document and exits 1 when a bound has no entry or an exact entry
// names a bound that no longer exists.
//
// An entry is a backquoted key in a table row: `kernel/ipc.MaxHandles`. A key
// ending in "Max*" covers every bound of that package, for packages whose
// bounds are all wire-format field widths classified together.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	root := flag.String("root", ".", "repository root")
	doc := flag.String("doc", "docs/architecture/resource-policy.md", "inventory document, relative to the root")
	list := flag.Bool("list", false, "print the Max* bounds and exit")
	flag.Parse()
	os.Exit(run(*root, *doc, *list, os.Stdout, os.Stderr))
}

func run(root, docPath string, list bool, stdout, stderr io.Writer) int {
	bounds, err := scan(root)
	if err != nil {
		fmt.Fprintln(stderr, "resourcepolicy:", err)
		return 2
	}
	if list {
		for _, b := range bounds {
			fmt.Fprintf(stdout, "%s\t%s\t%s:%d\n", b.Key(), b.Value, b.File, b.Line)
		}
		return 0
	}
	if !filepath.IsAbs(docPath) {
		docPath = filepath.Join(root, docPath)
	}
	text, err := os.ReadFile(docPath)
	if err != nil {
		fmt.Fprintln(stderr, "resourcepolicy:", err)
		return 2
	}
	problems := compare(bounds, parseInventory(string(text)))
	for _, p := range problems {
		fmt.Fprintln(stderr, p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(stderr, "resourcepolicy: %d problem(s); classify each bound in docs/architecture/resource-policy.md\n", len(problems))
		return 1
	}
	fmt.Fprintf(stdout, "resourcepolicy: %d Max* bounds, all classified\n", len(bounds))
	return 0
}
