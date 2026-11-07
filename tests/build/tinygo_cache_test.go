package build

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestTinyGoCacheIsolation(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(bin, "tinygo")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$HOME\" \"$XDG_CACHE_HOME\" \"$GOPATH\" \"$GOMODCACHE\" \"$GOENV\" \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(tmp, "cache")
	home := filepath.Join(tmp, "global-home")
	globalCache := filepath.Join(tmp, "global-cache")
	for _, dir := range []string{home, globalCache} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := filepath.Join(globalCache, "sentinel")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0644); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, namespace := range []string{"tls", "ssh", "goroutines"} {
		namespace := namespace
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command("sh", filepath.Join(root, "tools/tinygo.sh"), "build", "-o", "output with space", "./cmd")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME="+home, "XDG_CACHE_HOME="+globalCache, "GOOS_BUILD_CACHE="+cache, "TINYGO_CACHE_NAMESPACE="+namespace, "TINYGO_BIN="+fake, "GOPATH="+filepath.Join(tmp, "gopath"), "GOMODCACHE="+filepath.Join(tmp, "modules"), "GOENV=off")
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("%s: %v: %s", namespace, err, out)
				return
			}
			lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
			if len(lines) != 9 {
				errs <- fmt.Errorf("%s: unexpected output %q", namespace, out)
				return
			}
			local := filepath.Join(cache, "tinygo-home-"+namespace)
			if runtime.GOOS == "darwin" {
				if lines[0] != local {
					errs <- fmt.Errorf("%s: HOME = %q, want %q", namespace, lines[0], local)
					return
				}
			} else if lines[1] != filepath.Join(local, "cache") {
				errs <- fmt.Errorf("%s: XDG_CACHE_HOME = %q", namespace, lines[1])
				return
			}
			want := []string{filepath.Join(tmp, "gopath"), filepath.Join(tmp, "modules"), "off", "build", "-o", "output with space", "./cmd"}
			for i, value := range want {
				if lines[i+2] != value {
					errs <- fmt.Errorf("%s: field %d = %q, want %q", namespace, i+2, lines[i+2], value)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("global cache sentinel: %q, %v", data, err)
	}
}
