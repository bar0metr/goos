package remotepolicy

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"goos/userspace/cryptosys"
	"goos/userspace/truststate"
	"io"
)

// ErrNotFound is shared with nativefiles, so its bounded adapter can implement
// HostFiles without translating missing-file errors.
var ErrNotFound = truststate.ErrNotFound

// HostFiles closes handles and completes the filesystem durability barrier
// before returning. Create and Rename must refuse existing destinations. Lock
// excludes other key writers until Unlock; readers still verify persisted data.
type HostFiles interface {
	Lock() error
	Unlock() error
	Read(path string, limit int) ([]byte, error)
	Create(path string, data []byte) error
	Mkdir(path string) error
	Rename(source, target string) error
}

type HostKey struct {
	Private     ed25519.PrivateKey
	Public      ed25519.PublicKey
	Fingerprint string
}

type HostStore struct {
	Files HostFiles
	// Random is injectable for host tests. Nil selects the native kernel reader.
	Random io.Reader
}

// Load reads a raw 32-byte seed and its canonical .pub file. Only a missing
// final and staged seed permits generation. A complete staged seed is recovered
// without changing its identity. Corrupt existing seeds or public files fail
// closed; a missing public file can be reconstructed from the verified seed.
func (store *HostStore) Load(path string) (key *HostKey, retErr error) {
	if store == nil || store.Files == nil || !ValidPath(path) {
		return nil, errors.New("invalid host key store or path")
	}
	if err := store.Files.Lock(); err != nil {
		return nil, err
	}
	var private ed25519.PrivateKey
	defer func() {
		if err := store.Files.Unlock(); err != nil {
			key = nil
			retErr = errors.Join(retErr, err)
		}
		if retErr != nil {
			clear(private)
		}
	}()
	seed, err := store.Files.Read(path, ed25519.SeedSize)
	defer func() { clear(seed) }()
	staged := false
	if errors.Is(err, ErrNotFound) {
		staged = true
		seed, err = store.Files.Read(path+".new", ed25519.SeedSize)
		if errors.Is(err, ErrNotFound) {
			// A public companion without its seed is lost identity, not a fresh
			// installation. Never silently replace it with a new key.
			for _, publicPath := range []string{path + ".pub", path + ".pub.new"} {
				if _, err := store.Files.Read(publicPath, MaxAuthorizedKeyLine); !errors.Is(err, ErrNotFound) {
					if err != nil {
						return nil, err
					}
					return nil, errors.New("host public key exists without seed")
				}
			}
			random := store.Random
			if random == nil {
				random = cryptosys.Reader{}
			}
			_, private, generationErr := ed25519.GenerateKey(random)
			if generationErr != nil {
				return nil, generationErr
			}
			defer clear(private)
			seed = private.Seed()
			if err := store.makeParents(path); err != nil {
				return nil, err
			}
			if err := store.Files.Create(path+".new", seed); err != nil {
				return nil, err
			}
			if err := store.verify(path+".new", seed); err != nil {
				return nil, err
			}
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("corrupt host key seed")
	}
	private = ed25519.NewKeyFromSeed(seed)
	public := append(ed25519.PublicKey(nil), private[ed25519.SeedSize:]...)
	line, err := PublicLine(public)
	if err != nil {
		return nil, err
	}
	// A published seed is authoritative, but leftover staging must still agree
	// with it. Never hide corrupt or conflicting state behind a valid final pair.
	if !staged {
		if err := store.verify(path+".new", seed); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	publicPath := path + ".pub"
	pending, stageErr := store.Files.Read(publicPath+".new", len(line))
	if stageErr != nil && !errors.Is(stageErr, ErrNotFound) {
		return nil, stageErr
	}
	if stageErr == nil && !bytes.Equal(pending, line) {
		return nil, errors.New("staged host public key does not match seed")
	}
	publishedPublic, err := store.Files.Read(publicPath, len(line))
	publicMissing := errors.Is(err, ErrNotFound)
	if err != nil && !publicMissing {
		return nil, err
	}
	if !publicMissing && !bytes.Equal(publishedPublic, line) {
		return nil, errors.New("host public key does not match seed")
	}
	if publicMissing {
		if errors.Is(stageErr, ErrNotFound) {
			if err := store.Files.Create(publicPath+".new", line); err != nil {
				return nil, err
			}
		}
		if err := store.verify(publicPath+".new", line); err != nil {
			return nil, err
		}
	}
	if staged {
		if err := store.verify(path+".new", seed); err != nil {
			return nil, err
		}
		if err := store.Files.Rename(path+".new", path); err != nil {
			return nil, err
		}
	}
	if err := store.verify(path, seed); err != nil {
		return nil, err
	}
	if publicMissing {
		if err := store.Files.Rename(publicPath+".new", publicPath); err != nil {
			return nil, err
		}
	}
	if err := store.verify(publicPath, line); err != nil {
		return nil, err
	}
	// Verify both files after all publishing operations, not just each rename.
	if err := store.verify(path, seed); err != nil {
		return nil, err
	}
	fingerprint, err := Fingerprint(public)
	if err != nil {
		return nil, err
	}
	return &HostKey{Private: private, Public: public, Fingerprint: fingerprint}, nil
}

func (store *HostStore) verify(path string, expected []byte) error {
	actual, err := store.Files.Read(path, len(expected))
	defer clear(actual)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return errors.New("host key file changed or is incomplete")
	}
	return nil
}

func (store *HostStore) makeParents(path string) error {
	for index := 1; index < len(path); index++ {
		if path[index] == '/' {
			if err := store.Files.Mkdir(path[:index]); err != nil {
				return err
			}
		}
	}
	return nil
}

// LoadAuthorizedKeys returns an entirely parsed candidate or no usable keys.
func LoadAuthorizedKeys(files interface {
	Read(string, int) ([]byte, error)
}, path string) (AuthorizedKeys, error) {
	if files == nil || !ValidPath(path) {
		return AuthorizedKeys{}, errors.New("invalid authorized keys source")
	}
	data, err := files.Read(path, MaxAuthorizedKeysBytes)
	if err != nil {
		return AuthorizedKeys{}, err
	}
	return ParseAuthorizedKeys(data)
}
