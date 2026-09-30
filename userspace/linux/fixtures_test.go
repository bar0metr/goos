package linuxfixtures

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	linuxelf "goos/kernel/compat/linux/elf"
)

func TestMuslHelloMatchesItsPinnedChecksum(t *testing.T) {
	sum := sha256.Sum256(MuslHello)
	if got := hex.EncodeToString(sum[:]); got != MuslHelloSHA256 {
		t.Fatalf("prebuilt/musl-hello is %s, pinned %s", got, MuslHelloSHA256)
	}
}

// The kernel loader must accept the fixture as built: a static, non-PIE
// image with a TLS segment, a RELRO range, and a non-executable stack.
func TestMuslHelloIsAnAcceptedStaticELF(t *testing.T) {
	image, failure := linuxelf.Parse(MuslHello, linuxelf.GoOSConfig())
	if failure != linuxelf.OK {
		t.Fatalf("parse failure %d", failure)
	}
	executable, writable := 0, 0
	for index := 0; index < image.SegmentCount; index++ {
		segment := image.Segments[index]
		if segment.Executable() && segment.Writable() {
			t.Fatalf("segment %d is writable and executable", index)
		}
		if segment.Executable() {
			executable++
		}
		if segment.Writable() {
			writable++
		}
	}
	if executable != 1 || writable == 0 {
		t.Fatalf("segments: executable=%d writable=%d", executable, writable)
	}
	if !image.TLS.Present || !image.RELROPresent || !image.GNUStackPresent || image.TLS.FileSize == 0 || image.TLS.MemorySize <= image.TLS.FileSize {
		t.Fatalf("TLS=%#v relro=%v stack=%v", image.TLS, image.RELROPresent, image.GNUStackPresent)
	}
}
