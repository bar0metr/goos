package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func tryLock(t *testing.T, root string, how int) bool {
	t.Helper()
	resolved, err := canonical(root)
	if err != nil {
		t.Fatal(err)
	}
	file, err := lockOnce(lockPath(resolved), how|syscall.LOCK_NB)
	if err == nil && file != nil {
		file.Close()
		return true
	}
	return false
}

func TestLockPathKeepsLocksAtBuildRoot(t *testing.T) {
	build := t.TempDir()
	root, err := canonical(filepath.Join(build, "toolchains", "tls"))
	if err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(build)
	if got := filepath.Dir(lockPath(root)); got != filepath.Join(real, ".locks") {
		t.Fatalf("lock directory %s, want %s/.locks", got, real)
	}
	other, _ := canonical(filepath.Join(build, "toolchains", "ssh"))
	if lockPath(root) == lockPath(other) {
		t.Fatal("different roots share a lock file")
	}
}

func TestLockPathFollowsCanonicalParent(t *testing.T) {
	dir := t.TempDir()
	build := filepath.Join(dir, "build")
	if err := os.MkdirAll(filepath.Join(build, "toolchains"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(build, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	direct, _ := canonical(filepath.Join(build, "toolchains", "tls"))
	aliased, _ := canonical(filepath.Join(dir, "alias", "toolchains", "tls"))
	if lockPath(direct) != lockPath(aliased) {
		t.Fatalf("aliases lock %s and %s", lockPath(direct), lockPath(aliased))
	}
}

func TestWriterExcludesReaders(t *testing.T) {
	root := filepath.Join(t.TempDir(), "toolchains", "tls")
	reader, err := acquire(root, syscall.LOCK_SH)
	if err != nil {
		t.Fatal(err)
	}
	if !tryLock(t, root, syscall.LOCK_SH) {
		t.Fatal("a second reader waited for the first")
	}
	if tryLock(t, root, syscall.LOCK_EX) {
		t.Fatal("a writer locked a root that a reader holds")
	}
	reader.Close()
	if !tryLock(t, root, syscall.LOCK_EX) {
		t.Fatal("the writer still waits after the reader left")
	}
}

func TestRootsLockIndependently(t *testing.T) {
	build := t.TempDir()
	tls, err := acquire(filepath.Join(build, "toolchains", "tls"), syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	defer tls.Close()
	if !tryLock(t, filepath.Join(build, "toolchains", "goroutines"), syscall.LOCK_EX) {
		t.Fatal("a TLS writer blocked the goroutine root")
	}
	if !tryLock(t, filepath.Join(t.TempDir(), "toolchains", "tls"), syscall.LOCK_EX) {
		t.Fatal("a TLS writer blocked another build directory")
	}
}

// A waiter that locks a removed lock file must not keep it: it retries on
// the file now at the lock path.
func TestWaiterRetriesAfterLockFileRemoval(t *testing.T) {
	root := filepath.Join(t.TempDir(), "toolchains", "tls")
	resolved, _ := canonical(root)
	path := lockPath(resolved)
	first, err := acquire(root, syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	second, err := acquire(root, syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	first.Close()
	// The removed inode is free now, but locking it proves nothing.
	if err := syscall.Flock(int(stale.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("lock the removed inode: %v", err)
	}
	if file, err := lockOnce(path, syscall.LOCK_EX|syscall.LOCK_NB); err == nil && file != nil {
		t.Fatal("a third writer locked the root the second writer holds")
	}
}

func TestPublishSwapsCompleteRoots(t *testing.T) {
	dir := t.TempDir()
	root, stage := filepath.Join(dir, "tls"), filepath.Join(dir, "tls.stage.1")
	for _, path := range []string{root, stage} {
		if err := os.MkdirAll(path, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "old"), nil, 0o666)
	os.WriteFile(filepath.Join(stage, "new"), nil, 0o666)
	if err := publish(stage, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "new")); err != nil {
		t.Fatalf("published root lacks the new tree: %v", err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("the replaced root stays at %s", stage)
	}
	fresh := filepath.Join(dir, "ssh")
	os.MkdirAll(filepath.Join(dir, "ssh.stage.1"), 0o777)
	if err := publish(filepath.Join(dir, "ssh.stage.1"), fresh); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(fresh); err != nil || !info.IsDir() {
		t.Fatalf("first publication: %v", err)
	}
}

func TestCleanWaitsForReadersAndRemovesEverything(t *testing.T) {
	build := filepath.Join(t.TempDir(), "build")
	root := filepath.Join(build, "toolchains", "crypto")
	if err := os.MkdirAll(filepath.Join(build, "cache"), 0o777); err != nil {
		t.Fatal(err)
	}
	reader, err := acquire(root, syscall.LOCK_SH)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- clean(build) }()
	select {
	case err := <-done:
		t.Fatalf("clean finished while a reader held the root: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(build, "cache")); err != nil {
		t.Fatalf("clean removed state under an active reader: %v", err)
	}
	reader.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(build); !os.IsNotExist(err) {
		t.Fatalf("clean left %s: %v", build, err)
	}
	if leftovers, _ := filepath.Glob(build + ".clean.*"); len(leftovers) != 0 {
		t.Fatalf("clean left %v", leftovers)
	}
}
