package identityd

import (
	"bytes"
	"errors"
	"sort"
	"testing"

	"goos/userspace/identity"
	"goos/userspace/identityproto"
)

const testPath = "/state/identity/db"

// testEpoch is a fixed nonzero database epoch for host tests.
var testEpoch = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

func fixedEpoch() ([16]byte, error) { return testEpoch, nil }

// memFiles is a faithful in-memory Files: exclusive create, append write,
// sync (recorded), rename, bounded directory listing.
type memFiles struct {
	files   map[string][]byte
	open    string
	unlinks int
}

func newMemFiles() *memFiles {
	return &memFiles{files: map[string][]byte{}}
}

func (f *memFiles) Read(path string) ([]byte, error) {
	data, ok := f.files[path]
	if !ok {
		return nil, ErrNotFound
	}
	return data, nil
}

func (f *memFiles) Create(path string) error {
	if _, ok := f.files[path]; ok {
		return ErrExists
	}
	f.files[path] = nil
	return nil
}

func (f *memFiles) Unlink(path string) error {
	f.unlinks++
	if _, ok := f.files[path]; !ok {
		return ErrNotFound
	}
	delete(f.files, path)
	return nil
}

func (f *memFiles) OpenWrite(path string) (Handle, error) {
	if _, ok := f.files[path]; !ok {
		return 0, ErrNotFound
	}
	f.open = path
	return 1, nil
}

func (f *memFiles) Write(_ Handle, data []byte) (int, error) {
	f.files[f.open] = append(f.files[f.open], data...)
	return len(data), nil
}

func (f *memFiles) Sync(Handle) error { return nil }

func (f *memFiles) Close(Handle) error { return nil }

func (f *memFiles) Replace(source, target string) error {
	f.files[target] = f.files[source]
	delete(f.files, source)
	return nil
}

func (f *memFiles) SyncDir(string) error { return nil }

func (f *memFiles) ListNames(path string, max int) ([]string, error) {
	dir := dirOf(path)
	var names []string
	for p := range f.files {
		if dirOf(p) == dir {
			names = append(names, baseOf(p))
		}
	}
	sort.Strings(names)
	if len(names) > max {
		return nil, errors.New("too many entries")
	}
	return names, nil
}

func snapshotFor(t *testing.T, accounts ...string) []byte {
	t.Helper()
	store := identity.NewStoreWithEpoch(testEpoch)
	for _, name := range accounts {
		if _, err := store.CreateAccount(name); err != nil {
			t.Fatal(err)
		}
	}
	data, err := identity.Encode(store)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func newTestService() *Service {
	return New(newMemFiles(), testPath, fixedEpoch)
}

func TestPersistRoundTrip(t *testing.T) {
	files := newMemFiles()
	persist := NewPersist(files, testPath)
	want := snapshotFor(t, "alice", "bob")
	if err := persist.Save(want); err != nil {
		t.Fatalf("Save = %v", err)
	}
	loaded, err := persist.Open()
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	got, _ := identity.Encode(loaded)
	if !bytes.Equal(want, got) {
		t.Fatal("round-trip snapshot differs")
	}
	if len(files.files) != 1 {
		t.Fatalf("temporary files leaked: %d entries", len(files.files))
	}
}

func TestPersistCorruptLoad(t *testing.T) {
	files := newMemFiles()
	files.files[testPath] = []byte("not a snapshot")
	persist := NewPersist(files, testPath)
	if _, err := persist.Open(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open corrupt = %v, want ErrCorrupt", err)
	}
	service := New(files, testPath, fixedEpoch)
	if err := service.Open(); err == nil {
		t.Fatal("Service.Open corrupt = nil")
	}
	if service.State() != identityproto.StateUnavailable {
		t.Fatalf("corrupt state = %v, want Unavailable", service.State())
	}
}

func TestPersistMissingLoad(t *testing.T) {
	files := newMemFiles()
	persist := NewPersist(files, testPath)
	if _, err := persist.Open(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open missing = %v, want ErrNotFound", err)
	}
	service := New(files, testPath, fixedEpoch)
	if err := service.Open(); err == nil {
		t.Fatal("Service.Open missing = nil")
	}
	if service.State() != identityproto.StateUnprovisioned {
		t.Fatalf("missing state = %v, want Unprovisioned", service.State())
	}
}

func TestPersistStaleTempReconcile(t *testing.T) {
	t.Run("db present stale temp removed", func(t *testing.T) {
		files := newMemFiles()
		files.files[testPath] = snapshotFor(t, "alice")
		files.files[NewPersist(files, testPath).tempPath()] = []byte("partial")
		persist := NewPersist(files, testPath)
		store, err := persist.Open()
		if err != nil {
			t.Fatalf("Open = %v", err)
		}
		if store == nil {
			t.Fatal("Open = nil store")
		}
		if _, ok := files.files[persist.tempPath()]; ok {
			t.Fatal("stale temp not removed")
		}
		if len(files.files) != 1 {
			t.Fatalf("expected only db, got %d entries", len(files.files))
		}
	})

	t.Run("db absent stale temp unavailable", func(t *testing.T) {
		files := newMemFiles()
		persist := NewPersist(files, testPath)
		files.files[persist.tempPath()] = []byte("partial")
		if _, err := persist.Open(); !errors.Is(err, ErrUncertain) {
			t.Fatalf("Open = %v, want ErrUncertain (interrupted provisioning)", err)
		}
		service := New(files, testPath, fixedEpoch)
		if err := service.Open(); err == nil {
			t.Fatal("Service.Open = nil, want error")
		}
		if service.State() != identityproto.StateUnavailable {
			t.Fatalf("state = %v, want Unavailable", service.State())
		}
	})

	t.Run("stale temp does not block future save", func(t *testing.T) {
		files := newMemFiles()
		persist := NewPersist(files, testPath)
		files.files[persist.tempPath()] = []byte("partial")
		want := snapshotFor(t, "alice")
		if err := persist.Save(want); err != nil {
			t.Fatalf("Save over stale temp = %v", err)
		}
		if _, ok := files.files[persist.tempPath()]; ok {
			t.Fatal("stale temp still present after save")
		}
		got := files.files[testPath]
		if !bytes.Equal(got, want) {
			t.Fatal("save did not write the target")
		}
	})
}

func TestPersistFaultInjection(t *testing.T) {
	oldData := snapshotFor(t, "alice")
	newData := snapshotFor(t, "alice", "bob")

	tests := []struct {
		name           string
		fault          func(*Persist)
		wantUncertain  bool
		wantNewInPlace bool
		wantTempKept   bool
	}{
		{
			name: "before-temp-write",
			fault: func(p *Persist) {
				p.Faults.BeforeTempWrite = func() error { return errors.New("injected before temp write") }
			},
		},
		{
			name: "after-temp-write",
			fault: func(p *Persist) {
				p.Faults.AfterTempWrite = func() error { return errors.New("injected after temp write") }
			},
		},
		{
			name: "before-replace",
			fault: func(p *Persist) {
				p.Faults.BeforeReplace = func() error { return errors.New("injected before replace") }
			},
		},
		{
			name: "temp-sync",
			fault: func(p *Persist) {
				p.Faults.SyncErr = func() error { return errors.New("injected temp sync failure") }
			},
		},
		{
			name: "replace-uncertain",
			fault: func(p *Persist) {
				p.Faults.ReplaceErr = func() error { return ErrUncertain }
			},
			wantUncertain: true,
			wantTempKept:  true,
		},
		{
			name: "after-replace",
			fault: func(p *Persist) {
				p.Faults.AfterReplace = func() error { return errors.New("injected after replace") }
			},
			wantUncertain:  true,
			wantNewInPlace: true,
		},
		{
			name: "dir-sync",
			fault: func(p *Persist) {
				p.Faults.SyncDirErr = func() error { return errors.New("injected dir sync failure") }
			},
			wantUncertain:  true,
			wantNewInPlace: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := newMemFiles()
			files.files[testPath] = oldData
			persist := NewPersist(files, testPath)
			test.fault(persist)

			err := persist.Save(newData)
			if err == nil {
				t.Fatal("Save = nil, want error")
			}
			if test.wantUncertain && !errors.Is(err, ErrUncertain) {
				t.Fatalf("Save = %v, want ErrUncertain", err)
			}
			if !test.wantUncertain && errors.Is(err, ErrUncertain) {
				t.Fatalf("Save = %v, want a definite pre-replace failure", err)
			}

			current := files.files[testPath]
			if test.wantNewInPlace {
				if !bytes.Equal(current, snapshotFor(t, "alice", "bob")) {
					t.Fatal("target does not hold the new snapshot after a committed rename")
				}
			} else if !bytes.Equal(current, oldData) {
				t.Fatal("target does not hold the old snapshot after a pre-replace failure")
			}
			if test.wantTempKept {
				if _, ok := files.files[persist.tempPath()]; !ok {
					t.Fatal("temporary file should be kept on an uncertain outcome")
				}
			} else if len(files.files) != 1 {
				t.Fatalf("temporary file leaked: %d entries", len(files.files))
			}
		})
	}
}

func TestPersistReadOversized(t *testing.T) {
	files := newMemFiles()
	files.files[testPath] = bytes.Repeat([]byte{0xaa}, identity.MaxSnapshotBytes+1)
	persist := NewPersist(files, testPath)
	if _, err := persist.Open(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open oversized = %v, want ErrCorrupt", err)
	}
}

func TestPersistCleanFailureKeepsOldAndNoLeak(t *testing.T) {
	oldData := snapshotFor(t, "alice")
	files := newMemFiles()
	files.files[testPath] = oldData
	persist := NewPersist(files, testPath)
	persist.Faults.BeforeReplace = func() error { return errors.New("fail") }

	if err := persist.Save(snapshotFor(t, "alice", "bob")); err == nil {
		t.Fatal("Save = nil, want error")
	}
	if len(files.files) != 1 {
		t.Fatalf("temporary file leaked: %d entries", len(files.files))
	}
	if !bytes.Equal(files.files[testPath], oldData) {
		t.Fatal("target changed on a clean failure")
	}
}

func TestPersistUncertainReplaceKeepsTemp(t *testing.T) {
	files := newMemFiles()
	files.files[testPath] = snapshotFor(t, "alice")
	persist := NewPersist(files, testPath)
	persist.Faults.ReplaceErr = func() error { return ErrUncertain }

	if err := persist.Save(snapshotFor(t, "alice", "bob")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("Save = %v, want ErrUncertain", err)
	}
	// The uncertain replace outcome must not mutate the filesystem further:
	// no Unlink on the temporary, so it is still present.
	if files.unlinks != 0 {
		t.Fatalf("Unlink called %d times on the uncertain path, want 0", files.unlinks)
	}
	if _, ok := files.files[persist.tempPath()]; !ok {
		t.Fatal("temporary file was removed on the uncertain path")
	}
	// The fake Replace never ran, so the old snapshot is intact.
	if !bytes.Equal(files.files[testPath], snapshotFor(t, "alice")) {
		t.Fatal("target changed on an uncertain replace")
	}
}

func TestPersistOpenBarrierFailsClosed(t *testing.T) {
	files := newMemFiles()
	files.files[testPath] = snapshotFor(t, "alice")

	persist := NewPersist(files, testPath)
	persist.Faults.SyncDirErr = func() error { return errors.New("injected barrier failure") }
	if _, err := persist.Open(); err == nil {
		t.Fatal("Open = nil, want durability barrier failure")
	}

	service := New(files, testPath, fixedEpoch)
	service.persist.Faults.SyncDirErr = func() error { return errors.New("injected barrier failure") }
	if err := service.Open(); err == nil {
		t.Fatal("Service.Open = nil, want durability barrier failure")
	}
	if service.State() != identityproto.StateUnavailable {
		t.Fatalf("state = %v, want Unavailable", service.State())
	}
}
