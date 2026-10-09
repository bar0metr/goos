// Package truststate installs signed trust packages as persistent generations.
package truststate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"goos/userspace/truststore"
	"strconv"
)

const (
	StatePath           = "/state/security"
	GenerationsPath     = StatePath + "/generations"
	ActivePath          = StatePath + "/active"
	KeyPath             = "/system/security/trust-update-key.pub"
	URLPath             = "/system/security/trust-update-url"
	MaxGenerations      = 64
	MaxDirectoryEntries = 256
)

var ErrNotFound = errors.New("trust file not found")

// Files operations return only after closing their handles and completing the
// filesystem's durability barrier. Rename refuses an existing destination.
type Files interface {
	Lock() error
	Unlock() error
	Read(path string, limit int) ([]byte, error)
	Create(path string, data []byte) error
	Mkdir(path string) error
	Rename(source, target string) error
	Replace(source, target string) error
	List(path string, limit int) ([]string, error)
}

type Store struct {
	Files    Files
	Key      []byte
	Validate func([]byte) (int, error)
}

type Selection struct {
	Package *truststore.Package
	Path    string
	Reason  string
	Floor   uint64
}

func sequence(name string) (uint64, bool) {
	n, err := strconv.ParseUint(name, 10, 64)
	return n, err == nil && n != 0 && strconv.FormatUint(n, 10) == name
}

func directory(n uint64) string { return GenerationsPath + "/" + strconv.FormatUint(n, 10) }

func marker(meta []byte) []byte {
	digest := sha256.Sum256(meta)
	return []byte("GOOS-TRUST-COMMIT-V1\n" + hex.EncodeToString(digest[:]) + "\n")
}

func (s *Store) VerifyFiles(bundlePath, metaPath, sigPath string) (*truststore.Package, error) {
	bundle, err := s.Files.Read(bundlePath, truststore.MaxBundle)
	if err != nil {
		return nil, err
	}
	meta, err := s.Files.Read(metaPath, truststore.MaxMetadata)
	if err != nil {
		return nil, err
	}
	sig, err := s.Files.Read(sigPath, 64)
	if err != nil {
		return nil, err
	}
	return truststore.Verify(bundle, meta, sig, s.Key, s.Validate)
}

func (s *Store) generation(path string) (*truststore.Package, error) {
	pkg, err := s.VerifyFiles(path+"/ca-certificates.pem", path+"/ca-certificates.json", path+"/ca-certificates.sig")
	if err != nil {
		return nil, err
	}
	ready, err := s.Files.Read(path+"/committed", 128)
	if err != nil || !bytes.Equal(ready, marker(pkg.Meta)) {
		return nil, errors.New("trust generation incomplete or corrupt")
	}
	return pkg, nil
}

// Select considers only published generation directories, never staging paths.
// It verifies the highest surviving generation rather than trusting a writable
// selector to authorize a downgrade. A corrupt highest generation uses fallback.
func (s *Store) Select() Selection {
	result := Selection{Reason: "no installed signed update"}
	selector, err := s.Files.Read(ActivePath, 32)
	if err == nil {
		n, ok := sequence(string(bytes.TrimSuffix(selector, []byte("\n"))))
		if !ok || !bytes.Equal(selector, []byte(strconv.FormatUint(n, 10)+"\n")) {
			result.Reason = "corrupt trust selector"
		} else {
			result.Floor = n
		}
	} else if !errors.Is(err, ErrNotFound) {
		result.Reason = "unreadable trust selector"
	}
	names, err := s.Files.List(GenerationsPath, MaxDirectoryEntries)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			result.Reason = "trust generations unavailable or outside bounds"
		}
		if result.Floor != 0 {
			result.Reason = "selected trust generation unavailable"
		}
		return result
	}
	if len(names) > MaxDirectoryEntries {
		result.Reason = "too many trust generations"
		return result
	}
	highest := uint64(0)
	for _, name := range names {
		if n, ok := sequence(name); ok && n > highest {
			highest = n
		}
	}
	if highest < result.Floor {
		result.Reason = "trust generation rollback or missing state"
		return result
	}
	if highest == 0 {
		return result
	}
	result.Floor = highest
	path := directory(highest)
	pkg, err := s.generation(path)
	if err != nil {
		result.Reason = "state package invalid: " + err.Error()
		return result
	}
	if pkg.Metadata.Sequence != highest {
		result.Reason = "trust generation sequence mismatch"
		return result
	}
	result.Package, result.Path, result.Reason = pkg, path+"/ca-certificates.pem", ""
	return result
}

// Install leaves existing generations untouched. A published generation is
// complete and verified before the selector changes. Recovery can therefore
// select the old generation or the complete new one after an interrupted commit.
func (s *Store) Install(incoming *truststore.Package, nonce string) (result *truststore.Package, retErr error) {
	if incoming == nil {
		return nil, errors.New("trust package unavailable")
	}
	if len(nonce) != 16 {
		return nil, errors.New("invalid trust staging nonce")
	}
	for _, c := range nonce {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, errors.New("invalid trust staging nonce")
		}
	}
	// Do not rely on a caller having kept a verified Package immutable.
	verified, err := truststore.Verify(incoming.Bundle, incoming.Meta, incoming.Signature, s.Key, s.Validate)
	if err != nil {
		return nil, err
	}
	if err := s.Files.Lock(); err != nil {
		return nil, err
	}
	defer func() {
		if err := s.Files.Unlock(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	current := s.Select()
	if verified.Metadata.Sequence < current.Floor {
		return nil, errors.New("refusing trust store rollback from " + strconv.FormatUint(current.Floor, 10) + " to " + strconv.FormatUint(verified.Metadata.Sequence, 10))
	}
	if current.Package != nil {
		if err := truststore.CheckUpgrade(current.Package, verified); err != nil {
			return nil, err
		}
		if current.Package.Metadata.Sequence == verified.Metadata.Sequence {
			return current.Package, nil
		}
	} else if verified.Metadata.Sequence == current.Floor && current.Floor != 0 {
		return nil, errors.New("cannot replace corrupt trust generation at the same sequence")
	}
	names, listErr := s.Files.List(GenerationsPath, MaxDirectoryEntries)
	if listErr != nil && !errors.Is(listErr, ErrNotFound) {
		return nil, listErr
	}
	if len(names) >= MaxDirectoryEntries {
		return nil, errors.New("trust directory capacity reached; existing state remains active")
	}
	generations := 0
	for _, name := range names {
		if _, ok := sequence(name); ok {
			generations++
		}
	}
	if generations >= MaxGenerations {
		return nil, errors.New("trust generation capacity reached; existing state remains active")
	}
	for _, path := range []string{StatePath, GenerationsPath} {
		if err := s.Files.Mkdir(path); err != nil {
			return nil, err
		}
	}
	staging := StatePath + "/.trust-new-" + strconv.FormatUint(verified.Metadata.Sequence, 10) + "-" + nonce
	if err := s.Files.Mkdir(staging); err != nil {
		return nil, err
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"ca-certificates.pem", verified.Bundle}, {"ca-certificates.json", verified.Meta}, {"ca-certificates.sig", verified.Signature}} {
		if err := s.Files.Create(staging+"/"+file.name, file.data); err != nil {
			return nil, err
		}
	}
	staged, err := s.VerifyFiles(staging+"/ca-certificates.pem", staging+"/ca-certificates.json", staging+"/ca-certificates.sig")
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(staged.Bundle, verified.Bundle) || !bytes.Equal(staged.Meta, verified.Meta) || !bytes.Equal(staged.Signature, verified.Signature) {
		return nil, errors.New("staged trust data changed")
	}
	if err := s.Files.Create(staging+"/committed", marker(staged.Meta)); err != nil {
		return nil, err
	}
	// Recheck after staging so another installer cannot authorize a lower version.
	latest := s.Select()
	if latest.Floor >= staged.Metadata.Sequence && latest.Floor != 0 {
		return nil, errors.New("trust sequence changed during installation")
	}
	dest := directory(staged.Metadata.Sequence)
	if err := s.Files.Rename(staging, dest); err != nil {
		return nil, err
	}
	committed, err := s.generation(dest)
	if err != nil {
		return nil, err
	}
	selector := StatePath + "/.active-" + nonce
	if err := s.Files.Create(selector, []byte(strconv.FormatUint(committed.Metadata.Sequence, 10)+"\n")); err != nil {
		return nil, err
	}
	if err := s.Files.Replace(selector, ActivePath); err != nil {
		return nil, err
	}
	selected := s.Select()
	if selected.Package == nil || selected.Package.Metadata.Sequence < committed.Metadata.Sequence {
		return nil, errors.New("committed trust selection failed verification")
	}
	return selected.Package, nil
}
