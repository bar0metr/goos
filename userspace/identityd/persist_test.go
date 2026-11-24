package identityd

import (
	"bytes"
	"errors"
	"testing"

	"goos/userspace/identity"
	"goos/userspace/identityproto"
)

const testPath = "/state/identity/db"

// memFiles is a faithful in-memory Files: exclusive create, append write,
// and a rename that moves the temporary onto the target.
type memFiles struct {
	files map[string][]byte
	open  string
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
		return errors.New("identityd: already exists")
	}
	f.files[path] = nil
	return nil
}

func (f *memFiles) Unlink(path string) error {
	delete(f.files, path)
	return nil
}

func (f *memFiles) OpenWrite(path string) (Handle, error) {
	f.open = path
	return 1, nil
}

func (f *memFiles) Write(_ Handle, data []byte) (int, error) {
	f.files[f.open] = append(f.files[f.open], data...)
	return len(data), nil
}

func (f *memFiles) Close(Handle) error { return nil }

func (f *memFiles) Replace(source, target string) error {
	f.files[target] = f.files[source]
	delete(f.files, source)
	return nil
}

func snapshotFor(t *testing.T, accounts ...string) []byte {
	t.Helper()
	store := identity.NewStore()
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

func TestPersistRoundTrip(t *testing.T) {
	files := newMemFiles()
	persist := NewPersist(files, testPath)
	want := snapshotFor(t, "alice", "bob")
	if err := persist.Save(want); err != nil {
		t.Fatalf("Save = %v", err)
	}
	loaded, err := persist.Load()
	if err != nil {
		t.Fatalf("Load = %v", err)
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
	if _, err := persist.Load(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Load corrupt = %v, want ErrCorrupt", err)
	}
	// The same bytes drive the service to Unavailable.
	service := New()
	if err := service.Load([]byte("not a snapshot")); err == nil {
		t.Fatal("Service.Load corrupt = nil")
	}
	if service.State() != identityproto.StateUnavailable {
		t.Fatalf("corrupt state = %v, want Unavailable", service.State())
	}
}

func TestPersistMissingLoad(t *testing.T) {
	files := newMemFiles()
	persist := NewPersist(files, testPath)
	if _, err := persist.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load missing = %v, want ErrNotFound", err)
	}
	service := New()
	if err := service.Load(nil); err != nil {
		t.Fatalf("Service.Load(nil) = %v", err)
	}
	if service.State() != identityproto.StateUnprovisioned {
		t.Fatalf("missing state = %v, want Unprovisioned", service.State())
	}
}

func TestPersistFaultInjection(t *testing.T) {
	oldData := snapshotFor(t, "alice")
	newData := snapshotFor(t, "alice", "bob")

	tests := []struct {
		name           string
		fault          func(*Persist)
		wantUncertain  bool
		wantNewInPlace bool
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
			name: "after-replace",
			fault: func(p *Persist) {
				p.Faults.AfterReplace = func() error { return errors.New("injected after replace") }
			},
			wantUncertain:  true,
			wantNewInPlace: true,
		},
		{
			name: "replace-uncertain",
			fault: func(p *Persist) {
				p.Faults.ReplaceErr = func() error { return ErrUncertain }
			},
			wantUncertain: true,
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

			// The target must hold one whole snapshot, old or new, never a
			// partial or corrupt one.
			current := files.files[testPath]
			if test.wantNewInPlace {
				if !bytes.Equal(current, snapshotFor(t, "alice", "bob")) {
					t.Fatal("target does not hold the new snapshot after a committed rename")
				}
			} else if !bytes.Equal(current, oldData) {
				t.Fatal("target does not hold the old snapshot after a pre-replace failure")
			}

			// Reconciliation forward re-reads and validates whatever survived.
			loaded, loadErr := persist.Load()
			if loadErr != nil {
				t.Fatalf("reconcile Load = %v, want a valid snapshot", loadErr)
			}
			reencoded, _ := identity.Encode(loaded)
			if !bytes.Equal(reencoded, current) {
				t.Fatal("reconciled store does not match the stored snapshot")
			}
		})
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
