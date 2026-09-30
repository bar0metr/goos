package linux

import (
	"bytes"
	"strings"
	"testing"

	"goos/internal/limits"
	"goos/kernel/compat/linux/abi"
)

func resolve(cwd, path string) (string, bool, abi.Errno) {
	var target [limits.PathMax]byte
	size, directory, errno := ResolvePath(target[:], []byte(cwd), []byte(path))
	return string(target[:size]), directory, errno
}

func TestResolvePath(t *testing.T) {
	cases := []struct {
		cwd, path string
		want      string
		directory bool
	}{
		{"/", "/system/motd", "/system/motd", false},
		{"/home", "/system/motd", "/system/motd", false},
		{"/system/linux", "musl-hello", "/system/linux/musl-hello", false},
		{"/", "tmp/file", "/tmp/file", false},
		{"/system/linux", "./musl-hello", "/system/linux/musl-hello", false},
		{"/system/linux", "../motd", "/system/motd", false},
		{"/system/linux", "../../../../tmp/x", "/tmp/x", false},
		{"/", "//system///linux//./musl-hello", "/system/linux/musl-hello", false},
		{"/a/b", "c/../../d", "/a/d", false},
		{"/a/b", ".hidden", "/a/b/.hidden", false},
		{"/a/b", "...", "/a/b/...", false},
		{"/a/b", "..c", "/a/b/..c", false},
		// Every spelling below demands a directory.
		{"/a/b", ".", "/a/b", true},
		{"/a/b", "..", "/a", true},
		{"/a/b", "c/", "/a/b/c", true},
		{"/a/b", "c/.", "/a/b/c", true},
		{"/a/b", "c/d/..", "/a/b/c", true},
		{"/a", "/", "/", true},
		{"/a", "/..", "/", true},
		{"/a", "../..", "/", true},
		{"/a", "/tmp//", "/tmp", true},
	}
	for _, test := range cases {
		got, directory, errno := resolve(test.cwd, test.path)
		if errno != 0 || got != test.want || directory != test.directory {
			t.Errorf("cwd %q path %q = %q directory=%v errno=%d, want %q directory=%v", test.cwd, test.path, got, directory, errno, test.want, test.directory)
		}
	}
}

func TestResolvePathRefusesWhatItCannotHold(t *testing.T) {
	name := strings.Repeat("n", limits.NameMax)
	deep := strings.Repeat("/"+strings.Repeat("d", 255), 15) // 3840 bytes
	cases := []struct {
		label     string
		cwd, path string
		want      abi.Errno
	}{
		{"empty path", "/", "", abi.ENOENT},
		{"relative path without a base", "", "file", abi.ENOENT},
		{"relative base", "home", "file", abi.ENOENT},
		{"component at the name limit", "/", name, 0},
		{"component over the name limit", "/", name + "n", abi.ENAMETOOLONG},
		{"input at PATH_MAX with its terminator", "/", "/" + strings.Repeat("a/", 2047), 0},
		{"input over PATH_MAX", "/", "/" + strings.Repeat("a/", 2047) + "b", abi.ENAMETOOLONG},
		{"result at the path limit", deep, strings.Repeat("e", 255), 0},
		{"result over the path limit", deep, strings.Repeat("e", 256), abi.ENAMETOOLONG},
		{"detour a later .. would shorten", deep, strings.Repeat("e", 256) + "/..", abi.ENAMETOOLONG},
		{"embedded NUL", "/", "a\x00b", abi.EINVAL},
		{"embedded NUL in the base", "/a\x00", "b", abi.EINVAL},
	}
	for _, test := range cases {
		got, _, errno := resolve(test.cwd, test.path)
		if errno != test.want || errno != 0 && got != "" {
			t.Errorf("%s: %q errno=%d, want errno=%d", test.label, got, errno, test.want)
		}
	}
	var small [8]byte
	if _, _, errno := ResolvePath(small[:], nil, []byte("/12345678")); errno != abi.ENAMETOOLONG {
		t.Errorf("result over a small target: errno=%d", errno)
	}
	if size, _, errno := ResolvePath(small[:], nil, []byte("/1234567")); errno != 0 || size != 8 {
		t.Errorf("result filling a small target: size=%d errno=%d", size, errno)
	}
	var oversized [limits.PathMax + 1]byte
	if _, _, errno := ResolvePath(oversized[:], nil, []byte("/a")); errno != abi.EINVAL {
		t.Errorf("target over the path limit: errno=%d", errno)
	}
	if _, _, errno := ResolvePath(nil, nil, []byte("/a")); errno != abi.EINVAL {
		t.Errorf("no target: errno=%d", errno)
	}
}

// FuzzResolvePath checks the canonical form on arbitrary input: the result is
// absolute, within its bound, free of empty, "." and ".." components, and
// resolving it again changes nothing.
func FuzzResolvePath(fuzz *testing.F) {
	for _, seed := range [][2]string{{"/", "a"}, {"/a/b", "../c/./d/"}, {"/a", "/../../x//y"}, {"/", "."}, {"/a/b/c", "../../../.."}, {"x", "y"}} {
		fuzz.Add([]byte(seed[0]), []byte(seed[1]))
	}
	fuzz.Fuzz(func(t *testing.T, cwd, path []byte) {
		var target, again [limits.PathMax]byte
		size, directory, errno := ResolvePath(target[:], cwd, path)
		if errno != 0 {
			if size != 0 || directory {
				t.Fatalf("failure returned size=%d directory=%v", size, directory)
			}
			return
		}
		result := target[:size]
		if size == 0 || size > limits.PathMax || result[0] != '/' || bytes.IndexByte(result, 0) >= 0 {
			t.Fatalf("result %q", result)
		}
		if size > 1 {
			for _, component := range bytes.Split(result[1:], []byte("/")) {
				if len(component) == 0 || len(component) > limits.NameMax || string(component) == "." || string(component) == ".." {
					t.Fatalf("component %q in %q", component, result)
				}
			}
		}
		repeated, demand, errno := ResolvePath(again[:], []byte("/unused"), result)
		if errno != 0 || !bytes.Equal(again[:repeated], result) || demand != (size == 1) {
			t.Fatalf("second pass of %q = %q directory=%v errno=%d", result, again[:repeated], demand, errno)
		}
	})
}
