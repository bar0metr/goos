package linuxfixtures

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	linuxelf "goos/kernel/compat/linux/elf"
)

func TestFixturesMatchTheirPinnedChecksums(t *testing.T) {
	for _, fixture := range All() {
		sum := sha256.Sum256(fixture.Image)
		if got := hex.EncodeToString(sum[:]); got != fixture.SHA256 {
			t.Errorf("prebuilt/%s is %s, pinned %s", fixture.Name, got, fixture.SHA256)
		}
	}
}

// The kernel loader must accept every fixture as built: a static image with
// no writable and executable segment and a non-executable stack. One
// fixture is position-independent and loads at the fixed base; the rest
// stay at the addresses they name.
func TestFixturesAreAcceptedStaticELF(t *testing.T) {
	for _, fixture := range All() {
		image, failure := linuxelf.Parse(fixture.Image, linuxelf.GoOSConfig())
		if failure != linuxelf.OK {
			t.Errorf("%s: parse failure %d", fixture.Name, failure)
			continue
		}
		wantBase := uint64(0)
		if fixture.Name == "hello-pie" {
			wantBase = linuxelf.StaticPIEBase
		}
		if image.Base != wantBase || image.DynamicPresent != (wantBase != 0) || image.Entry < wantBase {
			t.Errorf("%s: base=%#x dynamic=%v entry=%#x", fixture.Name, image.Base, image.DynamicPresent, image.Entry)
		}
		executable := 0
		for index := 0; index < image.SegmentCount; index++ {
			segment := image.Segments[index]
			if segment.Executable() && segment.Writable() {
				t.Errorf("%s: segment %d is writable and executable", fixture.Name, index)
			}
			if segment.Executable() {
				executable++
			}
		}
		if executable != 1 || !image.GNUStackPresent {
			t.Errorf("%s: executable segments=%d stack header=%v", fixture.Name, executable, image.GNUStackPresent)
		}
		// The two programs with thread-local variables carry a TLS image
		// and the RELRO range that holds it.
		if (fixture.Name == "musl-hello" || fixture.Name == "abi-probe") && (!image.TLS.Present || !image.RELROPresent) {
			t.Errorf("%s: TLS=%v RELRO=%v", fixture.Name, image.TLS.Present, image.RELROPresent)
		}
	}
}
