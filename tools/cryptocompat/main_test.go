package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (source, output, backend string, pins sourcePins) {
	t.Helper()
	root := t.TempDir()
	source = filepath.Join(root, "source")
	output = filepath.Join(root, "output")
	backend = filepath.Join(root, "backend.go")
	writeFixture(t, filepath.Join(source, "src", "runtime", "runtime_uefi.go"), "//go:build uefi\n\npackage runtime\n")
	writeFixture(t, filepath.Join(source, "src", "runtime", "baremetal.go"), "// license kept\npackage runtime\n\n"+originalNow+"\n// suffix kept\n")
	writeFixture(t, filepath.Join(source, "src", "runtime", "other.go"), "runtime other")
	writeFixture(t, filepath.Join(source, "src", "crypto", "fixture.go"), "crypto source")
	writeFixture(t, filepath.Join(source, "src", "internal", "fixture.go"), "internal source")
	writeFixture(t, filepath.Join(source, "src", "crypto", "runtime_uefi.go"), "nested exclusion is not skipped")
	writeFixture(t, filepath.Join(source, "bin", "fixture"), "binary entry")
	writeFixture(t, filepath.Join(source, "root-file"), "root file")
	writeFixture(t, backend, "//go:build uefi\n\npackage runtime\n// native backend\n")
	if err := os.Mkdir(output, 0755); err != nil {
		t.Fatal(err)
	}
	uefi, err := os.ReadFile(filepath.Join(source, "src", "runtime", "runtime_uefi.go"))
	if err != nil {
		t.Fatal(err)
	}
	baremetal, err := os.ReadFile(filepath.Join(source, "src", "runtime", "baremetal.go"))
	if err != nil {
		t.Fatal(err)
	}
	return source, output, backend, sourcePins{digest(uefi), digest(baremetal)}
}

func TestPrepareMirror(t *testing.T) {
	source, output, backend, pins := setup(t)
	original, _ := os.ReadFile(filepath.Join(source, "src", "runtime", "baremetal.go"))
	if err := prepare(source, output, backend, pins, "tinygo version 0.42.0 test"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"bin", "root-file", "src/crypto/fixture.go", "src/crypto/runtime_uefi.go", "src/internal/fixture.go", "src/runtime/other.go"} {
		link, err := os.Readlink(filepath.Join(output, path))
		if err != nil || link != filepath.Join(source, path) {
			t.Fatalf("mirror %s: target=%q error=%v", path, link, err)
		}
	}
	for _, path := range []string{"src", "src/crypto", "src/internal", "src/runtime", "src/runtime/runtime_uefi.go", "src/runtime/baremetal.go", metadataFile} {
		info, err := os.Lstat(filepath.Join(output, path))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("expected copied entry %s: %v", path, err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(output, "src", "runtime", "runtime_uefi.go"))
	want, _ := os.ReadFile(backend)
	if !bytes.Equal(got, want) {
		t.Fatal("backend copy mismatch")
	}
	patched, _ := os.ReadFile(filepath.Join(output, "src", "runtime", "baremetal.go"))
	if string(patched) != strings.Replace(string(original), originalNow, replacementNow, 1) {
		t.Fatal("patch changed source outside now")
	}
	unchanged, _ := os.ReadFile(filepath.Join(source, "src", "runtime", "baremetal.go"))
	if !bytes.Equal(unchanged, original) {
		t.Fatal("installed source changed")
	}
	var record metadata
	data, err := os.ReadFile(filepath.Join(output, metadataFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.SourceUEFI != pins.UEFI || record.SourceBaremetal != pins.Baremetal || record.Backend != digest(want) || record.PatchedBaremetal != digest(patched) || record.TinyGoVersion != pinnedVersion || record.Toolchain != "tinygo version 0.42.0 test" {
		t.Fatalf("wrong metadata: %+v", record)
	}
	if err := prepare(source, output, backend, pins, "test"); err == nil {
		t.Fatal("reused nonempty root")
	}
}

func TestStrictPatch(t *testing.T) {
	for _, source := range []string{
		"package runtime",
		strings.Replace(originalNow, "mono = nanotime()", "mono = 0", 1),
		originalNow + "\n" + originalNow,
		originalNow + "\nfunc now() {}",
	} {
		if _, err := patchBaremetal([]byte(source)); err == nil {
			t.Fatalf("accepted changed patch input %q", source)
		}
	}
}

func TestValidationLeavesOutputEmpty(t *testing.T) {
	for _, failure := range []string{"uefi hash", "baremetal hash", "backend tag", "patch shape", "metadata collision"} {
		t.Run(failure, func(t *testing.T) {
			source, output, backend, pins := setup(t)
			switch failure {
			case "uefi hash":
				pins.UEFI = "bad hash"
			case "baremetal hash":
				pins.Baremetal = "bad hash"
			case "backend tag":
				writeFixture(t, backend, "package runtime\n")
			case "patch shape":
				changed := strings.Replace(originalNow, "mono = nanotime()", "mono = 0", 1)
				writeFixture(t, filepath.Join(source, "src", "runtime", "baremetal.go"), changed)
				pins.Baremetal = digest([]byte(changed))
			case "metadata collision":
				writeFixture(t, filepath.Join(source, metadataFile), "reserved")
			}
			if err := prepare(source, output, backend, pins, "test"); err == nil {
				t.Fatal("invalid input accepted")
			}
			if err := emptyOutput(output); err != nil {
				t.Fatalf("failed preparation modified output: %v", err)
			}
			stages, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".cryptocompat-*"))
			if err != nil || len(stages) != 0 {
				t.Fatalf("temporary staging leaked: %v %v", stages, err)
			}
		})
	}
}

func TestOutputHygiene(t *testing.T) {
	source, output, backend, pins := setup(t)
	writeFixture(t, filepath.Join(output, "keep"), "unchanged")
	if err := prepare(source, output, backend, pins, "test"); err == nil {
		t.Fatal("nonempty output accepted")
	}
	data, _ := os.ReadFile(filepath.Join(output, "keep"))
	if string(data) != "unchanged" {
		t.Fatal("existing output modified")
	}
	link := filepath.Join(filepath.Dir(output), "output-link")
	if err := os.Symlink(output, link); err != nil {
		t.Fatal(err)
	}
	if err := prepare(source, link, backend, pins, "test"); err == nil {
		t.Fatal("symlink output accepted")
	}
	if err := emptyOutput(filepath.Join(filepath.Dir(output), "missing")); err == nil {
		t.Fatal("missing output accepted")
	}
}

func TestCLIArgumentAndLocationValidation(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("missing arguments accepted")
	}
	if err := run([]string{"source", t.TempDir(), "backend"}); err == nil || !strings.Contains(err.Error(), "/Volumes/Data/tmp") {
		t.Fatalf("outside output accepted or unclear diagnostic: %v", err)
	}
}
