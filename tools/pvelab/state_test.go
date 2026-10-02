package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunIDAndNames(t *testing.T) {
	now := time.Date(2026, 10, 2, 13, 5, 1, 0, time.UTC)
	a, err := NewRunID(now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewRunID(now)
	if a == b {
		t.Error("two run IDs in the same second are equal")
	}
	if !runIDPattern.MatchString(a) || !strings.HasPrefix(a, "20261002T130501Z-") {
		t.Errorf("run ID %q", a)
	}
	name := vmName(a)
	if !strings.HasPrefix(name, "goos-pvelab-") || len(name) != len("goos-pvelab-")+8 {
		t.Errorf("vm name %q", name)
	}
	if iso := isoName(a); !strings.HasSuffix(iso, ".iso") || !strings.Contains(iso, a) {
		t.Errorf("iso name %q", iso)
	}
}

func TestStateSaveLoad(t *testing.T) {
	root := t.TempDir()
	id, _ := NewRunID(time.Now())
	st, err := NewState(root, id)
	if err != nil {
		t.Fatal(err)
	}
	st.VMID, st.ISOVolume, st.RemoteStaging = 154, "Images:iso/x.iso", stagingDir(id)
	if err := st.Advance(PhaseISOUploaded); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.VMID != 154 || got.ISOVolume != "Images:iso/x.iso" || got.Phase != PhaseISOUploaded {
		t.Errorf("loaded %+v", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(st.path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestLoadStateRejectsBadRunID(t *testing.T) {
	for _, id := range []string{"../../etc", "x", "20261002T130501Z-ABCDEFGH"} {
		if _, err := LoadState(t.TempDir(), id); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestStaleRuns(t *testing.T) {
	root := t.TempDir()
	old, _ := NewRunID(time.Now())
	st, _ := NewState(root, old)
	st.Save()
	cleaned, _ := NewRunID(time.Now())
	st2, _ := NewState(root, cleaned)
	st2.Cleaned = true
	st2.Save()
	got, err := staleRuns(root, time.Hour, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RunID != old {
		t.Errorf("stale = %v", got)
	}
	if got, _ := staleRuns(root, time.Hour, time.Now()); len(got) != 0 {
		t.Errorf("fresh run counted stale: %v", got)
	}
}
