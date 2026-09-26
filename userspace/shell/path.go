package shell

import "goos/internal/limits"

// MaxPath is the native path limit (ADR 0110).
const MaxPath = limits.PathMax

// ResolvePath canonicalizes an absolute or cwd-relative path into target.
func ResolvePath(cwd []byte, path []byte, target *[MaxPath]byte) ([]byte, bool) {
	if target == nil || len(cwd) == 0 || cwd[0] != '/' || len(path) == 0 {
		return nil, false
	}
	var joined [MaxPath * 2]byte
	size := 0
	if path[0] != '/' {
		if len(cwd)+1+len(path) > len(joined) {
			return nil, false
		}
		size += copy(joined[size:], cwd)
		if size > 1 {
			joined[size] = '/'
			size++
		}
	} else if len(path) > len(joined) {
		return nil, false
	}
	size += copy(joined[size:], path)

	target[0] = '/'
	out := 1
	starts := [MaxPath]uint16{}
	count := 0
	for position := 1; position < size; {
		for position < size && joined[position] == '/' {
			position++
		}
		start := position
		for position < size && joined[position] != '/' {
			position++
		}
		component := joined[start:position]
		if len(component) == 0 || len(component) == 1 && component[0] == '.' {
			continue
		}
		if len(component) == 2 && component[0] == '.' && component[1] == '.' {
			if count != 0 {
				count--
				out = int(starts[count])
			}
			continue
		}
		if len(component) > limits.NameMax || out+len(component)+1 > len(target) {
			return nil, false
		}
		starts[count] = uint16(out)
		count++
		if out > 1 {
			target[out] = '/'
			out++
		}
		copy(target[out:], component)
		out += len(component)
	}
	return target[:out], true
}

// TooLong reports whether path is refused for its length: longer than
// MaxPath, or with a component longer than the native name limit.
func TooLong(path []byte) bool {
	if len(path) > MaxPath {
		return true
	}
	start := 0
	for index := 0; index <= len(path); index++ {
		if index == len(path) || path[index] == '/' {
			if index-start > limits.NameMax {
				return true
			}
			start = index + 1
		}
	}
	return false
}
