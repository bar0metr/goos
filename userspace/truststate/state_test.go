package truststate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"goos/userspace/truststore"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type modelFiles struct {
	data              map[string][]byte
	dirs              map[string]bool
	mutations, failAt int
	locked            bool
	beforeRename      func()
}

func newFiles() *modelFiles {
	return &modelFiles{data: map[string][]byte{}, dirs: map[string]bool{StatePath: true, GenerationsPath: true}}
}
func (f *modelFiles) fail() error {
	f.mutations++
	if f.failAt == f.mutations {
		return errors.New("injected persistence failure")
	}
	return nil
}
func (f *modelFiles) Read(path string, limit int) ([]byte, error) {
	data, ok := f.data[path]
	if !ok {
		return nil, ErrNotFound
	}
	if len(data) > limit {
		return nil, errors.New("oversized")
	}
	return bytes.Clone(data), nil
}
func (f *modelFiles) Create(path string, data []byte) error {
	if err := f.fail(); err != nil {
		return err
	}
	if _, ok := f.data[path]; ok {
		return errors.New("exists")
	}
	f.data[path] = bytes.Clone(data)
	return nil
}
func (f *modelFiles) Mkdir(path string) error {
	if err := f.fail(); err != nil {
		return err
	}
	f.dirs[path] = true
	return nil
}
func (f *modelFiles) Rename(source, target string) error {
	if f.beforeRename != nil {
		hook := f.beforeRename
		f.beforeRename = nil
		hook()
	}
	if err := f.fail(); err != nil {
		return err
	}
	if f.dirs[target] {
		return errors.New("exists")
	}
	if !f.dirs[source] {
		return ErrNotFound
	}
	f.dirs[target] = true
	delete(f.dirs, source)
	for path, data := range f.data {
		if strings.HasPrefix(path, source+"/") {
			f.data[target+strings.TrimPrefix(path, source)] = data
			delete(f.data, path)
		}
	}
	return nil
}
func (f *modelFiles) Replace(source, target string) error {
	if err := f.fail(); err != nil {
		return err
	}
	data, ok := f.data[source]
	if !ok {
		return ErrNotFound
	}
	f.data[target] = data
	delete(f.data, source)
	return nil
}
func (f *modelFiles) List(path string, limit int) ([]string, error) {
	if !f.dirs[path] {
		return nil, ErrNotFound
	}
	var names []string
	for dir := range f.dirs {
		if strings.HasPrefix(dir, path+"/") {
			name := strings.TrimPrefix(dir, path+"/")
			if !strings.Contains(name, "/") {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	if len(names) > limit {
		return nil, errors.New("too many")
	}
	return names, nil
}

func fixture(t *testing.T, n uint64, body string) (*truststore.Package, ed25519.PublicKey) {
	t.Helper()
	seed := make([]byte, 32)
	seed[0] = 42
	private := ed25519.NewKeyFromSeed(seed)
	public := private.Public().(ed25519.PublicKey)
	digest := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(digest[:])
	metadata, err := truststore.CanonicalMetadata(truststore.Metadata{Format: 1, Sequence: n, Source: "mozilla", SourceDate: "2026-10-09T00:00:00Z", SourceSHA256: hash, BundleSHA256: hash, Certificates: 1, BundleBytes: len(body)})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := truststore.SigningMessage(metadata, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := truststore.Verify([]byte(body), metadata, ed25519.Sign(private, msg), public, validateFixture)
	if err != nil {
		t.Fatal(err)
	}
	return pkg, public
}
func validateFixture(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, errors.New("empty")
	}
	return 1, nil
}
func setup(t *testing.T) (*Store, *modelFiles, *truststore.Package) {
	pkg, key := fixture(t, 10, "old roots")
	files := newFiles()
	store := &Store{Files: files, Key: key, Validate: validateFixture}
	if _, err := store.Install(pkg, "1111111111111111"); err != nil {
		t.Fatal(err)
	}
	return store, files, pkg
}

func TestSelectionAndRollback(t *testing.T) {
	store, files, old := setup(t)
	selected := store.Select()
	if selected.Package == nil || selected.Package.Metadata.Sequence != 10 {
		t.Fatalf("selection: %+v", selected)
	}
	older, _ := fixture(t, 9, "older roots")
	if _, err := store.Install(older, "2222222222222222"); err == nil {
		t.Fatal("rollback accepted")
	}
	changed, _ := fixture(t, 10, "different roots")
	if _, err := store.Install(changed, "2222222222222222"); err == nil {
		t.Fatal("sequence replacement accepted")
	}
	if _, err := store.Install(old, "2222222222222222"); err != nil {
		t.Fatal(err)
	}
	if got := store.Select(); got.Package == nil || !bytes.Equal(got.Package.Bundle, old.Bundle) {
		t.Fatal("rejected update changed selection")
	}
	files.data[ActivePath] = []byte("broken selector")
	if got := store.Select(); got.Package == nil || got.Package.Metadata.Sequence != 10 {
		t.Fatal("valid generation recovery failed")
	}
}

func TestCorruptStateUsesFallback(t *testing.T) {
	for _, file := range []string{"ca-certificates.pem", "ca-certificates.json", "ca-certificates.sig", "committed"} {
		t.Run(file, func(t *testing.T) {
			store, files, _ := setup(t)
			files.data[directory(10)+"/"+file] = []byte("corrupt")
			selected := store.Select()
			if selected.Package != nil || selected.Reason == "" || selected.Floor != 10 {
				t.Fatalf("corrupt state selected: %+v", selected)
			}
		})
	}
	store, files, _ := setup(t)
	delete(files.data, directory(10)+"/ca-certificates.sig")
	if store.Select().Package != nil {
		t.Fatal("partial state selected")
	}
}

func TestAbsentStateAndIncompleteStaging(t *testing.T) {
	_, key := fixture(t, 10, "roots")
	files := newFiles()
	store := &Store{Files: files, Key: key, Validate: validateFixture}
	files.dirs[GenerationsPath+"/.trust-new-99-incomplete"] = true
	if got := store.Select(); got.Package != nil || got.Floor != 0 {
		t.Fatalf("staging selected: %+v", got)
	}
}

func TestFailureLeavesOldOrCompleteNewGeneration(t *testing.T) {
	for fail := 1; fail <= 12; fail++ {
		t.Run(string(rune('A'+fail)), func(t *testing.T) {
			store, files, old := setup(t)
			incoming, _ := fixture(t, 11, "new roots")
			files.mutations = 0
			files.failAt = fail
			_, _ = store.Install(incoming, "2222222222222222")
			files.failAt = 0
			got := store.Select()
			if got.Package == nil {
				t.Fatalf("failure %d lost active generation: %+v", fail, got)
			}
			if !bytes.Equal(got.Package.Bundle, old.Bundle) && !bytes.Equal(got.Package.Bundle, incoming.Bundle) {
				t.Fatalf("failure %d selected partial data", fail)
			}
			if !bytes.Equal(files.data[directory(10)+"/ca-certificates.pem"], old.Bundle) {
				t.Fatal("old generation changed")
			}
		})
	}
}

func TestSelectorCannotDowngradeSurvivingGeneration(t *testing.T) {
	store, files, _ := setup(t)
	incoming, _ := fixture(t, 11, "new roots")
	if _, err := store.Install(incoming, "2222222222222222"); err != nil {
		t.Fatal(err)
	}
	files.data[ActivePath] = []byte("10\n")
	if got := store.Select(); got.Package == nil || got.Package.Metadata.Sequence != 11 {
		t.Fatalf("selector rollback: %+v", got)
	}
	files.data[directory(11)+"/ca-certificates.sig"] = []byte("corrupt")
	if got := store.Select(); got.Package != nil || got.Floor != 11 {
		t.Fatal("corrupt highest generation silently downgraded")
	}
}

func TestGenerationCapacityPreservesActiveRoots(t *testing.T) {
	store, files, _ := setup(t)
	for n := uint64(11); n < uint64(10+MaxGenerations); n++ {
		files.dirs[directory(n)] = true
	}
	// Keep a valid latest generation without allocating all historic packages.
	incoming, _ := fixture(t, uint64(10+MaxGenerations-1), "latest roots")
	path := directory(incoming.Metadata.Sequence)
	files.data[path+"/ca-certificates.pem"] = incoming.Bundle
	files.data[path+"/ca-certificates.json"] = incoming.Meta
	files.data[path+"/ca-certificates.sig"] = incoming.Signature
	files.data[path+"/committed"] = marker(incoming.Meta)
	files.data[ActivePath] = []byte(strconv.FormatUint(incoming.Metadata.Sequence, 10) + "\n")
	next, _ := fixture(t, incoming.Metadata.Sequence+1, "next roots")
	if _, err := store.Install(next, "3333333333333333"); err == nil {
		t.Fatal("full generation set accepted another install")
	}
	if got := store.Select(); got.Package == nil || got.Package.Metadata.Sequence != incoming.Metadata.Sequence {
		t.Fatalf("capacity destroyed selection: %+v", got)
	}
}

func TestInterruptedStagingDoesNotCountAsGenerations(t *testing.T) {
	store, files, _ := setup(t)
	for i := 0; i < 100; i++ {
		files.dirs[GenerationsPath+"/.trust-new-interrupted-"+strconv.Itoa(i)] = true
	}
	incoming, _ := fixture(t, 11, "new roots")
	if _, err := store.Install(incoming, "3333333333333333"); err != nil {
		t.Fatal(err)
	}
	if got := store.Select(); got.Package == nil || got.Package.Metadata.Sequence != 11 {
		t.Fatalf("staging affected selection: %+v", got)
	}
}

func TestDirectoryCapacityRejectsBeforeStaging(t *testing.T) {
	store, files, _ := setup(t)
	for i := 0; i < MaxDirectoryEntries-1; i++ {
		files.dirs[GenerationsPath+"/.abandoned-"+strconv.Itoa(i)] = true
	}
	incoming, _ := fixture(t, 11, "new roots")
	before := files.mutations
	if _, err := store.Install(incoming, "4444444444444444"); err == nil {
		t.Fatal("full directory accepted install")
	}
	if files.mutations != before {
		t.Fatal("capacity refusal mutated state")
	}
	if got := store.Select(); got.Package == nil || got.Package.Metadata.Sequence != 10 {
		t.Fatal("capacity refusal lost current roots")
	}
}

func TestFailedStageAtDirectoryBoundaryKeepsSelection(t *testing.T) {
	store, files, _ := setup(t)
	for i := 0; i < MaxDirectoryEntries-2; i++ {
		files.dirs[GenerationsPath+"/.abandoned-"+strconv.Itoa(i)] = true
	}
	incoming, _ := fixture(t, 11, "new roots")
	files.mutations = 0
	files.failAt = 4
	if _, err := store.Install(incoming, "4444444444444444"); err == nil {
		t.Fatal("injected failure succeeded")
	}
	files.failAt = 0
	if got := store.Select(); got.Package == nil || got.Package.Metadata.Sequence != 10 {
		t.Fatal("interrupted staging lost current roots")
	}
	if _, err := store.Install(incoming, "5555555555555555"); err != nil {
		t.Fatal("retry after failed stage", err)
	}
	if got := store.Select(); got.Package == nil || got.Package.Metadata.Sequence != 11 {
		t.Fatal("complete update not selected")
	}
}

func (f *modelFiles) Lock() error {
	if f.locked {
		return errors.New("trust installer busy")
	}
	f.locked = true
	return nil
}
func (f *modelFiles) Unlock() error { f.locked = false; return nil }

func TestConcurrentPublicationIsSerialized(t *testing.T) {
	store, files, _ := setup(t)
	for i := 0; i < MaxDirectoryEntries-2; i++ {
		files.dirs[GenerationsPath+"/.abandoned-"+strconv.Itoa(i)] = true
	}
	first, _ := fixture(t, 11, "first roots")
	second, _ := fixture(t, 12, "second roots")
	var concurrentErr error
	files.beforeRename = func() { _, concurrentErr = store.Install(second, "6666666666666666") }
	if _, err := store.Install(first, "5555555555555555"); err != nil {
		t.Fatal(err)
	}
	if concurrentErr == nil {
		t.Fatal("concurrent installer acquired publication lock")
	}
	if got := store.Select(); got.Package == nil || got.Package.Metadata.Sequence != 11 {
		t.Fatalf("publication corrupted selection: %+v", got)
	}
	if files.locked {
		t.Fatal("installer retained lock")
	}
}
