# ADR 0123: GOP framebuffer graphical console

## Status

Accepted.

## Context

GoOS 0.1.4 exposes its interactive terminal through COM1. That path is useful
for diagnostics and automation but does not make the QEMU graphical window an
interactive system console. GoOS 0.1.5 needs graphical text output and keyboard
input without introducing a GUI, a second TTY policy implementation, or a
firmware dependency after kernel ownership begins.

UEFI Graphics Output Protocol (GOP) supplies a discoverable linear framebuffer
and describes its current pixel layout. VGA text mode is a legacy, fixed-format
interface that does not fit the UEFI boot path. Virtio-GPU would require a
transport, command queues, and a graphics driver far beyond a text console.
Neither alternative supplies the minimal firmware handoff GoOS needs.

GOP supplies output memory only. It does not provide keyboard input after
`ExitBootServices`. QEMU's emulated i8042 controller remains independently
available and is sufficient for the single keyboard required by this release.

## Decision

The UEFI boot boundary locates GOP, queries every bounded mode, and selects the
first usable mode in this order: 1024x768, 1280x720, 1280x800, 800x600, the
current valid mode, then another valid mode. A mode must expose a directly
writable 32-bit linear framebuffer. `PixelBltOnly`, invalid geometry,
overlapping masks, overflowing ranges, and framebuffer storage beyond the
bounded descriptor are rejected.

All GOP operations happen before `ExitBootServices`. Boot code copies only the
physical address, size, geometry, stride, normalized pixel format, and masks
into `boot.Info`. It retains no protocol pointer, firmware mode record, or
function pointer. Later kernel phases create a supervisor-only, writable, NX
mapping and render through that permanent mapping; they never call GOP.

Rendering stays in the kernel for 0.1.5 because the framebuffer is boot device
memory required before userspace and during panic handling. Ordinary userspace
does not receive its physical address or a mapping capability. The renderer is
limited to pixels, glyphs, UTF-8 decoding, scrolling, and the bounded control
sequence subset needed by the existing terminal stream.

Terminal policy stays in the userspace TTY. Line editing, foreground-session
selection, control-key policy, job control, prompts, and shell state do not
move into the kernel. Existing applications continue to use the capability-
protected `ConsoleWrite` and `ConsoleRead` operations without a framebuffer-
specific ABI.

QEMU keyboard input uses an interrupt-driven PS/2/i8042 driver. USB HID,
virtio-input, and keyboard-layout configuration remain outside this release.
Decoded bytes join the same logical console input queue as COM1, so the TTY
retains one input interface and one line discipline.

COM1 remains initialized first and remains independent. Normal console output
will be mirrored to serial and framebuffer, while early failures, panic,
headless tests, recovery, and diagnostics always remain observable through
serial. Missing or invalid GOP only prints a serial warning and leaves the
system serial-only. Missing PS/2 similarly leaves serial input usable.

## Consequences

The firmware boundary grows by one bounded discovery step and one immutable
descriptor. Graphics cannot prevent boot merely by being absent or malformed.
Mode selection is deterministic without depending on an OVMF mode number.

The kernel maps the framebuffer permanently at `0xffff808000000000` in PML4
slot 257, separate from the direct map and high entry mappings. Only the
page-aligned visible stride range is present. Leaves are supervisor-only,
writable, NX, and uncached through PCD+PWT. Process roots do not inherit the
slot. Graphics is disabled before PMM initialization if the range overlaps
UEFI conventional memory, preventing framebuffer pages from entering the free
pool. The kernel establishes the architectural PAT layout with caches disabled
and flushed before activating the mapping, so UC does not depend on firmware
PAT state. Firmware identity mappings are never the steady-state graphics path.

The low-level `kernel/framebuffer` package owns the only unsafe conversion from
the mapped address to pixels. Construction revalidates geometry and backing
length. Pixel, clear, and rectangle operations clip to visible width and
height, never touch stride padding, and allocate nothing. The first graphical
milestone clears the screen to dark blue and draws a white band plus red,
green, and blue bars after CR3 replacement. Fonts and terminal parsing remain
later phases.

The release will contain one graphical text console, not a general graphics
API. A future userspace graphics system would require a separate memory and
capability design rather than widening this boot descriptor into unrestricted
framebuffer access.
