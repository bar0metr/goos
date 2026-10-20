package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const header = "id\tpackage\tsymbol\towner\tcontract\ttest\n"

// tree writes a module with the given files under dir and returns dir.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example\n\ngo 1.22\n"
	for name, text := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var sources = map[string]string{
	"userspace/a/a.go":          "//go:build goos_userspace\n\npackage a\n\nvar buffer [16]byte\nvar (\n\tcount, limit int\n\t_ = limit\n)\n",
	"userspace/a/a_host.go":     "//go:build !goos_userspace\n\npackage a\n\nvar buffer [8]byte\n",
	"userspace/a/a_test.go":     "package a\n\nvar fixture int\n",
	"userspace/b/c/c.go":        "package c\n\nimport \"errors\"\n\nvar ErrX = errors.New(\"x\")\n\nfunc f() { var local int; _ = local }\n",
	"userspace/b/testdata/t.go": "package t\n\nvar ignored int\n",
}

func withSources(audit string) map[string]string {
	files := map[string]string{"audit.tsv": audit}
	for name, text := range sources {
		files[name] = text
	}
	return files
}

func runTree(t *testing.T, audit string) (int, string) {
	t.Helper()
	dir := tree(t, withSources(audit))
	var stdout, stderr bytes.Buffer
	code := run(dir, "audit.tsv", false, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

const complete = "# comment\n" + header +
	"CA-0001\texample/userspace/a\tbuffer\tgoroutine\towned\tnone\n" +
	"CA-0002\texample/userspace/a\tcount\tprocess\tatomic\tTestCount\n" +
	"CA-0003\texample/userspace/a\tlimit\tprocess\timmutable\tnone\n" +
	"CA-0004\texample/userspace/b/c\tErrX\tprocess\timmutable\tnone\n"

func TestScanListsPackageVariables(t *testing.T) {
	dir := tree(t, withSources(complete))
	vars, err := scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range vars {
		got = append(got, v.Package+"."+v.Name)
	}
	want := []string{"example/userspace/a.buffer", "example/userspace/a.count", "example/userspace/a.limit", "example/userspace/b/c.ErrX"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("scan = %v, want %v", got, want)
	}
}

func TestCompleteAuditPasses(t *testing.T) {
	if code, output := runTree(t, complete); code != 0 {
		t.Fatalf("exit %d: %s", code, output)
	}
}

func TestMissingRowFails(t *testing.T) {
	audit := strings.Replace(complete, "CA-0002\texample/userspace/a\tcount\tprocess\tatomic\tTestCount\n", "", 1)
	code, output := runTree(t, audit)
	if code != 1 || !strings.Contains(output, "example/userspace/a.count has no audit row") {
		t.Fatalf("exit %d: %s", code, output)
	}
}

func TestStaleRowFails(t *testing.T) {
	audit := complete + "CA-0005\texample/userspace/a\tgone\tprocess\towned\tnone\n"
	code, output := runTree(t, audit)
	if code != 1 || !strings.Contains(output, "CA-0005 names example/userspace/a.gone, which no longer exists") {
		t.Fatalf("exit %d: %s", code, output)
	}
}

func TestMalformedRowsFail(t *testing.T) {
	for name, audit := range map[string]string{
		"duplicate id":     complete + "CA-0004\texample/userspace/a\tother\tprocess\towned\tnone\n",
		"duplicate symbol": complete + "CA-0005\texample/userspace/a\tbuffer\tprocess\towned\tnone\n",
		"bad id":           strings.Replace(complete, "CA-0001", "CA-1", 1),
		"bad owner":        strings.Replace(complete, "goroutine\towned", "everyone\towned", 1),
		"bad contract":     strings.Replace(complete, "goroutine\towned", "goroutine\thope", 1),
		"empty test":       strings.Replace(complete, "TestCount", "", 1),
		"short row":        complete + "CA-0005\texample/userspace/a\n",
		"bad header":       strings.Replace(complete, "symbol", "name", 1),
		"empty":            "# nothing\n",
	} {
		if code, output := runTree(t, audit); code != 1 {
			t.Errorf("%s: exit %d: %s", name, code, output)
		}
	}
}

func TestListMode(t *testing.T) {
	dir := tree(t, withSources(complete))
	var stdout, stderr bytes.Buffer
	if code := run(dir, "missing.tsv", true, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "example/userspace/a\tbuffer\tuserspace/a/a.go:5\n") {
		t.Fatalf("list output:\n%s", stdout.String())
	}
}

// TestRepositoryAudit runs the check on this repository, so go test fails as
// soon as a package-level variable under userspace/ lacks an audit row.
func TestRepositoryAudit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(filepath.Join("..", ".."), "docs/development/native-concurrency-audit.tsv", false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr.String())
	}
}
