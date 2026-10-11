package admincmd

import (
	"goos/internal/limits"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
)

const (
	FindDepth = 16
	FindWork  = 4096
)

type Visitor interface {
	Visit([]byte, ramfs.Kind, uint64, ramfs.Timestamp) bool
}
type Directory interface {
	ReadDir([]byte, Visitor) vfs.Status
}

// Finder owns each active directory path. Names borrowed from ReadDir are
// copied into the next frame before recursion can make another VFS call.
// The caller supplies the workspace; no buffer grows with the tree.
type Finder struct {
	paths       [FindDepth + 1][limits.PathMax]byte
	sizes       [FindDepth + 1]int
	depth, work int
	name        []byte
	directories Directory
	writer      Writer
	status      Status
	VFSStatus   vfs.Status
}

func (finder *Finder) Walk(root, name []byte, kind ramfs.Kind, directories Directory, writer Writer) Status {
	if len(root) == 0 || len(root) > limits.PathMax || len(name) > limits.NameMax {
		return LimitError
	}
	for _, c := range name {
		if c == '/' || c == 0 {
			return InputError
		}
	}
	finder.depth, finder.work, finder.status, finder.VFSStatus = 0, 0, OK, vfs.StatusOK
	finder.sizes[0] = copy(finder.paths[0][:], root)
	finder.name, finder.directories, finder.writer = name, directories, writer
	finder.node(kind)
	return finder.status
}

func (finder *Finder) node(kind ramfs.Kind) {
	finder.work++
	if finder.work > FindWork {
		finder.status = LimitError
		return
	}
	path := finder.paths[finder.depth][:finder.sizes[finder.depth]]
	base := path
	for index, c := range path {
		if c == '/' && index+1 < len(path) {
			base = path[index+1:]
		}
	}
	if len(finder.name) == 0 || same(base, finder.name) {
		if finder.status = finder.writer.Write(path); finder.status != OK {
			return
		}
		if finder.status = finder.writer.Write([]byte{'\n'}); finder.status != OK {
			return
		}
	}
	if kind != ramfs.KindDirectory {
		return
	}
	if status := finder.directories.ReadDir(path, finder); status != vfs.StatusOK && finder.status == OK {
		finder.status, finder.VFSStatus = InputError, status
	}
}

func (finder *Finder) Visit(name []byte, kind ramfs.Kind, _ uint64, _ ramfs.Timestamp) bool {
	if finder.status != OK {
		return false
	}
	if same(name, []byte(".")) || same(name, []byte("..")) {
		finder.work++
		if finder.work > FindWork {
			finder.status = LimitError
			return false
		}
		return true
	}
	if len(name) > limits.NameMax {
		finder.status = LimitError
		return false
	}
	if finder.depth == FindDepth {
		finder.status = LimitError
		return false
	}
	for _, c := range name {
		if c == '/' || c == 0 {
			finder.status = InputError
			return false
		}
	}
	if len(name) == 0 {
		finder.status = InputError
		return false
	}
	parent := finder.paths[finder.depth][:finder.sizes[finder.depth]]
	child := finder.paths[finder.depth+1][:]
	size := copy(child, parent)
	if child[size-1] != '/' {
		if size == len(child) {
			finder.status = LimitError
			return false
		}
		child[size] = '/'
		size++
	}
	if len(name) > len(child)-size {
		finder.status = LimitError
		return false
	}
	size += copy(child[size:], name)
	finder.depth++
	finder.sizes[finder.depth] = size
	finder.node(kind)
	finder.depth--
	return finder.status == OK
}

func same(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for index, c := range a {
		if c != b[index] {
			return false
		}
	}
	return true
}
