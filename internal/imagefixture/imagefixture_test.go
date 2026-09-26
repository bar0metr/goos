package imagefixture

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	image := make([]byte, 3<<20+100)
	copy(image[5000:], "data")
	copy(image[len(image)-3:], "end")
	path := filepath.Join(t.TempDir(), "image.gz")
	os.WriteFile(path, Pack(image), 0o644)
	file, _ := os.Create(filepath.Join(t.TempDir(), "image"))
	defer file.Close()
	if err := Unpack(path, file); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file.Name())
	if !bytes.Equal(got, image) {
		t.Fatal("round trip differs")
	}
}
