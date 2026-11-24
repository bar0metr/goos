package identityd

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"goos/kernel/ramfs"
	"goos/userspace/fat32"
	"goos/userspace/identity"
	"goos/userspace/identityproto"
)

// fat32Path is the snapshot path the coordinator uses against the flat root
// directory of the real fat32 volume under test. It deliberately has no slash,
// so the adapter below is a single flat directory: every identityd path maps to
// a root-directory entry by base name.
const fat32Path = "db"

// fat32Device is the block device a real fat32.FileSystem reads and writes
// through. It is the same deterministic host fault-injection primitive fat32's
// own crash tests use (the test-private memoryDevice): reads, writes and
// flushes are counted, and a nonzero fail threshold makes that operation and
// every later one fail, as a device that has gone away or a power cut would.
// It is restated here only because memoryDevice is test-private to package
// fat32; the mechanism and semantics are identical, and it injects at the real
// device boundary (ReadAt/WriteAt/Flush), never inside identityd.
type fat32Device struct {
	data                    []byte
	reads, writes, flushes  int
	failReadAt, failWriteAt int
	failFlushAt             int
}

func (d *fat32Device) ReadAt(offset uint64, target []byte) bool {
	d.reads++
	if d.failReadAt != 0 && d.reads >= d.failReadAt {
		return false
	}
	return offset+uint64(len(target)) <= uint64(len(d.data)) && copy(target, d.data[offset:]) == len(target)
}

func (d *fat32Device) WriteAt(offset uint64, data []byte) bool {
	d.writes++
	if d.failWriteAt != 0 && d.writes >= d.failWriteAt {
		return false
	}
	return offset+uint64(len(data)) <= uint64(len(d.data)) && copy(d.data[offset:], data) == len(data)
}

func (d *fat32Device) Flush() bool {
	d.flushes++
	if d.failFlushAt != 0 && d.flushes >= d.failFlushAt {
		return false
	}
	return true
}

func (d *fat32Device) Capacity() uint64 { return uint64(len(d.data)) }

// openFile is one open whole-file write handle: the fat32 node being written,
// its open-object cookie (so Close is a real ReleaseObject), and the write
// cursor identityd.Files.Write advances (the Files surface has no offset).
type openFile struct {
	node   ramfs.NodeID
	object ramfs.NodeID
	offset uint64
}

// fat32Files adapts a real fat32.FileSystem to the identityd.Files surface over
// the actual directory/file/replace/sync operations. It is the smallest honest
// adapter over the fat32 public surface. The surface fat32 cannot express is
// documented at each method: Sync and SyncDir both flush the whole device
// (fat32 has no per-file or per-directory fsync distinct from the volume
// flush, and Write already syncs), and Close releases the open-object cookie
// (fat32 tracks no other per-handle close state).
type fat32Files struct {
	fs   *fat32.FileSystem
	root ramfs.NodeID
	open map[Handle]*openFile
	next Handle
}

func newFat32Files(fs *fat32.FileSystem, root ramfs.NodeID) *fat32Files {
	return &fat32Files{fs: fs, root: root, open: map[Handle]*openFile{}, next: 1}
}

func fat32Name(path string) (ramfs.Name, error) {
	name, ok := ramfs.MakeName([]byte(baseOf(path)))
	if !ok {
		return ramfs.Name{}, fmt.Errorf("identityd: invalid fat32 name %q", path)
	}
	return name, nil
}

// mapStatus maps a fat32 status to the identityd.Files sentinels. Only the two
// sentinels the coordinator's contract depends on are preserved; everything
// else is a typed error.
func mapStatus(status ramfs.Status) error {
	switch status {
	case ramfs.StatusOK:
		return nil
	case ramfs.StatusNotFound:
		return ErrNotFound
	case ramfs.StatusAlreadyExists:
		return ErrExists
	default:
		return fmt.Errorf("identityd: fat32 status %d", status)
	}
}

func (f *fat32Files) Read(path string) ([]byte, error) {
	name, err := fat32Name(path)
	if err != nil {
		return nil, err
	}
	id, status := f.fs.OpenName(f.root, name)
	if status != ramfs.StatusOK {
		return nil, mapStatus(status)
	}
	stat, status := f.fs.Stat(id)
	if status != ramfs.StatusOK {
		return nil, mapStatus(status)
	}
	if stat.Size > identity.MaxSnapshotBytes {
		return nil, fmt.Errorf("%w: snapshot exceeds %d bytes", ErrCorrupt, identity.MaxSnapshotBytes)
	}
	data := make([]byte, stat.Size)
	count, status := f.fs.Read(id, 0, data)
	if status != ramfs.StatusOK {
		return nil, mapStatus(status)
	}
	return data[:count], nil
}

func (f *fat32Files) Create(path string) error {
	name, err := fat32Name(path)
	if err != nil {
		return err
	}
	_, status := f.fs.CreateName(f.root, name)
	return mapStatus(status)
}

func (f *fat32Files) Unlink(path string) error {
	name, err := fat32Name(path)
	if err != nil {
		return err
	}
	return mapStatus(f.fs.UnlinkName(f.root, name))
}

func (f *fat32Files) OpenWrite(path string) (Handle, error) {
	name, err := fat32Name(path)
	if err != nil {
		return 0, err
	}
	id, status := f.fs.OpenName(f.root, name)
	if status != ramfs.StatusOK {
		return 0, mapStatus(status)
	}
	object, status := f.fs.AcquireObject(id)
	if status != ramfs.StatusOK {
		return 0, mapStatus(status)
	}
	handle := f.next
	f.next++
	f.open[handle] = &openFile{node: id, object: object}
	return handle, nil
}

func (f *fat32Files) Write(h Handle, data []byte) (int, error) {
	file, ok := f.open[h]
	if !ok {
		return 0, errors.New("identityd: bad handle")
	}
	count, status := f.fs.Write(file.node, file.offset, data)
	if status != ramfs.StatusOK {
		return 0, mapStatus(status)
	}
	file.offset += uint64(count)
	return count, nil
}

// Sync flushes the whole device: fat32 has no per-file fsync, and Write already
// synced the device, so this is the real (idempotent) volume barrier.
func (f *fat32Files) Sync(Handle) error {
	if !f.fs.Flush() {
		return errors.New("identityd: fat32 sync failed")
	}
	return nil
}

func (f *fat32Files) Close(h Handle) error {
	file, ok := f.open[h]
	if !ok {
		return errors.New("identityd: bad handle")
	}
	if _, status := f.fs.ReleaseObject(file.object); status != ramfs.StatusOK {
		return mapStatus(status)
	}
	delete(f.open, h)
	return nil
}

// Replace resolves the source by name and performs the real fat32.ReplaceName
// (the 4-flush rename). ReplaceName reports only a status, never how far it
// got: a device failure (StatusIOError) or corrupt metadata may have committed
// the rewrite or stopped before it, so both map to ErrUncertain. Refusals
// (NotFound/AlreadyExists/...) leave the target unchanged and stay definite.
func (f *fat32Files) Replace(source, target string) error {
	sourceName, err := fat32Name(source)
	if err != nil {
		return err
	}
	targetName, err := fat32Name(target)
	if err != nil {
		return err
	}
	id, status := f.fs.OpenName(f.root, sourceName)
	if status != ramfs.StatusOK {
		return mapStatus(status)
	}
	switch status = f.fs.ReplaceName(id, f.root, targetName); status {
	case ramfs.StatusOK:
		return nil
	case ramfs.StatusIOError, ramfs.StatusCorrupt:
		return ErrUncertain
	default:
		return mapStatus(status)
	}
}

// SyncDir flushes the whole device: the fat32 directory barrier is the same
// volume flush, because directory entries and FAT links are written through and
// ReplaceName already ends in a device flush.
func (f *fat32Files) SyncDir(string) error {
	if !f.fs.Flush() {
		return errors.New("identityd: fat32 directory sync failed")
	}
	return nil
}

func (f *fat32Files) ListNames(_ string, max int) ([]string, error) {
	var names []string
	for cursor := 0; ; {
		entry, next, status := f.fs.ReadDir(f.root, cursor)
		if status == ramfs.StatusNotFound {
			return names, nil
		}
		if status != ramfs.StatusOK {
			return nil, mapStatus(status)
		}
		names = append(names, string(entry.Name[:entry.Size]))
		if len(names) > max {
			return nil, fmt.Errorf("identityd: directory has more than %d entries", max)
		}
		cursor = next
	}
}

// newFat32Fixture formats and mounts a real, empty FAT32 volume on an in-memory
// device and returns the device, filesystem, root, and the adapter over it.
func newFat32Fixture(t *testing.T) (*fat32Device, *fat32.FileSystem, ramfs.NodeID, *fat32Files) {
	t.Helper()
	device := &fat32Device{data: make([]byte, 8<<20)}
	if !fat32.Format(device, fat32.Layout{SectorsPerCluster: 1, FATs: 1}) {
		t.Fatal("fat32 format failed")
	}
	fs := &fat32.FileSystem{}
	root, ok := fs.Mount(device)
	if !ok {
		t.Fatalf("fat32 mount failed: %v", fs.Refused())
	}
	return device, fs, root, newFat32Files(fs, root)
}

// reopen clears the injected faults and mounts a fresh FileSystem over the same
// device bytes, so a test reads exactly what the faulted operation left on
// disk — the "restart and reopen the same image, do not recreate it" step.
func reopen(t *testing.T, d *fat32Device) (*fat32.FileSystem, ramfs.NodeID, *fat32Files, *Persist) {
	t.Helper()
	d.failReadAt, d.failWriteAt, d.failFlushAt = 0, 0, 0
	fs := &fat32.FileSystem{}
	root, ok := fs.Mount(d)
	if !ok {
		t.Fatalf("remount failed: %v", fs.Refused())
	}
	files := newFat32Files(fs, root)
	return fs, root, files, NewPersist(files, fat32Path)
}

// readNamedBytes reads a file's raw bytes straight from the fat32 volume.
func readNamedBytes(t *testing.T, fs *fat32.FileSystem, root ramfs.NodeID, path string) ([]byte, error) {
	t.Helper()
	name, ok := ramfs.MakeName([]byte(baseOf(path)))
	if !ok {
		return nil, fmt.Errorf("identityd: invalid fat32 name %q", path)
	}
	id, status := fs.OpenName(root, name)
	if status != ramfs.StatusOK {
		return nil, mapStatus(status)
	}
	stat, status := fs.Stat(id)
	if status != ramfs.StatusOK {
		return nil, mapStatus(status)
	}
	data := make([]byte, stat.Size)
	count, status := fs.Read(id, 0, data)
	if status != ramfs.StatusOK {
		return nil, mapStatus(status)
	}
	return data[:count], nil
}

// dbOutcome classifies the on-disk snapshot after a faulted save.
type dbOutcome int

const (
	dbOld dbOutcome = iota
	dbNew
	dbMissing
	dbTorn
)

func classifyDB(t *testing.T, fs *fat32.FileSystem, root ramfs.NodeID, oldData, newData []byte) dbOutcome {
	t.Helper()
	data, err := readNamedBytes(t, fs, root, fat32Path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return dbMissing
		}
		return dbTorn
	}
	switch {
	case bytes.Equal(data, oldData):
		return dbOld
	case bytes.Equal(data, newData):
		return dbNew
	default:
		return dbTorn
	}
}

func TestFat32PersistRoundTrip(t *testing.T) {
	device, _, _, files := newFat32Fixture(t)
	persist := NewPersist(files, fat32Path)
	want := snapshotFor(t, "alice")
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

	// Restart over the same image, never recreating it: the durable snapshot
	// must survive.
	fs, root, rfiles, rpersist := reopen(t, device)
	data, err := readNamedBytes(t, fs, root, fat32Path)
	if err != nil || !bytes.Equal(data, want) {
		t.Fatalf("durable snapshot lost on restart: %v", err)
	}
	loaded, err = rpersist.Open()
	if err != nil {
		t.Fatalf("reopen = %v", err)
	}
	if _, err := identity.Encode(loaded); err != nil {
		t.Fatal(err)
	}
	_ = rfiles
}

// TestFat32PersistServiceRevocationSurvivesRestart drives the full Service
// commit path against the real fat32 volume: provision, add a key, then remove
// the key and disable the account, and confirm a restart over the same image
// sees the revocation — the "after the final durability barrier, before the
// reply" case where the reply is lost but the mutation is durable.
func TestFat32PersistServiceRevocationSurvivesRestart(t *testing.T) {
	device, _, _, files := newFat32Fixture(t)
	svc := New(files, fat32Path, fixedEpoch)
	if err := svc.Open(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open = %v, want ErrNotFound (unprovisioned)", err)
	}
	if svc.State() != identityproto.StateUnprovisioned {
		t.Fatalf("initial state = %v, want Unprovisioned", svc.State())
	}
	mustProvision(t, svc)
	if r := svc.HandleRequest(named(identityproto.AdminAddAccount, "alice")); r.Result != identityproto.ResultOK {
		t.Fatalf("AddAccount = %v", r.Result)
	}
	addKey := &identityproto.Request{Opcode: identityproto.AdminAddKey, Name: []byte("alice"), Key: key(1)}
	if r := svc.HandleRequest(addKey); r.Result != identityproto.ResultOK {
		t.Fatalf("AddKey = %v", r.Result)
	}
	if ok, res := svc.AuthorizeKey("alice", key(1)); !ok || res != identityproto.ResultOK {
		t.Fatalf("AuthorizeKey before restart = %v,%v, want true,OK", ok, res)
	}

	// Restart: the durable snapshot still authorizes.
	_, _, rfiles, _ := reopen(t, device)
	svc2 := New(rfiles, fat32Path, fixedEpoch)
	if err := svc2.Open(); err != nil {
		t.Fatalf("reopen Open = %v", err)
	}
	if svc2.State() != identityproto.StateAvailable {
		t.Fatalf("reopened state = %v, want Available", svc2.State())
	}
	if ok, res := svc2.AuthorizeKey("alice", key(1)); !ok || res != identityproto.ResultOK {
		t.Fatalf("AuthorizeKey after restart = %v,%v, want true,OK", ok, res)
	}

	// Revoke: remove the key and disable, each a durable committed mutation.
	removeKey := &identityproto.Request{Opcode: identityproto.AdminRemoveKey, Name: []byte("alice"), Fingerprint: fingerprintOf(key(1))}
	if r := svc2.HandleRequest(removeKey); r.Result != identityproto.ResultOK {
		t.Fatalf("RemoveKey = %v", r.Result)
	}
	disable := &identityproto.Request{Opcode: identityproto.AdminSetEnabled, Name: []byte("alice"), Enabled: false}
	if r := svc2.HandleRequest(disable); r.Result != identityproto.ResultOK {
		t.Fatalf("SetEnabled(false) = %v", r.Result)
	}
	if accounts, _, keys := svc2.Counts(); accounts != 1 || keys != 0 {
		t.Fatalf("counts after revoke = %d,%d, want 1 account, 0 keys", accounts, keys)
	}

	// The reply above is "lost": restart again and confirm the revocation is
	// durable, with no replay and no fallback to the old authorized view.
	_, _, rfiles2, _ := reopen(t, device)
	svc3 := New(rfiles2, fat32Path, fixedEpoch)
	if err := svc3.Open(); err != nil {
		t.Fatalf("reopen Open = %v", err)
	}
	if ok, res := svc3.AuthorizeKey("alice", key(1)); ok || res != identityproto.ResultDenied {
		t.Fatalf("AuthorizeKey after revocation restart = %v,%v, want false,Denied", ok, res)
	}
}

func fingerprintOf(k []byte) []byte {
	var key identity.Key
	copy(key[:], k)
	fp := identity.Fingerprint(key)
	return fp[:]
}

// TestFat32PersistFaultHooks exercises the deterministic identityd.Faults hooks
// against the real fat32 volume. These are the logical pre/post-replace
// boundaries; the on-device boundaries (which the hooks cannot reach) are cut
// by the flush/write tests below.
func TestFat32PersistFaultHooks(t *testing.T) {
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
				p.Faults.BeforeTempWrite = func() error { return errors.New("injected") }
			},
		},
		{
			name: "temp-sync",
			fault: func(p *Persist) {
				p.Faults.SyncErr = func() error { return errors.New("injected") }
			},
		},
		{
			name: "before-replace",
			fault: func(p *Persist) {
				p.Faults.BeforeReplace = func() error { return errors.New("injected") }
			},
		},
		{
			name: "replace-uncertain",
			fault: func(p *Persist) {
				p.Faults.ReplaceErr = func() error { return ErrUncertain }
			},
			wantUncertain: true,
		},
		{
			name: "after-replace",
			fault: func(p *Persist) {
				p.Faults.AfterReplace = func() error { return errors.New("injected") }
			},
			wantUncertain:  true,
			wantNewInPlace: true,
		},
		{
			name: "dir-sync",
			fault: func(p *Persist) {
				p.Faults.SyncDirErr = func() error { return errors.New("injected") }
			},
			wantUncertain:  true,
			wantNewInPlace: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, fs, root, files := newFat32Fixture(t)
			persist := NewPersist(files, fat32Path)
			if err := persist.Save(oldData); err != nil {
				t.Fatal(err)
			}
			test.fault(persist)
			if err := persist.Save(newData); err == nil {
				t.Fatal("Save = nil, want error")
			} else if test.wantUncertain && !errors.Is(err, ErrUncertain) {
				t.Fatalf("Save = %v, want ErrUncertain", err)
			} else if !test.wantUncertain && errors.Is(err, ErrUncertain) {
				t.Fatalf("Save = %v, want definite pre-replace failure", err)
			}

			outcome := classifyDB(t, fs, root, oldData, newData)
			if test.wantNewInPlace {
				if outcome != dbNew {
					t.Fatalf("target = %v, want new after committed rename", outcome)
				}
			} else if outcome != dbOld {
				t.Fatalf("target = %v, want old after pre-replace failure", outcome)
			}
		})
	}
}

// TestFat32PersistPartialWriteFault cuts the temp write at its first device
// write, modelling a short/partial temp write, and requires the old snapshot to
// survive and no temp to leak.
func TestFat32PersistPartialWriteFault(t *testing.T) {
	oldData := snapshotFor(t, "alice")
	newData := snapshotFor(t, "alice", "bob")

	device, _, _, files := newFat32Fixture(t)
	persist := NewPersist(files, fat32Path)
	if err := persist.Save(oldData); err != nil {
		t.Fatal(err)
	}
	// The BeforeTempWrite marker runs after the temp is created and before its
	// contents are written; arming the device there fails the first write of
	// the temp write, so the temp is only partially written.
	persist.Faults.BeforeTempWrite = func() error {
		device.failWriteAt = device.writes + 1
		return nil
	}
	if err := persist.Save(newData); err == nil {
		t.Fatal("Save = nil, want short-write error")
	} else if errors.Is(err, ErrUncertain) {
		t.Fatalf("Save = %v, want definite pre-replace failure", err)
	}

	fs, root, _, _ := reopen(t, device)
	if outcome := classifyDB(t, fs, root, oldData, newData); outcome != dbOld {
		t.Fatalf("target = %v, want old after partial temp write", outcome)
	}
}

// TestFat32PersistFlushFaultBoundaries cuts every device flush a save performs
// and requires the identityd classification and the on-disk snapshot to agree:
// a definite (non-uncertain) save error must leave the old snapshot, an
// uncertain one must leave either the old or the new snapshot, and the target
// is never torn or missing. The device flush is the real durability barrier
// fat32's sync uses, so failing the Nth flush is a deterministic boundary cut
// (no sleeps): create sync, temp-write sync, explicit temp sync, the four
// ReplaceName syncs, and the final directory sync.
func TestFat32PersistFlushFaultBoundaries(t *testing.T) {
	oldData := snapshotFor(t, "alice")
	newData := snapshotFor(t, "alice", "bob")

	// Measure how many device flushes one successful mutation save performs.
	device, _, _, files := newFat32Fixture(t)
	persist := NewPersist(files, fat32Path)
	if err := persist.Save(oldData); err != nil {
		t.Fatal(err)
	}
	start := device.flushes
	if err := persist.Save(newData); err != nil {
		t.Fatal(err)
	}
	total := device.flushes - start
	if total == 0 {
		t.Fatal("save performed no device flushes")
	}

	sawOld, sawNew := false, false
	for cut := 1; cut <= total; cut++ {
		cut := cut
		t.Run(fmt.Sprintf("flush-%d", cut), func(t *testing.T) {
			device, _, _, files := newFat32Fixture(t)
			persist := NewPersist(files, fat32Path)
			if err := persist.Save(oldData); err != nil {
				t.Fatal(err)
			}
			device.failFlushAt = device.flushes + cut
			err := persist.Save(newData)
			if err == nil {
				t.Fatalf("Save = nil, want error (flush %d)", cut)
			}
			uncertain := errors.Is(err, ErrUncertain)

			fs, root, _, _ := reopen(t, device)
			outcome := classifyDB(t, fs, root, oldData, newData)
			switch outcome {
			case dbOld:
				sawOld = true
			case dbNew:
				sawNew = true
			case dbMissing:
				t.Fatalf("flush %d: snapshot vanished", cut)
			case dbTorn:
				t.Fatalf("flush %d: snapshot torn (neither old nor new)", cut)
			}
			if !uncertain && outcome != dbOld {
				t.Fatalf("flush %d: definite error but snapshot is %v, want old", cut, outcome)
			}
		})
	}
	if !sawOld || !sawNew {
		t.Fatalf("flush cuts observed old=%v new=%v, want both", sawOld, sawNew)
	}
}

// fat32IssueCodes collects fat32.Check issues.
type fat32IssueCodes []fat32.CheckCode

func (c *fat32IssueCodes) Issue(issue fat32.CheckIssue) { *c = append(*c, issue.Code) }

// TestFat32PersistReplaceInterruptionAtEveryWrite cuts every device write the
// real 4-flush fat32.ReplaceName performs (the "during the replace" on-disk
// steps) and mounts what is left: the snapshot must be all old or all new,
// never torn, and the volume must have at worst lost clusters — never a chain
// two entries share. The write cut is armed through the deterministic
// BeforeReplace boundary marker, so it applies only to ReplaceName's writes.
func TestFat32PersistReplaceInterruptionAtEveryWrite(t *testing.T) {
	oldData := snapshotFor(t, "alice")
	newData := snapshotFor(t, "alice", "bob")

	// Measure how many device writes the replace phase alone performs.
	device, _, _, files := newFat32Fixture(t)
	persist := NewPersist(files, fat32Path)
	if err := persist.Save(oldData); err != nil {
		t.Fatal(err)
	}
	var replaceStart, replaceEnd int
	persist.Faults.BeforeReplace = func() error { replaceStart = device.writes; return nil }
	if err := persist.Save(newData); err != nil {
		t.Fatal(err)
	}
	replaceEnd = device.writes
	total := replaceEnd - replaceStart
	if total == 0 {
		t.Fatal("replace wrote nothing")
	}

	for cut := 1; cut <= total; cut++ {
		cut := cut
		t.Run(fmt.Sprintf("write-%d", cut), func(t *testing.T) {
			device, _, _, files := newFat32Fixture(t)
			persist := NewPersist(files, fat32Path)
			if err := persist.Save(oldData); err != nil {
				t.Fatal(err)
			}
			persist.Faults.BeforeReplace = func() error {
				device.failWriteAt = device.writes + cut
				return nil
			}
			if err := persist.Save(newData); err == nil {
				t.Fatalf("write %d: Save = nil, want error", cut)
			}

			fs, root, _, _ := reopen(t, device)
			switch classifyDB(t, fs, root, oldData, newData) {
			case dbOld, dbNew:
			case dbMissing:
				t.Fatalf("write %d: snapshot vanished", cut)
			case dbTorn:
				t.Fatalf("write %d: snapshot torn (neither old nor new)", cut)
			}

			var codes fat32IssueCodes
			fat32.Check(device, &codes)
			for _, code := range codes {
				if code != fat32.CheckLostCluster && code != fat32.CheckFSInfo && code != fat32.CheckLFN {
					t.Fatalf("write %d: check found %v", cut, codes)
				}
			}
		})
	}
}

// TestFat32PersistStaleTempCorruptRead covers the §4.3 recovery states against
// the real volume: a stale temp beside a valid snapshot is removed, a stale
// temp with no snapshot fails closed, a corrupt snapshot is Unavailable, and a
// read failure fails closed — never a fallback or silent reset.
func TestFat32PersistStaleTempCorruptRead(t *testing.T) {
	t.Run("stale temp with valid db reconciled", func(t *testing.T) {
		_, fs, root, files := newFat32Fixture(t)
		persist := NewPersist(files, fat32Path)
		if err := persist.Save(snapshotFor(t, "alice")); err != nil {
			t.Fatal(err)
		}
		tempName, _ := ramfs.MakeName([]byte(baseOf(persist.tempPath())))
		if _, status := fs.CreateName(root, tempName); status != ramfs.StatusOK {
			t.Fatal(status)
		}
		store, err := persist.Open()
		if err != nil {
			t.Fatalf("Open = %v", err)
		}
		if store == nil {
			t.Fatal("Open = nil store")
		}
		// The stale temp must be gone after reconciliation.
		if _, err := readNamedBytes(t, fs, root, persist.tempPath()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("stale temp not removed: %v", err)
		}
	})

	t.Run("stale temp without db unavailable", func(t *testing.T) {
		_, fs, root, files := newFat32Fixture(t)
		persist := NewPersist(files, fat32Path)
		tempName, _ := ramfs.MakeName([]byte(baseOf(persist.tempPath())))
		if _, status := fs.CreateName(root, tempName); status != ramfs.StatusOK {
			t.Fatal(status)
		}
		if _, err := persist.Open(); !errors.Is(err, ErrUncertain) {
			t.Fatalf("Open = %v, want ErrUncertain (interrupted provisioning)", err)
		}
		svc := New(files, fat32Path, fixedEpoch)
		if err := svc.Open(); err == nil {
			t.Fatal("Service.Open = nil, want error")
		}
		if svc.State() != identityproto.StateUnavailable {
			t.Fatalf("state = %v, want Unavailable", svc.State())
		}
	})

	t.Run("corrupt db unavailable", func(t *testing.T) {
		device, fs, root, files := newFat32Fixture(t)
		persist := NewPersist(files, fat32Path)
		if err := persist.Save(snapshotFor(t, "alice")); err != nil {
			t.Fatal(err)
		}
		name, _ := ramfs.MakeName([]byte("db"))
		id, status := fs.OpenName(root, name)
		if status != ramfs.StatusOK {
			t.Fatal(status)
		}
		if status = fs.Truncate(id, 0); status != ramfs.StatusOK {
			t.Fatal(status)
		}
		if count, status := fs.Write(id, 0, []byte("not a snapshot")); status != ramfs.StatusOK || count != len("not a snapshot") {
			t.Fatal(status)
		}
		_, _, rfiles, _ := reopen(t, device)
		rpersist := NewPersist(rfiles, fat32Path)
		if _, err := rpersist.Open(); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("Open corrupt = %v, want ErrCorrupt", err)
		}
		svc := New(rfiles, fat32Path, fixedEpoch)
		if err := svc.Open(); err == nil {
			t.Fatal("Service.Open corrupt = nil")
		}
		if svc.State() != identityproto.StateUnavailable {
			t.Fatalf("corrupt state = %v, want Unavailable", svc.State())
		}
	})

	t.Run("read failure unavailable", func(t *testing.T) {
		device, _, _, files := newFat32Fixture(t)
		persist := NewPersist(files, fat32Path)
		if err := persist.Save(snapshotFor(t, "alice")); err != nil {
			t.Fatal(err)
		}
		// Fail the next device read: Open's directory listing fails closed.
		device.failReadAt = device.reads + 1
		svc := New(files, fat32Path, fixedEpoch)
		if err := svc.Open(); err == nil {
			t.Fatal("Service.Open = nil, want read failure")
		}
		if svc.State() != identityproto.StateUnavailable {
			t.Fatalf("state = %v, want Unavailable", svc.State())
		}
	})
}
