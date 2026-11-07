//go:build uefi

package main

// Linux image placement (ADR 0219). A static position-independent image gets
// a base drawn from the kernel generator each time a process launches or
// execs it; an ET_EXEC image loads where it is linked.

import (
	"encoding/binary"

	linuxcompat "goos/kernel/compat/linux"
	linuxelf "goos/kernel/compat/linux/elf"
	kernelconsole "goos/kernel/console"
	"goos/kernel/diag"
)

type linuxPlacement uint8

const (
	linuxPlaced linuxPlacement = iota
	// linuxPlacementInvalid is an image the parser refuses at the window
	// start, before any base is drawn.
	linuxPlacementInvalid
	// linuxPlacementNoRoom is an image that, with its brk reservation, fits
	// no base in the window.
	linuxPlacementNoRoom
	// linuxPlacementNoRandom is a generator that refused output.
	linuxPlacementNoRandom
	// linuxPlacementRelocatedInvalid is an image the parser refuses at the
	// drawn base. PlacePIE rules this out; the second parse checks anyway.
	linuxPlacementRelocatedInvalid
)

// placeLinuxImage parses a Linux executable into image at its final
// addresses. The caller has built nothing for the new image yet, so a
// failure leaves no state behind. Launches and execs are serialized, as the
// parser's scratch table requires.
func placeLinuxImage(image *linuxelf.Image, source linuxelf.Source) linuxPlacement {
	if linuxelf.ParseInto(image, source, linuxelf.GoOSConfig(linuxelf.StaticPIEWindowStart)) != linuxelf.OK {
		return linuxPlacementInvalid
	}
	if image.Base == 0 {
		return linuxPlaced
	}
	placement, ok := linuxelf.PlacePIE(image, linuxcompat.BrkReserve)
	if !ok {
		*image = linuxelf.Image{}
		tracePlacementFailure(linuxPlacementNoRoom)
		return linuxPlacementNoRoom
	}
	local := thisCPU()
	stirRandom()
	if !randomBytes(local.linuxPlacementDraw[:]) {
		*image = linuxelf.Image{}
		tracePlacementFailure(linuxPlacementNoRandom)
		return linuxPlacementNoRandom
	}
	base := placement.Base(binary.LittleEndian.Uint64(local.linuxPlacementDraw[:]))
	clear(local.linuxPlacementDraw[:])
	if linuxelf.ParseInto(image, source, linuxelf.GoOSConfig(base)) != linuxelf.OK || image.Base != base {
		*image = linuxelf.Image{}
		tracePlacementFailure(linuxPlacementRelocatedInvalid)
		return linuxPlacementRelocatedInvalid
	}
	if diag.Enabled {
		out := diagWriter{console: kernelconsole.New()}
		out.text("linux pie")
		out.hex("base", base)
		out.hex("span", image.LoadEnd()-base)
		out.hex("slots", placement.Count)
		out.end()
	}
	return linuxPlaced
}

func tracePlacementFailure(failure linuxPlacement) {
	if !diag.Enabled {
		return
	}
	out := diagWriter{console: kernelconsole.New()}
	switch failure {
	case linuxPlacementNoRoom:
		out.text("linux pie placement failed: no room")
	case linuxPlacementNoRandom:
		out.text("linux pie placement failed: random source unavailable")
	default:
		out.text("linux pie placement failed: invalid after relocation")
	}
	out.end()
}
