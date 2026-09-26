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
