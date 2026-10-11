// Package testscratch creates host-test fixtures in the ignored GoOS build tree.
package testscratch

import (
	"testing"

	"goos/internal/repobuild"
)

func newAt(root, prefix string) (string, error) {
	return repobuild.NewScratch(root, prefix)
}

// TempDir returns an absolute unique child of build/tmp and removes it when the
// test finishes. It rejects redirected build or scratch directories.
func TempDir(t testing.TB, prefix string) string {
	t.Helper()
	root, err := repobuild.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	directory, err := newAt(root, prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repobuild.RemoveScratch(root, directory); err != nil {
			t.Errorf("remove test scratch %s: %v", directory, err)
		}
	})
	return directory
}
