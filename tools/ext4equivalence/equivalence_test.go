// The gate runs both production service loops under the host Go runtime.
// Host adapters replace capability IPC, readiness waits and wake streams; an
// in-memory block device backs the production ext4 volume. Native startup,
// syscall execution and the native goroutine scheduler need their own gates.
package ext4equivalence

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildService compiles the production loop with host IPC and wait adapters.
// It retains every declaration and statement in the selected loop source.
func buildService(t *testing.T, root, variant, out string) string {
	t.Helper()
	dir := filepath.Join(out, variant)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	rewrite := func(path string, keep map[string]bool) []byte {
		t.Helper()
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if keep != nil {
			var decls []ast.Decl
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok && keep[fn.Name.Name] {
					decls = append(decls, d)
				}
			}
			f.Decls = decls
			f.Imports = nil
		}
		for _, i := range f.Imports {
			switch i.Path.Value {
			case `"goos/userspace/runtime/sys"`, `"goos/userspace/wait"`, `"goos/userspace/log"`:
				i.Path.Value = `"goos/tools/ext4equivalence/host"`
				if i.Name == nil {
					i.Name = ast.NewIdent("sys")
				}
			}
		}
		var b bytes.Buffer
		if err := format.Node(&b, fset, f); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	source := filepath.Join(root, "userspace/cmd/ext4")
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("loop.go", rewrite(filepath.Join(source, "serve_"+variant+".go"), nil))
	write("readjob.go", rewrite(filepath.Join(source, "readjob.go"), nil))
	funcs := rewrite(filepath.Join(source, "main.go"), map[string]bool{"answer": true, "sendReply": true})
	// The extracted functions use the same package state as the native service.
	funcs = bytes.Replace(funcs, []byte("package main"), []byte("package main\nimport (\"goos/kernel/ramfs\";\"goos/kernel/ipc\";\"goos/kernel/kcall\";\"goos/userspace/fsproto\";sys \"goos/tools/ext4equivalence/host\";syslog \"goos/tools/ext4equivalence/host\")"), 1)
	write("answer.go", funcs)
	write("wire.go", rewrite(filepath.Join(source, "blocktransport.go"), nil))
	runner, err := os.ReadFile(filepath.Join(root, "tools/ext4equivalence/testdata/service.go"))
	if err != nil {
		t.Fatal(err)
	}
	write("main.go", runner)
	binary := filepath.Join(dir, "service")
	args := []string{"build", "-race", "-o", binary, filepath.Join(dir, "loop.go"), filepath.Join(dir, "readjob.go"), filepath.Join(dir, "answer.go"), filepath.Join(dir, "wire.go"), filepath.Join(dir, "main.go")}
	if variant == "goroutines" {
		write("restart_gate.go", rewrite(filepath.Join(source, "restart_gate_off.go"), nil))
		args = append(args, filepath.Join(dir, "restart_gate.go"))
	}
	command := exec.Command("go", args...)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile %s: %v\n%s", variant, err, output)
	}
	return binary
}

func compare(serial, concurrent []byte) error {
	if !bytes.Equal(serial, concurrent) {
		return &difference{serial, concurrent}
	}
	return nil
}

type difference struct{ serial, concurrent []byte }

func (d *difference) Error() string {
	return "serial/concurrent logical replies differ\nserial:\n" + string(d.serial) + "\nconcurrent:\n" + string(d.concurrent)
}

func TestServiceLogicalEquivalence(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "build/tmp"), 0755); err != nil {
		t.Fatal(err)
	}
	out, err := os.MkdirTemp(filepath.Join(root, "build/tmp"), "ext4-equivalence-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(out) })
	var transcripts [][]byte
	for _, variant := range []string{"serial", "goroutines"} {
		binary := buildService(t, root, variant, out)
		command := exec.Command(binary, filepath.Join(root, "userspace/ext4/testdata/canonical.img.gz"), variant)
		command.Dir = root
		data, err := command.Output()
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				t.Fatalf("%s: %v\n%s", variant, err, e.Stderr)
			}
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"Label":"unwritten"`) {
			t.Fatal("missing unwritten read")
		}
		transcripts = append(transcripts, data)
	}
	if err := compare(transcripts[0], transcripts[1]); err != nil {
		t.Fatal(err)
	}
	t.Logf("serial and goroutine production service builds agree on %d logical replies", bytes.Count(transcripts[0], []byte{'\n'}))
}

func TestComparatorRejectsLogicalMutation(t *testing.T) {
	baseline := []byte(`{"Status":0,"Node":12,"Kind":1,"Value":11,"Modified":{"Seconds":1767225600},"Unix":{"Links":2,"UID":0},"Filesystem":{"FreeBytes":4096,"ReadOnly":false},"Data":"hello"}`)
	for _, replacement := range [][2]string{{`"Status":0`, `"Status":1`}, {`"Node":12`, `"Node":13`}, {`"Kind":1`, `"Kind":2`}, {`"Value":11`, `"Value":10`}, {`"Links":2`, `"Links":1`}, {`"UID":0`, `"UID":1`}, {`"Seconds":1767225600`, `"Seconds":1767225601`}, {`"FreeBytes":4096`, `"FreeBytes":0`}, {`"ReadOnly":false`, `"ReadOnly":true`}, {`"hello"`, `"wrong"`}} {
		changed := bytes.Replace(baseline, []byte(replacement[0]), []byte(replacement[1]), 1)
		if compare(baseline, changed) == nil {
			t.Fatalf("accepted mutation %s", replacement[0])
		}
	}
	if compare(baseline, baseline) != nil {
		t.Fatal("identical transcript differs")
	}
}
