package shell

import (
	"goos/internal/limits"
	"strings"
	"testing"
)

func TestResolvePath(t *testing.T) {
	tests := []struct{ cwd, path, want string }{
		{"/", "system", "/system"},
		{"/system", "motd", "/system/motd"},
		{"/system", "../tmp", "/tmp"},
		{"/", "../../system//./motd", "/system/motd"},
		{"/system", "/tmp", "/tmp"},
	}
	for _, test := range tests {
		var target [MaxPath]byte
		got, ok := ResolvePath([]byte(test.cwd), []byte(test.path), &target)
		if !ok || string(got) != test.want {
			t.Errorf("ResolvePath(%q, %q) = %q, %v", test.cwd, test.path, got, ok)
		}
	}
}

func TestResolvePathNativeLimits(t *testing.T) {
	var target [MaxPath]byte
	name := strings.Repeat("n", limits.NameMax)
	if got, ok := ResolvePath([]byte("/"), []byte(name), &target); !ok || len(got) != limits.NameMax+1 {
		t.Fatalf("a %d-byte name refused", limits.NameMax)
	}
	if _, ok := ResolvePath([]byte("/"), []byte(name+"n"), &target); ok {
		t.Fatal("a name past NameMax accepted")
	}
	deep := strings.Repeat("/"+strings.Repeat("d", 200), 20)
	if got, ok := ResolvePath([]byte("/"), []byte(deep), &target); !ok || len(got) != len(deep) {
		t.Fatalf("a %d-byte path refused", len(deep))
	}
	if _, ok := ResolvePath([]byte("/"), []byte(deep+"/"+strings.Repeat("e", 100)), &target); ok {
		t.Fatal("a path past PathMax accepted")
	}
}

func TestJoinNameParentAndBaseName(t *testing.T) {
	var target [MaxPath]byte
	if path, ok := JoinName([]byte("/"), []byte("tmp"), &target); !ok || string(path) != "/tmp" {
		t.Fatalf("join root = %q %v", path, ok)
	}
	if path, ok := JoinName([]byte("/tmp/dir"), []byte("Файл.txt"), &target); !ok || string(path) != "/tmp/dir/Файл.txt" {
		t.Fatalf("join = %q %v", path, ok)
	}
	for _, name := range []string{"", ".", "..", "a/b", "a\x00b", strings.Repeat("n", limits.NameMax+1)} {
		if _, ok := JoinName([]byte("/tmp"), []byte(name), &target); ok {
			t.Fatalf("joined invalid name %q", name)
		}
	}
	long := "/" + strings.Repeat("d", MaxPath-3)
	if _, ok := JoinName([]byte(long), []byte("xy"), &target); ok {
		t.Fatal("joined past PathMax")
	}
	if path, ok := JoinName([]byte(long), []byte("x"), &target); !ok || len(path) != MaxPath {
		t.Fatalf("join at PathMax = %d %v", len(path), ok)
	}
	for path, want := range map[string]string{"/": "/", "/tmp": "/", "/tmp/a": "/tmp", "/a/b/c": "/a/b"} {
		if got := Parent([]byte(path), &target); string(got) != want {
			t.Fatalf("parent %q = %q", path, got)
		}
	}
	for path, want := range map[string]string{"/": "/", "/tmp": "tmp", "/a/Ёж": "Ёж"} {
		if got := BaseName([]byte(path)); string(got) != want {
			t.Fatalf("base %q = %q", path, got)
		}
	}
}
