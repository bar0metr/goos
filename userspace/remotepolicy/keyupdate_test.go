package remotepolicy

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

const adminPath = "/home/user/.ssh/authorized_keys"

type keyTestFiles struct {
	data    map[string][]byte
	calls   []string
	fail    string
	partial bool
	hook    func(string)
}

func (f *keyTestFiles) op(op string) error {
	f.calls = append(f.calls, op)
	if f.hook != nil {
		f.hook(op)
	}
	if f.fail == op {
		return errInjected
	}
	return nil
}
func (f *keyTestFiles) Read(path string, limit int) ([]byte, error) {
	if err := f.op("read " + path); err != nil {
		return nil, err
	}
	data, ok := f.data[path]
	if !ok {
		return nil, ErrNotFound
	}
	if len(data) > limit {
		return nil, errors.New("size bound")
	}
	return bytes.Clone(data), nil
}
func (f *keyTestFiles) Create(path string, data []byte) error {
	if _, ok := f.data[path]; ok {
		return errors.New("already exists")
	}
	if err := f.op("create " + path); err != nil {
		if f.partial {
			f.data[path] = bytes.Clone(data[:len(data)/2])
		}
		return err
	}
	f.data[path] = bytes.Clone(data)
	return nil
}
func (f *keyTestFiles) Rename(source, target string) error {
	if _, ok := f.data[target]; ok {
		return errors.New("already exists")
	}
	return f.Replace(source, target)
}
func (f *keyTestFiles) Replace(source, target string) error {
	data, ok := f.data[source]
	if !ok {
		return ErrNotFound
	}
	err := f.op("replace " + source)
	if err == nil || f.partial {
		f.data[target] = data
		delete(f.data, source)
	}
	return err
}
func (f *keyTestFiles) Remove(path string) error {
	if err := f.op("remove " + path); err != nil {
		return err
	}
	if _, ok := f.data[path]; !ok {
		return ErrNotFound
	}
	delete(f.data, path)
	return nil
}
func updateFixture(t *testing.T, exists bool) (*keyTestFiles, *KeyUpdate, []byte) {
	t.Helper()
	old := append([]byte("# preserve\n"), append(adminLine(t, 1, " first"), '\n')...)
	fs := &keyTestFiles{data: make(map[string][]byte)}
	if exists {
		fs.data[adminPath] = bytes.Clone(old)
	}
	u := &KeyUpdate{Files: fs, Check: func() error { return nil }, Reload: func(candidate []byte) (uint64, error) {
		if !bytes.Equal(fs.data[adminPath], candidate) {
			return 0, errors.New("candidate not published")
		}
		return 2, nil
	}}
	return fs, u, old
}
func addEdit(t *testing.T) func([]byte) ([]byte, error) {
	line := adminLine(t, 2, " second")
	return func(old []byte) ([]byte, error) { return AddAuthorizedKey(old, line) }
}
func TestKeyUpdatePublishAndBootstrap(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprint(exists), func(t *testing.T) {
			fs, u, old := updateFixture(t, exists)
			gen, err := u.Apply(adminPath, addEdit(t))
			if err != nil || gen != 2 {
				t.Fatal(gen, err)
			}
			if exists && !bytes.HasPrefix(fs.data[adminPath], old) {
				t.Fatal("old comments lost")
			}
			if len(fs.data) != 1 {
				t.Fatal("staging left after success", fs.data)
			}
		})
	}
}
func TestKeyUpdateWriteCloseVerifyReplaceFailuresKeepActiveOld(t *testing.T) {
	for _, failure := range []string{"write", "close", "verify-new", "verify-old", "replace", "replace-live"} {
		t.Run(failure, func(t *testing.T) {
			fs, u, old := updateFixture(t, true)
			reloads := 0
			u.Reload = func([]byte) (uint64, error) { reloads++; return 2, nil }
			switch failure {
			case "write", "close":
				fs.fail = "create " + adminPath + KeyStageSuffix
				fs.partial = true
			case "verify-new":
				fs.hook = func(op string) {
					if op == "read "+adminPath+KeyStageSuffix && fs.data[adminPath+KeyStageSuffix] != nil {
						fs.data[adminPath+KeyStageSuffix] = []byte("corrupt")
					}
				}
			case "verify-old":
				fs.hook = func(op string) {
					if op == "read "+adminPath+KeyPriorSuffix && fs.data[adminPath+KeyPriorSuffix] != nil {
						fs.data[adminPath+KeyPriorSuffix] = []byte("corrupt")
					}
				}
			case "replace", "replace-live":
				fs.fail = "replace " + adminPath + KeyStageSuffix
				fs.partial = failure == "replace-live"
			}
			if gen, err := u.Apply(adminPath, addEdit(t)); err == nil || gen != 0 {
				t.Fatal("failure acknowledged", gen, err)
			}
			if reloads != 0 || !bytes.Equal(fs.data[adminPath], old) {
				t.Fatal("failure changed active/persisted policy", reloads)
			}
			if len(fs.data) != 1 {
				t.Fatal("owned failed staging retained", fs.data)
			}
		})
	}
}
func TestKeyUpdateFailedReloadRestoresPriorPersistedContent(t *testing.T) {
	for _, exists := range []bool{true, false} {
		t.Run(fmt.Sprint(exists), func(t *testing.T) {
			fs, u, old := updateFixture(t, exists)
			u.Reload = func([]byte) (uint64, error) { return 0, errInjected }
			if _, err := u.Apply(adminPath, addEdit(t)); err == nil {
				t.Fatal("failed reload accepted")
			}
			if exists {
				if !bytes.Equal(fs.data[adminPath], old) {
					t.Fatal("old file not restored")
				}
			} else if _, ok := fs.data[adminPath]; ok {
				t.Fatal("bootstrap candidate not removed")
			}
		})
	}
}
func TestKeyUpdateConcurrentFileAndConfigNeverOverwritten(t *testing.T) {
	for _, when := range []string{"before-publish", "failed-reload-file", "failed-reload-config"} {
		t.Run(when, func(t *testing.T) {
			fs, u, _ := updateFixture(t, true)
			unrelated := []byte("# unrelated edit\n")
			changed := false
			if when == "before-publish" {
				fs.hook = func(op string) {
					if op == "create "+adminPath+KeyPriorSuffix {
						fs.data[adminPath] = unrelated
					}
				}
			} else {
				u.Reload = func([]byte) (uint64, error) {
					if when == "failed-reload-file" {
						fs.data[adminPath] = unrelated
					} else {
						changed = true
					}
					return 0, errInjected
				}
				u.Check = func() error {
					if changed {
						return errors.New("config revision changed")
					}
					return nil
				}
			}
			if _, err := u.Apply(adminPath, addEdit(t)); err == nil {
				t.Fatal("concurrent change accepted")
			}
			if when != "failed-reload-config" && !bytes.Equal(fs.data[adminPath], unrelated) {
				t.Fatal("unrelated file overwritten")
			}
			if when == "failed-reload-config" {
				keys, err := ParseAuthorizedKeys(fs.data[adminPath])
				if err != nil || !keys.Matches(testPublic(2)) {
					t.Fatal("rollback overwrote content after revision changed", err)
				}
			}
		})
	}
}
func TestKeyUpdateConfigChangeBeforePublish(t *testing.T) {
	fs, u, old := updateFixture(t, true)
	changed := false
	fs.hook = func(op string) {
		if op == "create "+adminPath+KeyPriorSuffix {
			changed = true
		}
	}
	u.Check = func() error {
		if changed {
			return errInjected
		}
		return nil
	}
	if _, err := u.Apply(adminPath, addEdit(t)); err == nil {
		t.Fatal("config race")
	}
	if !bytes.Equal(fs.data[adminPath], old) {
		t.Fatal("config race published")
	}
}
func TestKeyUpdateStagingCollisionAndInvalidCandidateDoNotWrite(t *testing.T) {
	for _, name := range []string{adminPath + KeyStageSuffix, adminPath + KeyPriorSuffix} {
		fs, u, _ := updateFixture(t, true)
		fs.data[name] = []byte("unrelated")
		if _, err := u.Apply(adminPath, addEdit(t)); err == nil {
			t.Fatal("collision")
		}
		if string(fs.data[name]) != "unrelated" {
			t.Fatal("pre-existing staging overwritten")
		}
	}
	fs, u, old := updateFixture(t, true)
	if _, err := u.Apply(adminPath, func([]byte) ([]byte, error) { return []byte("malformed"), nil }); err == nil {
		t.Fatal("bad full candidate")
	}
	if len(fs.data) != 1 || !bytes.Equal(fs.data[adminPath], old) {
		t.Fatal("malformed candidate wrote")
	}
	fs.data[adminPath] = []byte("malformed")
	if _, err := u.Apply(adminPath, addEdit(t)); err == nil {
		t.Fatal("bad existing file")
	}
}
func TestKeyUpdateRollbackFailureReported(t *testing.T) {
	fs, u, old := updateFixture(t, true)
	u.Reload = func([]byte) (uint64, error) { fs.fail = "replace " + adminPath + KeyPriorSuffix; return 0, errInjected }
	if gen, err := u.Apply(adminPath, addEdit(t)); err == nil || gen != 0 {
		t.Fatal("rollback failure hidden", gen, err)
	}
	if !bytes.Equal(fs.data[adminPath+KeyPriorSuffix], old) {
		t.Fatal("verified prior erased after failed rollback")
	}
}

func TestKeyUpdateNewConfiguredPathUsesScopedCandidateReader(t *testing.T) {
	fs, u, old := updateFixture(t, true)
	path := "/home/user/.ssh/another"
	u.Reload = func(candidate []byte) (uint64, error) {
		loaded, err := fs.Read(path, MaxAuthorizedKeysBytes)
		if err != nil || !bytes.Equal(loaded, candidate) {
			return 0, errors.New("new path candidate unavailable")
		}
		return 2, nil
	}
	if gen, err := u.Apply(path, addEdit(t)); err != nil || gen != 2 {
		t.Fatal("missing new configured path", gen, err)
	}
	if !bytes.Equal(fs.data[adminPath], old) {
		t.Fatal("old configured file overwritten")
	}
}
