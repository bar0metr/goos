package linux

import (
	"goos/internal/limits"
	"goos/kernel/compat/linux/abi"
)

// ResolvePath writes to target the canonical absolute path that path names
// from the working directory cwd, and returns its length. The root is the
// GoOS VFS root: there is no chroot and no mount namespace.
//
// The result has no empty, "." or ".." component and no trailing slash. A
// ".." at the root stays at the root, as on Linux. GoOS has no symbolic
// links, so removing "name/.." by text names the same object a walk would.
//
// directory reports that the path demands a directory: it ends in a slash or
// in a "." or ".." component. The canonical form drops that spelling, so the
// caller must enforce the demand instead of letting "file/" name a file.
//
// A path GoOS cannot hold fails with ENAMETOOLONG and is never shortened: a
// component over limits.NameMax, or a result over the target, which callers
// size to limits.PathMax. The check applies to each step, so a long detour
// that ".." would later shorten is refused too.
func ResolvePath(target []byte, cwd []byte, path []byte) (size int, directory bool, errno abi.Errno) {
	if len(path) == 0 {
		return 0, false, abi.ENOENT
	}
	if len(path) >= abi.PathMax {
		return 0, false, abi.ENAMETOOLONG
	}
	if len(target) == 0 || len(target) > limits.PathMax {
		return 0, false, abi.EINVAL
	}
	if path[0] != '/' {
		if len(cwd) == 0 || cwd[0] != '/' {
			return 0, false, abi.ENOENT
		}
		if size, _, errno = appendComponents(target, 0, cwd); errno != 0 {
			return 0, false, errno
		}
	}
	size, directory, errno = appendComponents(target, size, path)
	if errno != 0 {
		return 0, false, errno
	}
	if size == 0 {
		target[0] = '/'
		return 1, true, 0
	}
	return size, directory, 0
}

// appendComponents applies the components of source to the canonical path in
// target[:size]. An empty canonical path stands for the root.
func appendComponents(target []byte, size int, source []byte) (int, bool, abi.Errno) {
	directory := false
	for position := 0; position < len(source); {
		if source[position] == '/' {
			position++
			directory = true
			continue
		}
		start := position
		for position < len(source) && source[position] != '/' {
			if source[position] == 0 {
				return 0, false, abi.EINVAL
			}
			position++
		}
		component := source[start:position]
		switch {
		case len(component) == 1 && component[0] == '.':
			directory = true
		case len(component) == 2 && component[0] == '.' && component[1] == '.':
			for size > 0 && target[size-1] != '/' {
				size--
			}
			if size > 0 {
				size--
			}
			directory = true
		default:
			if len(component) > limits.NameMax || len(component)+1 > len(target)-size {
				return 0, false, abi.ENAMETOOLONG
			}
			target[size] = '/'
			size += 1 + copy(target[size+1:], component)
			directory = false
		}
	}
	return size, directory, 0
}
