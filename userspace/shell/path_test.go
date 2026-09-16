package shell

import "testing"

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
