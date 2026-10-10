package remotepolicy

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"testing"
)

const testHostPath = "/state/security/ssh/ssh_host_ed25519_key"

var errInjected = errors.New("injected storage failure")

type memoryFiles struct {
	files       map[string][]byte
	directories map[string]bool
	locked      bool
	operations  []string
	failAt      int
	mutate      func(operation string, files *memoryFiles)
}

func newMemoryFiles() *memoryFiles {
	return &memoryFiles{files: make(map[string][]byte), directories: make(map[string]bool)}
}

func (files *memoryFiles) op(operation string) error {
	files.operations = append(files.operations, operation)
	if files.mutate != nil {
		files.mutate(operation, files)
	}
	if files.failAt == len(files.operations) {
		return errInjected
	}
	return nil
}

func (files *memoryFiles) Lock() error {
	if err := files.op("lock"); err != nil {
		return err
	}
	if files.locked {
		return errors.New("already locked")
	}
	files.locked = true
	return nil
}
func (files *memoryFiles) Unlock() error {
	files.locked = false
	return files.op("unlock")
}
func (files *memoryFiles) Read(path string, limit int) ([]byte, error) {
	if err := files.op("read " + path); err != nil {
		return nil, err
	}
	data, ok := files.files[path]
	if !ok {
		return nil, ErrNotFound
	}
	if len(data) > limit {
		return nil, errors.New("file outside bound")
	}
	return append([]byte(nil), data...), nil
}
func (files *memoryFiles) Create(path string, data []byte) error {
	if err := files.op("create " + path); err != nil {
		return err
	}
	if _, exists := files.files[path]; exists {
		return errors.New("destination exists")
	}
	files.files[path] = append([]byte(nil), data...)
	return nil
}
func (files *memoryFiles) Mkdir(path string) error {
	if err := files.op("mkdir " + path); err != nil {
		return err
	}
	files.directories[path] = true
	return nil
}
func (files *memoryFiles) Rename(source, target string) error {
	if err := files.op("rename " + source + " " + target); err != nil {
		return err
	}
	data, ok := files.files[source]
	if !ok {
		return ErrNotFound
	}
	if _, exists := files.files[target]; exists {
		return errors.New("destination exists")
	}
	files.files[target] = data
	delete(files.files, source)
	return nil
}

type entropyReader struct {
	calls int
	fail  bool
}

func (reader *entropyReader) Read(data []byte) (int, error) {
	reader.calls++
	if reader.fail {
		return 0, errInjected
	}
	for index := range data {
		data[index] = 7
	}
	return len(data), nil
}

type deniedReader struct{}

func (deniedReader) Read([]byte) (int, error) { return 0, errors.New("unexpected entropy request") }

func verifyHostKey(t *testing.T, files *memoryFiles, key *HostKey, seed []byte) {
	t.Helper()
	if key == nil || !bytes.Equal(key.Private.Seed(), seed) || !bytes.Equal(files.files[testHostPath], seed) {
		t.Fatal("host key identity changed")
	}
	line, _ := PublicLine(key.Public)
	if !bytes.Equal(files.files[testHostPath+".pub"], line) || !ed25519.Verify(key.Public, []byte("host check"), ed25519.Sign(key.Private, []byte("host check"))) {
		t.Fatal("host seed/public pair inconsistent")
	}
	fingerprint, _ := Fingerprint(key.Public)
	if key.Fingerprint != fingerprint || files.locked {
		t.Fatal("fingerprint or lock state incorrect")
	}
}

func TestHostKeyGenerationAndStableReload(t *testing.T) {
	files := newMemoryFiles()
	entropy := &entropyReader{}
	store := HostStore{Files: files, Random: entropy}
	key, err := store.Load(testHostPath)
	if err != nil {
		t.Fatal(err)
	}
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	verifyHostKey(t, files, key, seed)
	if entropy.calls != 1 {
		t.Fatalf("entropy calls = %d", entropy.calls)
	}
	for _, path := range []string{"/state", "/state/security", "/state/security/ssh"} {
		if !files.directories[path] {
			t.Errorf("parent %s not created", path)
		}
	}
	store.Random = deniedReader{}
	second, err := store.Load(testHostPath)
	if err != nil {
		t.Fatal(err)
	}
	verifyHostKey(t, files, second, seed)
	if _, exists := files.files[testHostPath+".new"]; exists {
		t.Fatal("seed staging not consumed")
	}
	if _, exists := files.files[testHostPath+".pub.new"]; exists {
		t.Fatal("public staging not consumed")
	}
}

func TestHostKeyRecoversCompleteStaging(t *testing.T) {
	seed := bytes.Repeat([]byte{9}, ed25519.SeedSize)
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	line, _ := PublicLine(public)
	for _, fixture := range []struct {
		name                                             string
		finalSeed, finalPublic, stagedSeed, stagedPublic bool
	}{
		{"seed-staged", false, false, true, false},
		{"pair-staged", false, false, true, true},
		{"seed-published", true, false, false, true},
		{"public-missing", true, false, false, false},
		{"public-published", false, true, true, false},
		{"complete", true, true, false, false},
		{"consistent-stale-staging", true, true, true, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			files := newMemoryFiles()
			if fixture.finalSeed {
				files.files[testHostPath] = append([]byte(nil), seed...)
			}
			if fixture.finalPublic {
				files.files[testHostPath+".pub"] = append([]byte(nil), line...)
			}
			if fixture.stagedSeed {
				files.files[testHostPath+".new"] = append([]byte(nil), seed...)
			}
			if fixture.stagedPublic {
				files.files[testHostPath+".pub.new"] = append([]byte(nil), line...)
			}
			store := HostStore{Files: files, Random: deniedReader{}}
			key, err := store.Load(testHostPath)
			if err != nil {
				t.Fatal(err)
			}
			verifyHostKey(t, files, key, seed)
		})
	}
}

func TestHostKeyCorruptionNeverRegenerates(t *testing.T) {
	seed := bytes.Repeat([]byte{9}, ed25519.SeedSize)
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	line, _ := PublicLine(public)
	for _, fixture := range []struct {
		name string
		data map[string][]byte
	}{
		{"short-final-seed", map[string][]byte{testHostPath: seed[:31]}},
		{"long-final-seed", map[string][]byte{testHostPath: append(append([]byte(nil), seed...), 0)}},
		{"empty-final-seed", map[string][]byte{testHostPath: nil}},
		{"short-staged-seed", map[string][]byte{testHostPath + ".new": seed[:31]}},
		{"empty-staged-seed", map[string][]byte{testHostPath + ".new": nil}},
		{"mismatched-public", map[string][]byte{testHostPath: seed, testHostPath + ".pub": []byte(keyLine(t, testPublic(1)))}},
		{"malformed-public", map[string][]byte{testHostPath: seed, testHostPath + ".pub": []byte("corrupt")}},
		{"partial-public-stage", map[string][]byte{testHostPath + ".new": seed, testHostPath + ".pub.new": line[:10]}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			files := newMemoryFiles()
			for path, data := range fixture.data {
				files.files[path] = append([]byte(nil), data...)
			}
			entropy := &entropyReader{}
			store := HostStore{Files: files, Random: entropy}
			key, err := store.Load(testHostPath)
			if err == nil || key != nil || entropy.calls != 0 || files.locked {
				t.Fatalf("corruption = %v, %v; entropy=%d", key, err, entropy.calls)
			}
			for path, original := range fixture.data {
				if !bytes.Equal(files.files[path], original) {
					t.Errorf("corrupt file %s overwritten", path)
				}
			}
		})
	}
}

func TestHostKeyCorruptStaleStagingFailsClosed(t *testing.T) {
	seed := bytes.Repeat([]byte{9}, ed25519.SeedSize)
	line, _ := PublicLine(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	for _, fixture := range []struct {
		suffix string
		data   []byte
	}{
		{".new", seed[:31]}, {".new", nil}, {".new", bytes.Repeat([]byte{1}, ed25519.SeedSize)},
		{".pub.new", line[:10]}, {".pub.new", nil}, {".pub.new", []byte(keyLine(t, testPublic(1)))},
	} {
		files := newMemoryFiles()
		files.files[testHostPath] = append([]byte(nil), seed...)
		files.files[testHostPath+".pub"] = append([]byte(nil), line...)
		files.files[testHostPath+fixture.suffix] = append([]byte(nil), fixture.data...)
		entropy := &entropyReader{}
		store := HostStore{Files: files, Random: entropy}
		key, err := store.Load(testHostPath)
		if err == nil || key != nil || entropy.calls != 0 || files.locked {
			t.Fatalf("corrupt stale staging %s accepted: %v", fixture.suffix, err)
		}
		if !bytes.Equal(files.files[testHostPath], seed) || !bytes.Equal(files.files[testHostPath+".pub"], line) {
			t.Fatal("valid final host identity overwritten")
		}
	}
}

func TestHostKeyOrphanPublicNeverRegenerates(t *testing.T) {
	for _, suffix := range []string{".pub", ".pub.new"} {
		files := newMemoryFiles()
		files.files[testHostPath+suffix] = []byte(keyLine(t, testPublic(1)))
		entropy := &entropyReader{}
		store := HostStore{Files: files, Random: entropy}
		if key, err := store.Load(testHostPath); err == nil || key != nil || entropy.calls != 0 || files.locked {
			t.Fatal("orphan public key caused identity regeneration")
		}
	}
}

func TestHostKeyEveryStorageFailureIsRecoverable(t *testing.T) {
	baseline := newMemoryFiles()
	store := HostStore{Files: baseline, Random: &entropyReader{}}
	if _, err := store.Load(testHostPath); err != nil {
		t.Fatal(err)
	}
	for failure, operation := range baseline.operations {
		t.Run(fmt.Sprintf("%02d-%s", failure+1, operation), func(t *testing.T) {
			files := newMemoryFiles()
			files.failAt = failure + 1
			entropy := &entropyReader{}
			store := HostStore{Files: files, Random: entropy}
			key, err := store.Load(testHostPath)
			if !errors.Is(err, errInjected) || key != nil || files.locked {
				t.Fatalf("failure = %v, %v, locked=%v", key, err, files.locked)
			}
			generated := entropy.calls
			_, persistedFinal := files.files[testHostPath]
			_, persistedStaged := files.files[testHostPath+".new"]
			files.failAt = 0
			key, err = store.Load(testHostPath)
			if err != nil {
				t.Fatal(err)
			}
			verifyHostKey(t, files, key, bytes.Repeat([]byte{7}, ed25519.SeedSize))
			if (persistedFinal || persistedStaged) && entropy.calls != generated {
				t.Fatal("recovery regenerated a persisted identity")
			}
		})
	}
}

func TestHostKeyVerifiesStageAndFinal(t *testing.T) {
	for _, point := range []string{"staged-seed", "staged-public", "final-seed", "final-public", "seed-after-public"} {
		t.Run(point, func(t *testing.T) {
			files := newMemoryFiles()
			fired := false
			publicPublished := false
			files.mutate = func(operation string, files *memoryFiles) {
				if operation == "rename "+testHostPath+".pub.new "+testHostPath+".pub" {
					publicPublished = true
				}
				path := ""
				if !fired {
					switch point {
					case "staged-seed":
						if operation == "read "+testHostPath+".new" && len(files.files[testHostPath+".new"]) != 0 {
							path = testHostPath + ".new"
						}
					case "staged-public":
						if operation == "read "+testHostPath+".pub.new" && len(files.files[testHostPath+".pub.new"]) != 0 {
							path = testHostPath + ".pub.new"
						}
					case "final-seed":
						if operation == "read "+testHostPath && len(files.files[testHostPath]) != 0 {
							path = testHostPath
						}
					case "final-public":
						if operation == "read "+testHostPath+".pub" && len(files.files[testHostPath+".pub"]) != 0 {
							path = testHostPath + ".pub"
						}
					case "seed-after-public":
						if operation == "read "+testHostPath && publicPublished {
							path = testHostPath
						}
					}
				}
				if path != "" {
					files.files[path][0] ^= 1
					fired = true
				}
			}
			store := HostStore{Files: files, Random: &entropyReader{}}
			key, err := store.Load(testHostPath)
			if !fired || err == nil || key != nil || files.locked {
				t.Fatalf("mutation = fired %v, key %v, err %v", fired, key, err)
			}
		})
	}
}

func TestHostKeyFailureGuards(t *testing.T) {
	files := newMemoryFiles()
	entropy := &entropyReader{fail: true}
	store := HostStore{Files: files, Random: entropy}
	if key, err := store.Load(testHostPath); !errors.Is(err, errInjected) || key != nil || len(files.files) != 0 || files.locked {
		t.Fatal("entropy failure published state or retained lock")
	}
	for _, store := range []*HostStore{nil, {}} {
		if _, err := store.Load(testHostPath); err == nil {
			t.Fatal("missing store accepted")
		}
	}
	store = HostStore{Files: files, Random: io.LimitReader(bytes.NewReader([]byte{1}), 1)}
	if _, err := store.Load(testHostPath); err == nil {
		t.Fatal("short entropy accepted")
	}
	if _, err := store.Load("/state/../key"); err == nil {
		t.Fatal("noncanonical path accepted")
	}
}

func TestLoadAuthorizedKeys(t *testing.T) {
	files := newMemoryFiles()
	path := "/home/user/.ssh/authorized_keys"
	if keys, err := LoadAuthorizedKeys(files, path); !errors.Is(err, ErrNotFound) || keys.Len() != 0 {
		t.Fatal("missing keys accepted")
	}
	files.files[path] = []byte(keyLine(t, testPublic(1)))
	if keys, err := LoadAuthorizedKeys(files, path); err != nil || keys.Len() != 1 {
		t.Fatal("valid stored keys rejected")
	}
	files.files[path] = bytes.Repeat([]byte{'x'}, MaxAuthorizedKeysBytes+1)
	if keys, err := LoadAuthorizedKeys(files, path); err == nil || keys.Len() != 0 {
		t.Fatal("oversized keys accepted")
	}
	if _, err := LoadAuthorizedKeys(nil, path); err == nil {
		t.Fatal("nil reader accepted")
	}
	if _, err := LoadAuthorizedKeys(files, "relative"); err == nil {
		t.Fatal("invalid source path accepted")
	}
}
