//go:build goos_userspace

package tlsclient

import (
	"errors"
	"goos/internal/validation"
	"goos/userspace/nativefiles"
	"goos/userspace/truststate"
	"goos/userspace/utility"
	"strconv"
)

// LoadSystemRoots prefers authenticated persistent roots and reports fallback.
func LoadSystemRoots(bulk *utility.Bulk) (*Roots, error) {
	roots, selected, err := LoadSelectedRoots(bulk)
	if selected.Package != nil {
		utility.WriteError([]byte("trust: using signed state roots sequence=" + strconv.FormatUint(selected.Package.Metadata.Sequence, 10) + "\n"))
	} else {
		utility.WriteError([]byte("trust: " + selected.Reason + ", using system fallback roots\n"))
	}
	return roots, err
}

// LoadSelectedRoots never authorizes writable state without the immutable key.
func LoadSelectedRoots(bulk *utility.Bulk) (*Roots, truststate.Selection, error) {
	files := &nativefiles.Files{Bulk: bulk}
	key, keyErr := files.Read(truststate.KeyPath, 32)
	var roots *Roots
	store := truststate.Store{Files: files, Key: key, Validate: func(data []byte) (int, error) {
		parsed, err := LoadRoots(data)
		if err != nil {
			return 0, err
		}
		roots = parsed
		return parsed.Count(), nil
	}}
	selected := store.Select()
	if keyErr == nil && selected.Package != nil {
		return roots, selected, nil
	}
	if keyErr != nil {
		selected.Package = nil
		selected.Reason = "trust update key unavailable"
	}
	fallback, err := loadNativeRoots(RootPath, bulk)
	return fallback, selected, err
}

func LoadProductionProbeRoots(bulk *utility.Bulk) (*Roots, error) {
	if !validation.Enabled {
		return nil, errors.New("production bundle probe requires a validation image")
	}
	return loadNativeRoots("/system/security/probe-production.pem", bulk)
}

func loadNativeRoots(name string, bulk *utility.Bulk) (*Roots, error) {
	files := &nativefiles.Files{Bulk: bulk}
	data, err := files.Read(name, MaxBundle)
	if err != nil {
		return nil, err
	}
	return LoadRoots(data)
}
