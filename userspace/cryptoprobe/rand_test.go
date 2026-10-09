//go:build cryptoprobe_rand && !goos_userspace

package cryptoprobe

import "testing"

func TestRandRequiresKernel(t *testing.T) {
	if err := Run(); err == nil {
		t.Fatal("host probe unexpectedly obtained kernel entropy")
	}
}
