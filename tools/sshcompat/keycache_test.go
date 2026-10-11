package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"goos/internal/testscratch"
)

func TestNativeKeyCachePrivateRootAndStatelessLifetime(t *testing.T) {
	root := testscratch.TempDir(t, "goos-keycache-test-")
	output := filepath.Join(root, "go")
	directory := filepath.Join(output, filepath.Dir(keyCachePath))
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(runtime.GOROOT(), keyCachePath)
	before, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(output, keyCachePath)
	if err := os.Symlink(original, target); err != nil {
		t.Fatal(err)
	}
	if err := nativeKeyCache(runtime.GOROOT(), output, "keycache.go.txt"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(original)
	if string(before) != string(after) {
		t.Fatal("modified installed source")
	}
	info, _ := os.Lstat(target)
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("overlay writes through link")
	}
	if _, err := os.Stat(filepath.Join(output, "ssh-keycache.json")); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	contract := filepath.Join(root, "contract")
	if err := os.Mkdir(contract, 0755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"go.mod":   "module keycachetest\n\ngo 1.25\n",
		"cache.go": string(source),
		"cache_test.go": `package fips140cache
import("errors";"testing")
func TestLifetime(t *testing.T){
 var cache Cache[byte,int];var key byte;calls:=0
 create:=func()(*int,error){calls++;value:=calls;return &value,nil}
 check:=func(*int)bool{t.Fatal("retained key check");return true}
 first,err:=cache.Get(&key,create,check);if err!=nil||*first!=1{t.Fatal("first")}
 second,err:=cache.Get(&key,create,check);if err!=nil||*second!=2||first==second{t.Fatal("cached expanded key survived request")}
 failure:=errors.New("constructor failure");_,err=cache.Get(&key,func()(*int,error){return nil,failure},check);if err!=failure{t.Fatal("failure lost")}
}
`,
	} {
		if err := os.WriteFile(filepath.Join(contract, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "test", ".")
	cmd.Dir = contract
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cache lifetime contract: %v\n%s", err, output)
	}
	// Repatching an already modified input fails rather than hiding drift.
	if err := nativeKeyCache(runtime.GOROOT(), output, "keycache.go.txt"); err == nil {
		t.Fatal("unpinned private source accepted")
	}
	if err := nativeKeyCache(runtime.GOROOT(), runtime.GOROOT(), "keycache.go.txt"); err == nil {
		t.Fatal("installed root accepted")
	}
}

func keyCacheFixture(t *testing.T) (string, string, []byte) {
	t.Helper()
	directory := testscratch.TempDir(t, "goos-keycache-security-")
	output := filepath.Join(directory, "go")
	if err := os.MkdirAll(filepath.Join(output, filepath.Dir(keyCachePath)), 0755); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(runtime.GOROOT(), keyCachePath)
	upstream, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original, filepath.Join(output, keyCachePath)); err != nil {
		t.Fatal(err)
	}
	return directory, output, upstream
}

func TestNativeKeyCacheRejectsExistingMetadata(t *testing.T) {
	for _, kind := range []string{"symlink", "file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			directory, output, _ := keyCacheFixture(t)
			sentinel := filepath.Join(directory, "sentinel")
			if err := os.WriteFile(sentinel, []byte("KEEP"), 0600); err != nil {
				t.Fatal(err)
			}
			metadata := filepath.Join(output, "ssh-keycache.json")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(sentinel, metadata)
			case "file":
				err = os.WriteFile(metadata, []byte("KEEP"), 0600)
			case "directory":
				err = os.Mkdir(metadata, 0755)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := nativeKeyCache(runtime.GOROOT(), output, "keycache.go.txt"); err == nil {
				t.Fatal("preexisting metadata accepted")
			}
			text, err := os.ReadFile(sentinel)
			if err != nil || string(text) != "KEEP" {
				t.Fatal("metadata target modified", err)
			}
			info, err := os.Lstat(filepath.Join(output, keyCachePath))
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatal("cache modified before metadata refusal", err)
			}
			if kind == "file" {
				text, err := os.ReadFile(metadata)
				if err != nil || string(text) != "KEEP" {
					t.Fatal("existing metadata overwritten", err)
				}
			}
		})
	}
}

func TestNativeKeyCacheRejectsLinkedDirectory(t *testing.T) {
	directory, output, _ := keyCacheFixture(t)
	internal := filepath.Join(output, "src/crypto/internal")
	external := filepath.Join(directory, "external")
	if err := os.Rename(internal, external); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, internal); err != nil {
		t.Fatal(err)
	}
	if err := nativeKeyCache(runtime.GOROOT(), output, "keycache.go.txt"); err == nil {
		t.Fatal("linked directory accepted")
	}
	if _, err := os.Stat(filepath.Join(output, "ssh-keycache.json")); !os.IsNotExist(err) {
		t.Fatal("metadata created for unsafe output", err)
	}
}

func TestNativeKeyCacheDescriptorSurvivesRootReplacement(t *testing.T) {
	directory, output, upstream := keyCacheFixture(t)
	root, err := os.OpenRoot(output)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	owned := filepath.Join(directory, "owned")
	if err := os.Rename(output, owned); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(directory, "external")
	if err := os.MkdirAll(filepath.Join(external, filepath.Dir(keyCachePath)), 0755); err != nil {
		t.Fatal(err)
	}
	externalCache := filepath.Join(external, keyCachePath)
	if err := os.WriteFile(externalCache, upstream, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, output); err != nil {
		t.Fatal(err)
	}
	overlay, err := os.ReadFile("keycache.go.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeNativeKeyCache(root, runtime.GOROOT(), upstream, overlay, overlay); err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(externalCache)
	if err != nil || string(unchanged) != string(upstream) {
		t.Fatal("replacement root modified", err)
	}
	if _, err := os.Stat(filepath.Join(external, "ssh-keycache.json")); !os.IsNotExist(err) {
		t.Fatal("metadata escaped anchored root", err)
	}
	info, err := os.Lstat(filepath.Join(owned, keyCachePath))
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("original descriptor root not patched", err)
	}
}

func TestNativeKeyCacheWritesCannotEscapeChangedAncestor(t *testing.T) {
	directory, output, upstream := keyCacheFixture(t)
	root, err := os.OpenRoot(output)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	internal := filepath.Join(output, "src/crypto/internal")
	if err := os.Rename(internal, filepath.Join(directory, "owned-internal")); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(directory, "external")
	if err := os.MkdirAll(filepath.Join(external, "fips140cache"), 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(external, "fips140cache/cache.go")
	if err := os.WriteFile(sentinel, upstream, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, internal); err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(keyCachePath); err == nil {
		t.Fatal("remove escaped descriptor root")
	}
	if err := writeNewRootFile(root, keyCachePath, []byte("REPLACED")); err == nil {
		t.Fatal("write escaped descriptor root")
	}
	unchanged, err := os.ReadFile(sentinel)
	if err != nil || string(unchanged) != string(upstream) {
		t.Fatal("changed ancestor target modified", err)
	}
}

func TestNativeKeyCacheRejectsSourceRootInsideAllowedScratch(t *testing.T) {
	_, source, upstream := keyCacheFixture(t)
	target := filepath.Join(source, keyCachePath)
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, upstream, 0600); err != nil {
		t.Fatal(err)
	}
	err := nativeKeyCache(source, source, "keycache.go.txt")
	if err == nil || !strings.Contains(err.Error(), "source root") {
		t.Fatal("source root accepted", err)
	}
	unchanged, err := os.ReadFile(target)
	if err != nil || string(unchanged) != string(upstream) {
		t.Fatal("source modified", err)
	}
	if _, err := os.Stat(filepath.Join(source, "ssh-keycache.json")); !os.IsNotExist(err) {
		t.Fatal("source metadata created", err)
	}
}
