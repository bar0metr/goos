# ADR 0106: Lazily loaded executables

## Status

Accepted. Extends ADR 0097 (executable objects) and ADR 0081 (shared image
pages).

## Context

An executable object held its frames in a fixed array of 256 pages, so no
program outside the system image could be larger than 1 MiB, and only two
objects could exist at once. Launching from an object, or from the system
image, copied every page of every writable segment, and every page of an
object's segments, into the new process before it ran: a program was
duplicated whole at every launch whatever part of it would ever run. The
native image loader capped an image at 256 pages for the same reason. None of
that suits the programs the Linux compatibility layer will have to start
(GoOS 0.1.3 specification §3).

## Decision

**Objects keep their frames in page lists.** `kernel/mm/pagelist` stores the
frame addresses of an object in index pages, 512 to a page, taken from the page
allocator as it grows, with a fixed root of 128 index pages: 256 MiB per
object. An object costs its data pages plus its index pages, charged to the
page quota of its creator, so in practice the quota bounds it. The table holds
16 objects, an intentional system limit that covers launches in flight and
programs still running from an object. `ExecutableWrite` appends up to 1 MiB
per call.

**Launch loads nothing private.** Every segment becomes a pinned image region
that records its origin — the system image extent's key, or the executable
object's identity — the offset of its file bytes, and how many there are
(ADR 0104). When the process first touches a page, the fault path allocates a
zeroed page, copies the page's file bytes from the origin, and maps it with
the segment's permissions; a page past the file bytes stays zero. The origin
is immutable while the process can read it: the system image for the whole
boot, a sealed object for as long as the process holds its reference. The
read-only segments of system image programs are still shared frames filled
once (ADR 0081).

**Objects are reference counted, and the user pays.** The creator's handle is
one reference and every process started from the object holds another,
dropped when it exits; the object is freed with the last. The first process
started from an object takes its charge over from the creator, since it is the
one that keeps reading the pages, and refuses to start if its own quota cannot
take it.

**The image format has no page cap.** A segment is bounded by the image area,
its file bytes by the file, and the pages it costs by the quota of the process
that touches them.

## Alternatives

- **Mapping an object's frames directly.** Segment payloads are packed in the
  file without page alignment, so frames do not line up with pages of the
  address space; aligning them would change the format for no gain over a copy
  of only the pages touched.
- **Copying the whole program at launch into private pages and freeing the
  object.** Simpler lifetime, but it is exactly the whole-image duplication
  this removes.
- **Keeping the charge with the creator.** PID 1 would pay for every running
  program started from a file and could be exhausted by them.

## Consequences

- A program's first touch of each page costs a fault and a page copy; a program
  that never touches most of itself never pays for it. The boot test starts a
  program padded with three MiB it never reads.
- A process started from a file keeps its object alive; closing PID 1's handle
  after the launch no longer frees it.
- The kernel's validation self-test images, loaded without a region map, are
  still loaded whole.
