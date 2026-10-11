package repobuild

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const scratchModule = "module goos.build.scratch\n"

// NewScratch creates a unique child of root/build/tmp. root must be the exact
// canonical GoOS module root returned by FindRoot.
func NewScratch(root, prefix string) (string, error) {
	if err := validScratchPrefix(prefix); err != nil {
		return "", err
	}
	selected, err := FindRoot(root)
	if err != nil {
		return "", err
	}
	if selected != root {
		return "", fmt.Errorf("scratch allocation requires the canonical GoOS module root")
	}

	repo, err := openVerifiedRoot(root)
	if err != nil {
		return "", err
	}
	defer repo.Close()

	build, err := openScratchParent(repo, "build")
	if err != nil {
		return "", fmt.Errorf("open GoOS build directory: %w", err)
	}
	defer build.Close()
	tmp, err := openScratchParent(build, "tmp")
	if err != nil {
		return "", fmt.Errorf("open GoOS scratch directory: %w", err)
	}
	defer tmp.Close()

	if err := ensureScratchModule(tmp); err != nil {
		return "", err
	}
	return makeScratch(tmp, filepath.Join(root, "build", "tmp"), prefix)
}

// RemoveScratch removes a directory previously returned by NewScratch. It uses
// rooted descriptors rather than following the returned path.
func RemoveScratch(root, directory string) error {
	selected, err := FindRoot(root)
	if err != nil {
		return err
	}
	if selected != root {
		return fmt.Errorf("scratch removal requires the canonical GoOS module root")
	}
	relative, err := filepath.Rel(filepath.Join(root, "build", "tmp"), directory)
	if err != nil || relative == "." || relative == ".." || relative == "go.mod" || filepath.Base(relative) != relative || filepath.IsAbs(relative) || filepath.Clean(directory) != directory {
		return fmt.Errorf("not a scratch child: %s", directory)
	}

	repo, err := openVerifiedRoot(root)
	if err != nil {
		return err
	}
	defer repo.Close()
	build, err := openScratchParent(repo, "build")
	if err != nil {
		return fmt.Errorf("open GoOS build directory: %w", err)
	}
	defer build.Close()
	tmp, err := openScratchParent(build, "tmp")
	if err != nil {
		return fmt.Errorf("open GoOS scratch directory: %w", err)
	}
	defer tmp.Close()
	info, err := tmp.Lstat(relative)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("not a scratch directory: %s", directory)
	}
	return tmp.RemoveAll(relative)
}

func validScratchPrefix(prefix string) error {
	if filepath.IsAbs(prefix) || filepath.Base(prefix) != prefix || strings.Contains(prefix, "/") || strings.Contains(prefix, "\\") {
		return fmt.Errorf("scratch prefix must be a single path component")
	}
	return nil
}

func openVerifiedRoot(path string) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("not a real directory: %s", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	if !opened.IsDir() || !os.SameFile(before, opened) {
		root.Close()
		return nil, fmt.Errorf("directory changed while opening: %s", path)
	}
	if err := validateRootModule(root); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

func validateRootModule(root *os.Root) error {
	info, err := root.Lstat("go.mod")
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxModuleBytes {
		return fmt.Errorf("GoOS module marker must be a regular file of at most %d bytes", maxModuleBytes)
	}
	file, err := root.Open("go.mod")
	if err != nil {
		return err
	}
	opened, statErr := file.Stat()
	if statErr != nil {
		file.Close()
		return statErr
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		file.Close()
		return fmt.Errorf("GoOS module marker changed while opening")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxModuleBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	after, err := root.Lstat("go.mod")
	if err != nil {
		return err
	}
	if len(data) > maxModuleBytes || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return fmt.Errorf("GoOS module marker changed while reading")
	}
	if !goOSModule(data) {
		return fmt.Errorf("not a GoOS module root")
	}
	return nil
}

func openScratchParent(parent *os.Root, name string) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := parent.Mkdir(name, 0755); err != nil && !os.IsExist(err) {
			return nil, err
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("not a real directory: %s", name)
	}
	opened, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	current, err := opened.Stat(".")
	if err != nil {
		opened.Close()
		return nil, err
	}
	if !current.IsDir() || !os.SameFile(info, current) {
		opened.Close()
		return nil, fmt.Errorf("directory changed while opening: %s", name)
	}
	return opened, nil
}

func ensureScratchModule(root *os.Root) error {
	info, err := root.Lstat("go.mod")
	if errors.Is(err, fs.ErrNotExist) {
		var file *os.File
		file, err = root.OpenFile("go.mod", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return err
		}
		_, writeErr := io.WriteString(file, scratchModule)
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		info, err = root.Lstat("go.mod")
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("scratch module marker must be a regular file")
	}

	file, err := root.Open("go.mod")
	if err != nil {
		return err
	}
	opened, statErr := file.Stat()
	data, readErr := io.ReadAll(io.LimitReader(file, int64(len(scratchModule)+1)))
	closeErr := file.Close()
	after, lstatErr := root.Lstat("go.mod")
	if statErr != nil {
		return statErr
	}
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if lstatErr != nil {
		return lstatErr
	}
	if !opened.Mode().IsRegular() || !after.Mode().IsRegular() || !os.SameFile(info, opened) || !os.SameFile(opened, after) || string(data) != scratchModule {
		return fmt.Errorf("scratch module marker changed or is invalid")
	}
	return nil
}

func makeScratch(root *os.Root, parent, prefix string) (string, error) {
	for attempts := 0; attempts < 128; attempts++ {
		name := prefix + rand.Text()
		if err := root.Mkdir(name, 0700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		info, err := root.Lstat(name)
		if err != nil {
			_ = root.RemoveAll(name)
			return "", err
		}
		child, err := root.OpenRoot(name)
		if err == nil {
			var opened os.FileInfo
			opened, err = child.Stat(".")
			closeErr := child.Close()
			if err == nil {
				err = closeErr
			}
			if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !opened.IsDir() || !os.SameFile(info, opened)) {
				err = fmt.Errorf("scratch directory changed while opening")
			}
		}
		if err != nil {
			_ = root.RemoveAll(name)
			return "", err
		}
		return filepath.Join(parent, name), nil
	}
	return "", fmt.Errorf("could not allocate a unique scratch directory")
}
