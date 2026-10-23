package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildAgainstInstalledToolchain checks the pins and patches against the
// TinyGo the build uses, when it is installed.
func TestBuildAgainstInstalledToolchain(t *testing.T) {
	root := os.Getenv("TINYGOROOT")
	if root == "" {
		root = "/opt/tinygo"
	}
	if _, err := os.Stat(filepath.Join(root, "src", "runtime", "scheduler_cores.go")); err != nil {
		t.Skip("pinned TinyGo is not installed")
	}
	files, sources, err := build(root, filepath.Join("..", "..", "tinygo", "goroutines"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 12 {
		t.Fatalf("%d sources pinned", len(sources))
	}
	if string(files["go.mod"]) != "module goos.local/goroutine-toolchain\n" {
		t.Fatal("generated toolchain has no host module boundary")
	}
	runtimeGo := string(files["src/runtime/runtime.go"])
	if !strings.Contains(runtimeGo, "return goosMaxProcs(n)") || !strings.Contains(runtimeGo, "LockOSThread is not supported") {
		t.Fatal("runtime.go was not patched")
	}
	if !strings.Contains(string(files["src/internal/task/task_stack.go"]), "goos_g_start") {
		t.Fatal("task_stack.go was not replaced")
	}
	channelGo := string(files["src/runtime/chan.go"])
	for _, cleanup := range []string{
		"if state.ch.closed {\n\t\t\t\tunlockAllStates(states)\n\t\t\t\tchanSelectLock.Unlock()\n\t\t\t\tinterrupt.Restore(mask)",
		"if !selectOk && states[selectIndex].value != nil {\n\t\truntimePanic(\"send on closed channel\")",
	} {
		if !strings.Contains(channelGo, cleanup) {
			t.Fatal("select closed-send panic cleanup missing")
		}
	}
}

func TestBuildRejectsChangedSource(t *testing.T) {
	source := t.TempDir()
	for _, r := range replacements {
		path := filepath.Join(source, filepath.FromSlash(r.path))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("changed"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := build(source, filepath.Join("..", "..", "tinygo", "goroutines")); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("changed source accepted: %v", err)
	}
}

func TestMirrorMakesDirectoriesRealAndLinksFiles(t *testing.T) {
	source := t.TempDir()
	for _, path := range []string{"src/runtime/a.go", "src/runtime/b.go", "src/os/c.go", "bin/tinygo"} {
		full := filepath.Join(source, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(path), 0644); err != nil {
			t.Fatal(err)
		}
	}
	output := t.TempDir()
	if err := mirror(source, output, "", map[string][]byte{"src/runtime/a.go": nil}); err != nil {
		t.Fatal(err)
	}
	for path, link := range map[string]bool{"src": false, "src/runtime": false, "src/runtime/b.go": true, "src/os": false, "src/os/c.go": true, "bin": false, "bin/tinygo": true} {
		info, err := os.Lstat(filepath.Join(output, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if (info.Mode()&os.ModeSymlink != 0) != link {
			t.Fatalf("%s link=%v", path, !link)
		}
	}
	if _, err := os.Lstat(filepath.Join(output, "src", "runtime", "a.go")); !os.IsNotExist(err) {
		t.Fatal("the changed file was mirrored")
	}
}
