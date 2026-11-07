// rootlock serializes independent processes that read or replace a
// generated toolchain root under a build directory.
//
// Each root has one lock file under <build>/.locks, named after the root's
// kind and a hash of its canonical absolute path. Readers hold flock(2)
// LOCK_SH for the whole life of the command they run; a writer holds
// LOCK_EX while it stages, invalidates its cache namespace and publishes.
// The OS drops the lock when its holder exits or dies.
//
// clean takes LOCK_EX on every generated root of a build directory in one
// fixed order, renames the build directory aside and removes it. A process
// that waited on a lock file clean removed wakes on an inode that no longer
// sits at the lock path; it compares the two after each flock and retries,
// so two processes never hold independent locks for one root.
//
// Usage:
//
//	rootlock shared|exclusive [-stamp FILE] ROOT -- COMMAND [ARG...]
//	rootlock publish STAGE ROOT
//	rootlock clean BUILD_DIR
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// Kinds lists the generated roots under <build>/toolchains, in the order
// clean locks them.
var kinds = []string{"crypto", "goroutines", "ssh", "tls"}

// envLock names the variable rootlock sets for the command it runs:
// "<mode> <root>" with root exactly as given, so a script that re-executes
// itself under a lock can recognise that it already holds it.
const envLock = "GOOS_ROOT_LOCK"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rootlock:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "shared", "exclusive":
		return lockAndExec(args[0], args[1:])
	case "publish":
		if len(args) != 3 {
			return usage()
		}
		return publish(args[1], args[2])
	case "clean":
		if len(args) != 2 {
			return usage()
		}
		return clean(args[1])
	}
	return usage()
}

func usage() error {
	return errors.New("usage: rootlock shared|exclusive [-stamp FILE] ROOT -- COMMAND [ARG...] | publish STAGE ROOT | clean BUILD_DIR")
}

func lockAndExec(mode string, args []string) error {
	stamp := ""
	if len(args) >= 2 && args[0] == "-stamp" {
		stamp, args = args[1], args[2:]
	}
	if len(args) < 3 || args[1] != "--" {
		return usage()
	}
	root, command := args[0], args[2:]
	how := syscall.LOCK_SH
	if mode == "exclusive" {
		how = syscall.LOCK_EX
	}
	file, err := acquire(root, how)
	if err != nil {
		return err
	}
	if stamp != "" {
		if _, err := os.Stat(filepath.Join(root, stamp)); err != nil {
			return fmt.Errorf("%s is not prepared (%v); build its stamp first", root, err)
		}
	}
	// Go opens files close-on-exec; the command must inherit the lock.
	if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFD, 0); err != nil {
		return fmt.Errorf("keep %s open across exec: %w", file.Name(), err)
	}
	path, err := exec.LookPath(command[0])
	if err != nil {
		return err
	}
	env := append(os.Environ(), envLock+"="+mode+" "+root)
	return syscall.Exec(path, command, env)
}

// canonical returns root with its parent directory resolved, creating the
// parent when it is missing. The root itself may be absent.
func canonical(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return "", err
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// lockPath maps a canonical root to its lock file. A root under
// <build>/toolchains keeps its lock in <build>/.locks; any other root keeps
// it in .locks beside itself. Every process derives the same path from the
// same root, whatever build directory it runs for.
func lockPath(root string) string {
	parent := filepath.Dir(root)
	dir := parent
	if filepath.Base(parent) == "toolchains" {
		dir = filepath.Dir(parent)
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(dir, ".locks", filepath.Base(root)+"-"+hex.EncodeToString(sum[:8])+".lock")
}

// acquire locks root's lock file with how and returns the open file. It
// retries until the locked inode is the one at the lock path.
func acquire(root string, how int) (*os.File, error) {
	for {
		resolved, err := canonical(root)
		if err != nil {
			return nil, err
		}
		file, err := lockOnce(lockPath(resolved), how)
		if err != nil {
			return nil, err
		}
		if file != nil {
			return file, nil
		}
	}
}

// lockOnce returns nil, nil when the lock file it locked was removed or
// replaced while it waited.
func lockOnce(path string, how int) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o666)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), how)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	held, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	current, err := os.Stat(path)
	if err != nil || !os.SameFile(held, current) {
		file.Close()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, nil
	}
	return file, nil
}

// publish makes stage the root in one rename. An existing root is swapped
// with stage and then removed from stage's path, so a reader never sees a
// missing or partial root and a writer killed in between leaves only a
// stale stage for the next writer to remove.
func publish(stage, root string) error {
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return os.Rename(stage, root)
	} else if err != nil {
		return err
	}
	if err := exchange(stage, root); err != nil {
		return fmt.Errorf("exchange %s with %s: %w", stage, root, err)
	}
	return os.RemoveAll(stage)
}

// clean removes build after it holds every generated root's lock. Earlier
// clean runs that died leave <build>.clean.<pid> beside it; those go too.
func clean(build string) error {
	var held []*os.File
	defer func() {
		for _, file := range held {
			file.Close()
		}
	}()
	roots := make([]string, len(kinds))
	for i, kind := range kinds {
		roots[i] = filepath.Join(build, "toolchains", kind)
	}
	sort.Strings(roots)
	for _, root := range roots {
		file, err := acquire(root, syscall.LOCK_EX)
		if err != nil {
			return err
		}
		held = append(held, file)
	}
	abs, err := filepath.Abs(build)
	if err != nil {
		return err
	}
	stale, err := filepath.Glob(abs + ".clean.*")
	if err != nil {
		return err
	}
	aside := abs + ".clean." + strconv.Itoa(os.Getpid())
	if err := os.Rename(abs, aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, path := range append(stale, aside) {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}
