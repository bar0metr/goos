package linuxfixtures

import (
	"embed"
	"io/fs"
)

// The compiled terminfo entries Linux programs on GoOS find under
// /system/linux/terminfo: goos for the console, and xterm, xterm-256color,
// and vt100 for remote terminals. terminfo/build.sh compiles them.
//
//go:embed terminfo/db
var termInfo embed.FS

// TermInfoFile is one compiled entry, at its path below the terminfo root,
// such as g/goos.
type TermInfoFile struct {
	Path  string
	Image []byte
}

// TermInfo lists the compiled entries in name order.
func TermInfo() []TermInfoFile {
	var files []TermInfoFile
	fs.WalkDir(termInfo, "terminfo/db", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		image, readErr := termInfo.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files = append(files, TermInfoFile{Path: path[len("terminfo/db/"):], Image: image})
		return nil
	})
	return files
}
