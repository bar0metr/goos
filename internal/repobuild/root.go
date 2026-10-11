// Package repobuild locates the GoOS repository and confines host build output.
package repobuild

import (
	"fmt"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maxParents = 64
const maxModuleBytes = 1 << 20

func goOSModule(data []byte) bool {
	var lexer scanner.Scanner
	lexer.Init(token.NewFileSet().AddFile("go.mod", -1, len(data)), data, nil, 0)
	start, found := true, false
	for {
		_, kind, text := lexer.Scan()
		if kind == token.EOF {
			return found && lexer.ErrorCount == 0
		}
		if start && kind == token.IDENT && text == "module" {
			if found {
				return false
			}
			_, kind, text = lexer.Scan()
			if kind == token.STRING && strings.HasPrefix(text, "\"") {
				var err error
				text, err = strconv.Unquote(text)
				if err != nil {
					return false
				}
			} else if kind != token.IDENT {
				return false
			}
			if text != "goos" {
				return false
			}
			_, end, _ := lexer.Scan()
			if end != token.SEMICOLON && end != token.EOF {
				return false
			}
			found, start = true, true
			if end == token.EOF {
				return lexer.ErrorCount == 0
			}
			continue
		}
		start = kind == token.SEMICOLON
	}
}

func moduleAt(root string) (bool, error) {
	path := filepath.Join(root, "go.mod")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxModuleBytes {
		return false, fmt.Errorf("GoOS module marker must be a regular file of at most %d bytes", maxModuleBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return false, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return false, fmt.Errorf("GoOS module marker changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxModuleBytes+1))
	if err != nil {
		return false, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if len(data) > maxModuleBytes || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return false, fmt.Errorf("GoOS module marker changed while reading")
	}
	return goOSModule(data), nil
}

// FindRoot accepts a repository or package directory and walks a bounded number
// of parents. It does not create build output.
func FindRoot(start string) (string, error) {
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	absolute, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	current, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(current)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("GoOS repository lookup requires a directory: %s", current)
	}
	for depth := 0; depth < maxParents; depth++ {
		matches, err := moduleAt(current)
		if err != nil {
			return "", err
		}
		if matches {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", fmt.Errorf("no GoOS module root found within %d parent directories", maxParents)
}

// BuildDir validates an explicit module root, including synthetic test roots.
func BuildDir(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	matches, err := moduleAt(real)
	if err != nil {
		return "", err
	}
	if !matches {
		return "", fmt.Errorf("not a GoOS module root: %s", real)
	}
	build := filepath.Join(real, "build")
	if err := RealDirectory(build); err != nil {
		return "", fmt.Errorf("GoOS build directory: %w", err)
	}
	return build, nil
}

// RealDirectory rejects a symlink at the directory or any resolved ancestor.
func RealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("not a real directory: %s", path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if real != filepath.Clean(path) {
		return fmt.Errorf("directory has a symlinked ancestor: %s", path)
	}
	return nil
}

// Contains receives an absolute, canonical output path. Missing descendants
// remain eligible; callers must check the destination and their writes.
func Contains(root, path string) bool {
	build, err := BuildDir(root)
	if err != nil || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	relative, err := filepath.Rel(build, path)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	current := build
	for _, name := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, name)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return true
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}
