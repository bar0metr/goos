package linuxfixtures

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	linuxcompat "goos/kernel/compat/linux"
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
// no writable and executable segment and a non-executable stack. Two
// fixtures are position-independent and move to the base they are parsed
// at; the rest stay at the addresses they name.
func TestFixturesAreAcceptedStaticELF(t *testing.T) {
	for _, fixture := range All() {
		image, failure := linuxelf.Parse(fixture.Image, linuxelf.GoOSConfig(linuxelf.StaticPIEWindowStart))
		if failure != linuxelf.OK {
			t.Errorf("%s: parse failure %d", fixture.Name, failure)
			continue
		}
		wantBase := uint64(0)
		if fixture.Name == "hello-pie" || fixture.Name == "pie-probe" {
			wantBase = linuxelf.StaticPIEWindowStart
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
		// The programs with thread-local variables carry a TLS image
		// and the RELRO range that holds it.
		if (fixture.Name == "musl-hello" || fixture.Name == "abi-probe" || fixture.Name == "pie-probe") && (!image.TLS.Present || !image.RELROPresent) {
			t.Errorf("%s: TLS=%v RELRO=%v", fixture.Name, image.TLS.Present, image.RELROPresent)
		}
	}
}

func TestTermInfoHoldsTheSupportedTerminals(t *testing.T) {
	found := map[string]bool{}
	for _, entry := range TermInfo() {
		if len(entry.Image) == 0 {
			t.Fatalf("%s is empty", entry.Path)
		}
		found[entry.Path] = true
	}
	for _, path := range []string{"g/goos", "x/xterm", "x/xterm-256color", "v/vt100"} {
		if !found[path] {
			t.Fatalf("%s missing from %v", path, found)
		}
	}
}

// Each position-independent fixture has room for well over 2^17 bases in
// the static-PIE window with its brk reservation (ADR 0219).
func TestPositionIndependentFixturesHaveManyPlacements(t *testing.T) {
	for _, fixture := range All() {
		image, failure := linuxelf.Parse(fixture.Image, linuxelf.GoOSConfig(linuxelf.StaticPIEWindowStart))
		if failure != linuxelf.OK || image.Base == 0 {
			continue
		}
		placement, ok := linuxelf.PlacePIE(&image, linuxcompat.BrkReserve)
		if !ok || placement.Step != linuxcompat.LinuxPageSize || placement.Count < 1<<17 {
			t.Errorf("%s: placement %+v %v", fixture.Name, placement, ok)
		}
		t.Logf("%s: %d bases", fixture.Name, placement.Count)
	}
}
