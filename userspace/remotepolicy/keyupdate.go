package remotepolicy

import (
	"bytes"
	"errors"
)

const KeyStageSuffix = ".key-new"
const KeyPriorSuffix = ".key-old"

// KeyFiles operations close their handles before returning. Create and Rename
// refuse existing destinations; Replace is the filesystem's atomic publication.
type KeyFiles interface {
	Read(string, int) ([]byte, error)
	Create(string, []byte) error
	Rename(string, string) error
	Replace(string, string) error
	Remove(string) error
}

// KeyUpdate runs under the dedicated remote update guard. Check verifies the
// PID 1-issued path/revision ownership. Reload acknowledges a committed runtime
// generation, and must verify that generation uses the candidate key bytes.
type KeyUpdate struct {
	Files  KeyFiles
	Check  func() error
	Reload func([]byte) (uint64, error)
}

func (u *KeyUpdate) read(path string) ([]byte, bool, error) {
	data, err := u.Files.Read(path, MaxAuthorizedKeysBytes)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	return data, err == nil, err
}
func (u *KeyUpdate) owns(path string, expected []byte, exists bool) error {
	if err := u.Check(); err != nil {
		return err
	}
	actual, present, err := u.read(path)
	if err != nil {
		return err
	}
	if present != exists || !bytes.Equal(actual, expected) {
		return errors.New("authorized keys changed concurrently")
	}
	return u.Check()
}

func (u *KeyUpdate) Apply(path string, edit func([]byte) ([]byte, error)) (generation uint64, retErr error) {
	if u == nil || u.Files == nil || u.Check == nil || u.Reload == nil || !ValidPath(path) {
		return 0, errors.New("invalid authorized key update")
	}
	if err := u.Check(); err != nil {
		return 0, err
	}
	old, exists, err := u.read(path)
	if err != nil {
		return 0, err
	}
	if _, err = ParseAuthorizedKeys(old); err != nil {
		return 0, err
	}
	candidate, err := edit(old)
	if err != nil {
		return 0, err
	}
	if _, err = ParseAuthorizedKeys(candidate); err != nil {
		return 0, err
	}
	stage, prior := path+KeyStageSuffix, path+KeyPriorSuffix
	for _, name := range []string{stage, prior} {
		if _, err = u.Files.Read(name, MaxAuthorizedKeysBytes); !errors.Is(err, ErrNotFound) {
			return 0, errors.New("authorized key staging file already exists or unavailable")
		}
	}
	// Only names observed absent under the confined, exclusive staging grant
	// are cleaned up. A pre-existing sibling is never overwritten or removed.
	stageOwned, priorOwned := true, false
	defer func() {
		if stageOwned {
			if err := u.Files.Remove(stage); err != nil && !errors.Is(err, ErrNotFound) {
				retErr = errors.Join(retErr, err)
			}
		}
		if priorOwned {
			if err := u.Files.Remove(prior); err != nil && !errors.Is(err, ErrNotFound) {
				retErr = errors.Join(retErr, err)
			}
		}
	}()
	if err = u.Files.Create(stage, candidate); err != nil {
		return 0, err
	}
	verified, err := u.Files.Read(stage, MaxAuthorizedKeysBytes)
	if err != nil || !bytes.Equal(verified, candidate) {
		return 0, errors.Join(errors.New("authorized key staging verification failed"), err)
	}
	if exists {
		priorOwned = true
		if err = u.Files.Create(prior, old); err != nil {
			return 0, err
		}
		verified, err = u.Files.Read(prior, MaxAuthorizedKeysBytes)
		if err != nil || !bytes.Equal(verified, old) {
			return 0, errors.Join(errors.New("prior authorized key verification failed"), err)
		}
	}
	if err = u.owns(path, old, exists); err != nil {
		return 0, err
	}
	if exists {
		err = u.Files.Replace(stage, path)
	} else {
		err = u.Files.Rename(stage, path)
	}
	// A failed filesystem durability barrier may still have published its live
	// namespace. Roll back only if the exact candidate still belongs to us.
	if err == nil {
		stageOwned = false
		if err = u.owns(path, candidate, true); err == nil {
			generation, err = u.Reload(candidate)
			if err == nil && generation == 0 {
				err = errors.New("remote reload did not acknowledge a generation")
			}
		}
	}
	if err == nil {
		return generation, nil
	}
	if ownership := u.owns(path, candidate, true); ownership != nil {
		return 0, errors.Join(err, errors.New("prior authorized keys not restored: ownership changed"), ownership)
	}
	if exists {
		verified, check := u.Files.Read(prior, MaxAuthorizedKeysBytes)
		if check != nil || !bytes.Equal(verified, old) {
			return 0, errors.Join(err, errors.New("prior authorized keys not restored: staging changed"), check)
		}
		if rollback := u.Files.Replace(prior, path); rollback != nil {
			priorOwned = false // Retain the verified prior for trusted recovery.
			return 0, errors.Join(err, rollback, errors.New("prior authorized keys retained at "+prior))
		}
		priorOwned = false
	} else {
		if rollback := u.Files.Remove(path); rollback != nil {
			return 0, errors.Join(err, rollback)
		}
	}
	if check := u.owns(path, old, exists); check != nil {
		return 0, errors.Join(err, errors.New("prior authorized keys restoration not verified"), check)
	}
	return 0, err
}
