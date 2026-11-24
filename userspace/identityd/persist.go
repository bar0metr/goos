package identityd

import (
	"errors"
	"fmt"
	"strings"

	"goos/userspace/identity"
)

// Sentinel errors the persistence layer reports. ErrNotFound means the
// snapshot is absent (Unprovisioned); ErrCorrupt means the snapshot failed
// decode/validation (Unavailable); ErrUncertain means a replacement or final
// durability barrier may have committed or failed, so the caller must
// reconcile from storage on a fresh lifetime, never assume.
var (
	ErrNotFound  = errors.New("identityd: snapshot not found")
	ErrCorrupt   = errors.New("identityd: snapshot corrupt")
	ErrUncertain = errors.New("identityd: snapshot durability uncertain")
	// ErrExists reports an exclusive-create collision on the reserved
	// temporary name.
	ErrExists = errors.New("identityd: already exists")
)

// tempSuffix is the deterministic temporary-file marker. identityd uses a
// single reserved temporary name — dir + "." + name + tempSuffix, no counter —
// created exclusively. A file under that exact name left by a prior writer is
// recognized by name and reconciled explicitly (ADR 0233 §4).
const tempSuffix = ".~save"

// maxDirEntries bounds the startup directory reconciliation (§4.3). The
// snapshot directory holds only the db file plus, transiently, the reserved
// temporary name; a larger count is an unexpected kind and fails closed.
const maxDirEntries = 8

// Handle is an opaque open-file handle from Files.OpenWrite.
type Handle uint64

// Files is the durable filesystem surface the coordinator needs. It is
// expressed with string paths and error returns so this layer stays
// host-testable and free of kernel/VFS types. The native service adapts these
// calls to its confined VFS session; a replaced/rename operation that may have
// committed maps to ErrUncertain.
//
// Read must return an error — never a partial success — on timeout, malformed
// reply, failed read/status, or a short or invalid length, and must enforce
// identity.MaxSnapshotBytes before appending. Create must be an exclusive
// create: an existing path reports ErrExists and leaves it unchanged. Close is
// not fsync. A required unsupported or failed Sync/SyncDir is not success.
type Files interface {
	// Read reads the whole file at path.
	Read(path string) ([]byte, error)
	// Create atomically creates path only if it is absent.
	Create(path string) error
	// Unlink removes path. NotFound may be idempotent for the caller's own
	// temporary file.
	Unlink(path string) error
	// OpenWrite opens path for whole-file write and returns a handle.
	OpenWrite(path string) (Handle, error)
	// Write writes data to file, returning the bytes written (all or error).
	Write(file Handle, data []byte) (int, error)
	// Sync flushes file's data to stable storage (fsync on the open handle).
	Sync(file Handle) error
	// Close closes file.
	Close(file Handle) error
	// Replace atomically renames source onto target. A failure here may still
	// have committed; ambiguous statuses map to ErrUncertain.
	Replace(source, target string) error
	// SyncDir establishes the durability barrier for the directory containing
	// path (the barrier that persists a rename of path).
	SyncDir(path string) error
	// ListNames returns up to max base names of the entries in the directory
	// containing path, without following links. It fails rather than return a
	// partial list when the directory holds more than max entries.
	ListNames(path string, max int) ([]string, error)
}

// Faults lets host tests cut a save at each durability boundary. A nil hook is
// a no-op. A non-nil error aborts the save: before the replacement the old
// snapshot is intact, at or after it the caller must reconcile.
type Faults struct {
	// BeforeTempWrite runs after the temporary file is created and before its
	// contents are written.
	BeforeTempWrite func() error
	// AfterTempWrite runs after the temporary file is fully written, synced and
	// closed, before it is renamed into place.
	AfterTempWrite func() error
	// BeforeReplace runs immediately before the atomic rename.
	BeforeReplace func() error
	// AfterReplace runs immediately after the atomic rename has committed.
	AfterReplace func() error
	// ReplaceErr, when non-nil, replaces the rename's real result. A test sets
	// it to ErrUncertain to model a rename whose outcome is unknown.
	ReplaceErr func() error
	// SyncErr, when non-nil, replaces the temporary-handle Sync result. A
	// failure here is a definite pre-replace failure.
	SyncErr func() error
	// SyncDirErr, when non-nil, replaces the final directory Sync result. In a
	// save a failure here follows a committed rename and is uncertain; in Open
	// the same barrier confirms the loaded snapshot's storage and fails closed.
	SyncDirErr func() error
}

// Persist loads and saves one versioned snapshot atomically. It is not safe
// for concurrent use; identityd is single-owner.
type Persist struct {
	Files  Files
	Faults Faults
	Path   string
}

// NewPersist returns a persistence layer over files for path.
func NewPersist(files Files, path string) *Persist {
	return &Persist{Files: files, Path: path}
}

// Open reconciles any stale temporary file (§4.3) and loads the durable
// snapshot. Returns:
//
//	(store, nil)        valid committed snapshot (Available)
//	(nil, ErrNotFound)  no db and no temp (Unprovisioned)
//	(nil, ErrCorrupt)   db present but invalid (Unavailable)
//	(nil, other)        interrupted provisioning (db absent + stale save) or a
//	                    failed listing/cleanup → fail closed (Unavailable)
//
// Open never promotes a temporary snapshot, never follows links, never scans
// an unlimited directory, and never regenerates a database.
func (p *Persist) Open() (*identity.Store, error) {
	temp := p.tempPath()
	names, err := p.Files.ListNames(p.Path, maxDirEntries)
	if err != nil {
		// Failed listing fails closed.
		return nil, fmt.Errorf("identityd: snapshot directory listing failed: %w", err)
	}
	if hasName(names, baseOf(temp)) {
		// A reserved temp name is present. identityd is single-owner, so a
		// prior writer's lifetime is fenced by process death once this process
		// is running; the temp is therefore stale and not authoritative.
		if !hasName(names, baseOf(p.Path)) {
			// DB absent + stale save = interrupted/ambiguous provisioning.
			return nil, fmt.Errorf("identityd: interrupted provisioning: stale save %s: %w", temp, ErrUncertain)
		}
		// DB present: the temp is not authoritative. Remove it (bounded and
		// verified) so it cannot block a future save, then load the db.
		if err := p.removeTemp(temp); err != nil {
			return nil, err
		}
	}
	store, err := p.loadDB()
	if err != nil {
		return nil, err
	}
	// The loaded snapshot is not accepted as Available until the underlying
	// storage is confirmed stable: issue the durability barrier for the
	// directory that persists the snapshot. A failed barrier fails closed.
	if err := p.syncDir(); err != nil {
		return nil, fmt.Errorf("identityd: snapshot durability barrier failed: %w", err)
	}
	return store, nil
}

// Save atomically replaces the snapshot with data, following the required
// ordering:
//
//	exclusive temp create -> full write -> Sync(temp handle) -> checked Close
//	-> Replace(temp, db) -> Sync(containing directory)
//
// It returns nil when the replacement committed and the final barrier
// succeeded, ErrUncertain when the outcome is unknown (the caller must
// reconcile from storage, never by guessing), and another error when the save
// failed before the replacement, leaving the old snapshot intact.
func (p *Persist) Save(data []byte) error {
	return p.saveBytes(data)
}

// saveBytes writes data beside the target through the fault hooks and then
// atomically replaces the target. Failures before the replacement never touch
// the target; a failure at or after it is ErrUncertain.
func (p *Persist) saveBytes(data []byte) error {
	temp := p.tempPath()
	if err := p.createTemp(temp); err != nil {
		return err
	}
	// replaceAttempted is false until the atomic replace is about to run.
	// §4.2 forbids any further filesystem mutation once an earlier operation
	// could still complete, so the temporary is unlinked only on a definite
	// pre-replace failure and never once the replace has been attempted —
	// whether it committed, was refused, or its outcome is unknown.
	replaceAttempted := false
	defer func() {
		if !replaceAttempted {
			_ = p.Files.Unlink(temp)
		}
	}()

	if err := runHook(p.Faults.BeforeTempWrite); err != nil {
		return err
	}
	file, err := p.Files.OpenWrite(temp)
	if err != nil {
		return err
	}
	writeErr := p.writeAll(file, data)
	var syncErr error
	if writeErr == nil {
		syncErr = p.syncFile(file)
	}
	closeErr := p.Files.Close(file)
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := runHook(p.Faults.AfterTempWrite); err != nil {
		return err
	}
	if err := runHook(p.Faults.BeforeReplace); err != nil {
		return err
	}

	replaceAttempted = true
	replaceErr := error(nil)
	if p.Faults.ReplaceErr != nil {
		replaceErr = p.Faults.ReplaceErr()
	} else {
		replaceErr = p.Files.Replace(temp, p.Path)
	}
	if replaceErr != nil {
		if errors.Is(replaceErr, ErrUncertain) {
			return fmt.Errorf("%w: %v", ErrUncertain, replaceErr)
		}
		return replaceErr
	}
	if err := runHook(p.Faults.AfterReplace); err != nil {
		// The rename committed; the caller must reconcile forward, never
		// assume the old snapshot survived.
		return fmt.Errorf("%w: %v", ErrUncertain, err)
	}
	if err := p.syncDir(); err != nil {
		// The rename committed but the final barrier did not: fail closed.
		return fmt.Errorf("%w: final directory sync failed: %v", ErrUncertain, err)
	}
	return nil
}

// createTemp exclusively creates the reserved temporary name. identityd is
// single-owner, so an existing file under that exact name is a stale remnant
// of a prior writer whose lifetime is fenced; it is removed once (bounded) and
// the create retried, so a stale file never permanently blocks a legitimate
// save or recovery.
func (p *Persist) createTemp(temp string) error {
	if err := p.Files.Create(temp); err == nil {
		return nil
	} else if !errors.Is(err, ErrExists) {
		return err
	}
	if err := p.removeTemp(temp); err != nil {
		return err
	}
	return p.Files.Create(temp)
}

// removeTemp removes the reserved temporary name and verifies the removal by a
// bounded re-listing. It fails closed if the temp is still present after a
// successful unlink, so a stale file can never be silently left to block saves.
func (p *Persist) removeTemp(temp string) error {
	if err := p.Files.Unlink(temp); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("identityd: stale temp cleanup failed: %w", err)
	}
	names, err := p.Files.ListNames(p.Path, maxDirEntries)
	if err != nil {
		return fmt.Errorf("identityd: stale temp cleanup verification failed: %w", err)
	}
	if hasName(names, baseOf(temp)) {
		return fmt.Errorf("identityd: stale temp cleanup unverified: %s still present", temp)
	}
	return nil
}

// loadDB reads and decodes the snapshot, enforcing the size bound before
// decode and never returning a partial success.
func (p *Persist) loadDB() (*identity.Store, error) {
	data, err := p.Files.Read(p.Path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if len(data) > identity.MaxSnapshotBytes {
		return nil, fmt.Errorf("%w: snapshot exceeds %d bytes", ErrCorrupt, identity.MaxSnapshotBytes)
	}
	store, err := identity.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return store, nil
}

// writeAll writes data in whole pieces, rejecting a short write.
func (p *Persist) writeAll(file Handle, data []byte) error {
	for len(data) != 0 {
		written, err := p.Files.Write(file, data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return errors.New("identityd: short snapshot write")
		}
		data = data[written:]
	}
	return nil
}

func (p *Persist) syncFile(file Handle) error {
	if p.Faults.SyncErr != nil {
		return p.Faults.SyncErr()
	}
	return p.Files.Sync(file)
}

func (p *Persist) syncDir() error {
	if p.Faults.SyncDirErr != nil {
		return p.Faults.SyncDirErr()
	}
	return p.Files.SyncDir(p.Path)
}

// tempPath returns the single reserved deterministic temporary-file name
// beside the target: dir + "." + name + tempSuffix.
func (p *Persist) tempPath() string {
	return dirOf(p.Path) + "." + baseOf(p.Path) + tempSuffix
}

func dirOf(path string) string {
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		return path[:slash+1]
	}
	return ""
}

func baseOf(path string) string {
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		return path[slash+1:]
	}
	return path
}

func hasName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// runHook invokes hook when set, returning its error.
func runHook(hook func() error) error {
	if hook == nil {
		return nil
	}
	return hook()
}
