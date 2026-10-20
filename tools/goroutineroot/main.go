// goroutineroot prepares the copy-on-write TinyGo root that goroutine
// programs build with (ADR 0204). It mirrors a pinned TinyGo 0.42.0 install
// through symbolic links and replaces the runtime and internal/task files
// that implement scheduling with the GoOS versions under tinygo/goroutines,
// after checking that every file it replaces or patches is the pinned
// original. The installed toolchain is never written.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"goos/tools/internal/compatroot"
)

const pinnedVersion = "0.42.0"
const metadataFile = "goroutineroot.json"

// A replacement swaps a whole file of the source root for an overlay file.
type replacement struct {
	path     string // in the root, slash-separated
	overlay  string // relative to the overlay directory
	original string // SHA-256 of the pinned source file
}

// A patch changes one exact piece of a source file.
type patch struct {
	path     string
	original string
	old, new string
}

var replacements = []replacement{
	{"src/sync/mutex.go", "sync-mutex.go.txt", "fda11e7a574dcddc09fbfe9223048571e837776ddfcae6a81597cd7c48a8f4db"},
	{"src/internal/task/pmutex-preemptive.go", "pmutex-preemptive.go.txt", "5ef4d819ff20b83d863af91240255e727eadb66a4f702adfacca4f4863d8fa2e"},
	{"src/runtime/runtime_uefi.go", "../runtime/native.go.txt", "e17517d3eca2177d9d4094abcc35347c3f650d5e52d1df92e3dd45ebe8b0f393"},
	{"src/runtime/scheduler_cores.go", "scheduler_cores.go.txt", "09d28211279e80f0ef43e2f7768a1429a04a6419faf1857055f07a14039b2d7e"},
	{"src/internal/task/task_stack.go", "task_stack.go.txt", "ebbd7303f2fd74c5a89f0e303048236d17fd656a954091d0ce175126abef1795"},
	{"src/internal/task/task_stack_multicore.go", "task_stack_multicore.go.txt", "9c3a9f2f749b331ab5c60fbfbc39040c8236dff010c4a7e9d714cfa5a2e207e0"},
	{"src/internal/task/task_exit.go", "task_exit.go.txt", "a844434131bfb347e48350a24b0a623a08277eed3b33e8bf87d080c24d7c33d1"},
	{"src/internal/task/futex-cores.go", "futex-cores.go.txt", "1edb0a53be8bb3d410d2e886e5483e0fbb85a0d0168b7810f2df3c2e983cd2ff"},
}

var patches = []patch{
	{
		// A send on a closed channel panics inside trySend with the
		// channel's lock held, so a recovered panic left the channel locked
		// for good. Check first and release the lock before the panic.
		path: "src/runtime/chan.go", original: "8cf81f77b92ebd99413d93a4e6bf88fe49967a700fbe72a229082a8f267436b5",
		old: `	mask := interrupt.Disable()
	ch.lock.Lock()

	// See whether we can proceed immediately, and if so, return early.
	if sent, wake := ch.trySend(value); sent {`,
		new: `	mask := interrupt.Disable()
	ch.lock.Lock()

	if ch.closed {
		ch.lock.Unlock()
		interrupt.Restore(mask)
		runtimePanic("send on closed channel")
	}

	// See whether we can proceed immediately, and if so, return early.
	if sent, wake := ch.trySend(value); sent {`,
	},
	{
		// Select holds all channel locks and the global select lock.
		// Balance their pins before a recoverable closed-send panic.
		path: "src/runtime/chan.go",
		old: `		} else { // chan send
			if sent, receiver := state.ch.trySend(state.value); sent {`,
		new: `		} else { // chan send
			if state.ch.closed {
				unlockAllStates(states)
				chanSelectLock.Unlock()
				interrupt.Restore(mask)
				runtimePanic("send on closed channel")
			}
			if sent, receiver := state.ch.trySend(state.value); sent {`,
	},
	{
		// After a blocked select wakes, its cleanup has released the locks.
		// A closed send must panic; a closed receive returns zero, false.
		path: "src/runtime/chan.go",
		old: `	selectIndex = t.DataUint32() >> 2
	selectOk = t.DataUint32()&chanOperationMask != chanOperationClosed

	return selectIndex, selectOk`,
		new: `	selectIndex = t.DataUint32() >> 2
	selectOk = t.DataUint32()&chanOperationMask != chanOperationClosed

	if !selectOk && states[selectIndex].value != nil {
		runtimePanic("send on closed channel")
	}

	return selectIndex, selectOk`,
	},
	{
		path: "src/runtime/baremetal.go", original: "111e71bb241f682738895ec039dec7115b0ee16183fb240d42017545027b2e1f",
		old: `func now() (sec int64, nsec int32, mono int64) {
	mono = nanotime()
	to := timeOffset.Load()
	sec = (mono + to) / (1000 * 1000 * 1000)
	nsec = int32((mono + to) - sec*(1000*1000*1000))
	return
}`,
		new: `func now() (sec int64, nsec int32, mono int64) {
	return goosNativeNow()
}`,
	},
	{
		path: "src/runtime/runtime.go", original: "d1be2f7083071bd44d140d7e5531a3aff4f4da53506f94ad6a8ef56fb2a185db",
		old: `func GOMAXPROCS(n int) int {
	// Note: setting GOMAXPROCS is ignored.
	return 1
}`,
		new: `func GOMAXPROCS(n int) int {
	return goosMaxProcs(n)
}`,
	},
	{
		path: "src/runtime/runtime.go",
		old: `func LockOSThread() {
}`,
		new: `func LockOSThread() {
	runtimePanic("LockOSThread is not supported on GoOS; use thread.Start")
}`,
	},
	{
		path: "src/runtime/debug.go", original: "3b1cc227f7bc9fb8106cc4a5f8f6a3ef07be686d958c1748430757395accd7e8",
		old: `// Stub for NumGoroutine, does not return the real value
func NumGoroutine() int {
	return 1
}`,
		new: `// NumGoroutine returns the number of goroutines that exist.
func NumGoroutine() int {
	return goosGoroutines()
}`,
	},
}

type metadata struct {
	TinyGoVersion string            `json:"tinygo_version"`
	Toolchain     string            `json:"toolchain"`
	Sources       map[string]string `json:"source_sha256"`
	Outputs       map[string]string `json:"output_sha256"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "goroutineroot:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 3 {
		return errors.New("usage: goroutineroot SOURCE_TINYGOROOT OUTPUT_EMPTY_ROOT OVERLAY_DIR")
	}
	output, err := filepath.Abs(args[1])
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return err
	}
	if parent != filepath.Dir(output) || !compatroot.Allowed(output) {
		return errors.New("output must be under the GoOS build directory")
	}
	version, err := exec.Command(filepath.Join(args[0], "bin", "tinygo"), "version").Output()
	if err != nil {
		return fmt.Errorf("read TinyGo version: %w", err)
	}
	toolchain := strings.TrimSpace(string(version))
	if !strings.HasPrefix(toolchain, "tinygo version "+pinnedVersion+" ") {
		return fmt.Errorf("expected TinyGo %s, got %q", pinnedVersion, toolchain)
	}
	return prepare(args[0], output, args[2], toolchain)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// build computes every changed file of the root from the source root and
// the overlay directory, checking each source against its pin.
func build(source, overlay string) (map[string][]byte, map[string]string, error) {
	// Keep host ./... discovery out of this generated standard-library tree.
	// TinyGo still resolves its runtime through TINYGOROOT, not this module.
	files := map[string][]byte{"go.mod": []byte("module goos.local/goroutine-toolchain\n")}
	sources := map[string]string{}
	for _, r := range replacements {
		original, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(r.path)))
		if err != nil {
			return nil, nil, err
		}
		if digest(original) != r.original {
			return nil, nil, fmt.Errorf("%s does not match the pinned TinyGo source", r.path)
		}
		sources[r.path] = r.original
		data, err := os.ReadFile(filepath.Join(overlay, filepath.FromSlash(r.overlay)))
		if err != nil {
			return nil, nil, err
		}
		if !bytes.HasPrefix(data, []byte("//go:build ")) {
			return nil, nil, fmt.Errorf("overlay %s must begin with a build constraint", r.overlay)
		}
		files[r.path] = data
	}
	for _, p := range patches {
		data, ok := files[p.path]
		if !ok {
			original, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(p.path)))
			if err != nil {
				return nil, nil, err
			}
			data = original
			sources[p.path] = digest(original)
		}
		if p.original != "" && sources[p.path] != p.original {
			return nil, nil, fmt.Errorf("%s does not match the pinned TinyGo source", p.path)
		}
		if bytes.Count(data, []byte(p.old)) != 1 {
			return nil, nil, fmt.Errorf("%s must contain the pinned text exactly once: %.40q", p.path, p.old)
		}
		files[p.path] = bytes.Replace(data, []byte(p.old), []byte(p.new), 1)
	}
	return files, sources, nil
}

func prepare(source, output, overlay, toolchain string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	if err := emptyOutput(output); err != nil {
		return err
	}
	files, sources, err := build(source, overlay)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(source, metadataFile)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("source root contains reserved metadata filename")
	}
	// Stage first: a failure leaves the requested root empty.
	stage, err := os.MkdirTemp(filepath.Dir(output), ".goroutineroot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := mirror(source, stage, "", files); err != nil {
		return err
	}
	outputs := map[string]string{}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := os.WriteFile(filepath.Join(stage, filepath.FromSlash(path)), files[path], 0644); err != nil {
			return err
		}
		outputs[path] = digest(files[path])
	}
	record := metadata{TinyGoVersion: pinnedVersion, Toolchain: toolchain, Sources: sources, Outputs: outputs}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, metadataFile), append(data, '\n'), 0644); err != nil {
		return err
	}
	info, err := os.Lstat(output)
	if err != nil {
		return err
	}
	if err := os.Chmod(stage, info.Mode().Perm()); err != nil {
		return err
	}
	// macOS cannot rename over an empty directory.
	if err := os.Remove(output); err != nil {
		return err
	}
	if err := os.Rename(stage, output); err != nil {
		return errors.Join(err, os.Mkdir(output, info.Mode().Perm()))
	}
	return nil
}

func emptyOutput(output string) error {
	info, err := os.Lstat(output)
	if err != nil {
		return fmt.Errorf("output must exist as an empty directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("output must be a directory, not a symlink")
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("output directory is not empty")
	}
	return nil
}

// mirror recreates every directory of source under output and links every
// file, except the changed files, which the caller writes. TinyGo merges
// its source tree with Go's by directory, so every directory must be real.
func mirror(source, output, prefix string, files map[string][]byte) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		path := name
		if prefix != "" {
			path = prefix + "/" + name
		}
		if _, changed := files[path]; changed {
			continue
		}
		sourcePath := filepath.Join(source, name)
		outputPath := filepath.Join(output, name)
		// Stat follows directory links in the trusted installed toolchain.
		info, err := os.Stat(sourcePath)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := os.Mkdir(outputPath, 0755); err != nil {
				return err
			}
			if err := mirror(sourcePath, outputPath, path, files); err != nil {
				return err
			}
			continue
		}
		if err := os.Symlink(sourcePath, outputPath); err != nil {
			return err
		}
	}
	return nil
}
