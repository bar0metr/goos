package identityd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"goos/userspace/identity"
)

// Sentinel errors the persistence layer reports. ErrNotFound means the
// snapshot is absent (Unprovisioned); ErrCorrupt means the snapshot failed
// decode/validation (Unavailable); ErrUncertain means a replacement may have
// committed, so the caller must reconcile from storage, never assume.
var (
	ErrNotFound  = errors.New("identityd: snapshot not found")
	ErrCorrupt   = errors.New("identityd: snapshot corrupt")
	ErrUncertain = errors.New("identityd: snapshot durability uncertain")
)

// tempSuffix mirrors safesave.Suffix: the deterministic temporary-file marker
// written beside the target (ADR 0129, ADR 0233 §3).
const tempSuffix = ".~save"

// tempSequence names successive temporary files. identityd is single-owner, so
// no atomicity is needed; the value only has to avoid reusing a name within a
// run. Stale files from a previous run are never opened or changed.
var tempSequence uint64

// Handle is an opaque open-file handle from Files.OpenWrite.
type Handle uint64

// Files is the minimal filesystem surface the persistence layer needs. It is
// safesave's surface (exclusive create, write, close, replace, unlink) plus a
// whole-file read, expressed with string paths and error returns so this layer
// stays host-testable and free of kernel/VFS types. The service adapts the
// calls to its VFS session; its Replace maps VFS errors to ErrUncertain when a
// rename may have committed.
type Files interface {
	Read(path string) ([]byte, error)
	Create(path string) error
	Unlink(path string) error
	OpenWrite(path string) (Handle, error)
	Write(file Handle, data []byte) (int, error)
	Close(file Handle) error
	Replace(source, target string) error
}

// Faults lets host tests cut a save at each durability boundary. A nil hook is
// a no-op. A non-nil error aborts the save: before the replacement the old
// snapshot is intact, after it the caller must reconcile.
type Faults struct {
	// BeforeTempWrite runs after the temporary file is created and before its
	// contents are written.
	BeforeTempWrite func() error
	// AfterTempWrite runs after the temporary file is fully written and
	// closed, before it is renamed into place.
	AfterTempWrite func() error
	// BeforeReplace runs immediately before the atomic rename.
	BeforeReplace func() error
	// AfterReplace runs immediately after the atomic rename has committed.
	AfterReplace func() error
	// ReplaceErr, when non-nil, replaces the rename's real result. A test sets
	// it to ErrUncertain to model a rename whose outcome is unknown.
	ReplaceErr func() error
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

// Load reads and decodes the snapshot. It returns ErrNotFound when the file is
// absent, ErrCorrupt (wrapping the identity error) when the bytes fail
// validation, and the validated store on success.
func (p *Persist) Load() (*identity.Store, error) {
	data, err := p.Files.Read(p.Path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	store, err := identity.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return store, nil
}

// Save atomically replaces the snapshot with data. It returns nil when the
// replacement committed, ErrUncertain when the outcome is unknown (the caller
// must reconcile from storage), and another error when the save failed before
// the replacement, leaving the old snapshot intact.
func (p *Persist) Save(data []byte) error {
	return p.saveBytes(data)
}

// saveBytes writes data beside the target through the fault hooks and then
// atomically replaces the target. Failures before the replacement never touch
// the target; a failure at or after it is ErrUncertain.
func (p *Persist) saveBytes(data []byte) error {
	faults := &p.Faults
	temp := p.tempPath()
	if err := p.Files.Create(temp); err != nil {
		return err
	}
	replaced := false
	defer func() {
		if !replaced {
			_ = p.Files.Unlink(temp)
		}
	}()
	if err := runHook(faults.BeforeTempWrite); err != nil {
		return err
	}
	file, err := p.Files.OpenWrite(temp)
	if err != nil {
		return err
	}
	writeErr := p.writeAll(file, data)
	closeErr := p.Files.Close(file)
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := runHook(faults.AfterTempWrite); err != nil {
		return err
	}
	if err := runHook(faults.BeforeReplace); err != nil {
		return err
	}
	replaceErr := error(nil)
	if faults.ReplaceErr != nil {
		replaceErr = faults.ReplaceErr()
	} else {
		replaceErr = p.Files.Replace(temp, p.Path)
	}
	if replaceErr != nil {
		if errors.Is(replaceErr, ErrUncertain) {
			return fmt.Errorf("%w: %v", ErrUncertain, replaceErr)
		}
		return replaceErr
	}
	replaced = true
	if err := runHook(faults.AfterReplace); err != nil {
		// The rename committed; the caller must reconcile forward, never
		// assume the old snapshot survived.
		return fmt.Errorf("%w: %v", ErrUncertain, err)
	}
	return nil
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

// tempPath returns the deterministic temporary-file name beside the target:
// dir + "." + name + tempSuffix + "." + counter.
func (p *Persist) tempPath() string {
	tempSequence++
	slash := strings.LastIndexByte(p.Path, '/')
	if slash < 0 {
		slash = -1
	}
	dir, name := p.Path[:slash+1], p.Path[slash+1:]
	buf := make([]byte, 0, len(dir)+1+len(name)+len(tempSuffix)+1+16)
	buf = append(buf, dir...)
	buf = append(buf, '.')
	buf = append(buf, name...)
	buf = append(buf, tempSuffix...)
	buf = append(buf, '.')
	buf = strconv.AppendUint(buf, tempSequence, 16)
	return string(buf)
}

// runHook invokes hook when set, returning its error.
func runHook(hook func() error) error {
	if hook == nil {
		return nil
	}
	return hook()
}
