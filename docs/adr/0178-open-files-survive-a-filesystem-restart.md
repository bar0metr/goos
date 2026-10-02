# ADR 0178: Open files survive a filesystem service restart

## Status

Accepted. Reverses the part of ADR 0119 that let a FAT32 service restart
invalidate open handles, and restores the ADR 0096 promise ADR 0119 withdrew.

## Context

ADR 0119 gave every file open on FAT32 a live object cookie from the service,
so a handle follows its file across renames. The cookies live in the
service's memory and die with it, and ADR 0119 accepted that: callers reopen
the path after PID 1 attaches the restarted service.

Since then the installed system runs from the volume (ADR 0175), and a
service's image is paged in from its file as the service touches it
(ADR 0106). The shell holds its image file open for its whole life and has no
path to reopen. After `fault fat32`, the first page of the shell not yet read
came back `StatusNotFound` from the restarted service, the kernel retired the
shell, and the command typed during its restart was lost. A page touched while
the service was down failed the same way at once. The boot test's check that
the restarted service serves the volume passed only when earlier commands had
happened to read that page; a replay of the steps around the fault failed this
way on every run.

## Decision

**The VFS acquires a new object for every open file when a restarted service
is attached.** Each open file already records `identity`, the place its name
has on the volume, and a rename moves it with the name; an open file cannot be
removed (ADR 0119). On reattach the VFS first releases every old cookie, which
the new instance does not know, so only the VFS's cached attributes and pages
for it go; then it acquires an object for each file's identity. Releasing all
old cookies before acquiring any keeps an old cookie that happens to equal a
new one from releasing it.

**A page of an image waits for the filesystem.** When the VFS answers a page
request with `StatusPeerDied` or `StatusTimeout`, the faulting task stays
parked and the kernel sends the request again ten times a second, for up to
ten seconds from the first such answer. Any other answer, or a request that
cannot be sent, ends the wait as before.

## Consequences

A handle opened before a FAT32 restart reads and writes the same file after
it, whether or not the file was renamed in between, and a service paging its
image from the volume survives the restart of the filesystem under it. A file
whose identity the new instance cannot resolve answers `StatusNotFound`, as
every old handle did before.

A process that touches an unread page while the filesystem is down blocks
until the service is back or ten seconds pass, instead of being retired at
once.
