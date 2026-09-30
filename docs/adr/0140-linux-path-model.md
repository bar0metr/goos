# ADR 0140: Linux path model

## Status

Accepted.

## Context

A Linux program names files with absolute paths, paths relative to its working
directory, and spellings such as `a/../b`, `./a`, and `a/`. The VFS takes
absolute paths of at most 4096 bytes with components of at most 1024. The
compatibility layer has to turn one into the other without inventing a name
the program did not write.

## Decision

Linux paths resolve in the GoOS VFS namespace. `/` is the VFS root. There is
no chroot and no mount namespace.

Each Linux process has a working directory in its kernel-side compatibility
state. It starts as the directory PID 1 supplied at launch, stored in canonical
form. A launch whose working directory is relative or too long is refused.

Resolution is one pure, bounded pass over the working directory and the path:

- An absolute path ignores `dirfd`. A relative path starts at the working
  directory when `dirfd` is `AT_FDCWD`. No descriptor names a directory yet,
  so another `dirfd` is `ENOTDIR` when it is open and `EBADF` when it is not.
- Empty components and `.` vanish. `..` removes the component before it and
  stays at the root when none is left, as on Linux. GoOS has no symbolic
  links, so removing `name/..` by text names the object a walk would reach.
- The result is absolute, has no trailing slash, and never exceeds 4096 bytes.
- A path that ends in a slash, `.`, or `..` demands a directory. The canonical
  form cannot carry that spelling, so the resolver reports the demand and the
  caller enforces it. `openat` opens regular files only and refuses such a
  path with `EOPNOTSUPP`, as it refuses `O_DIRECTORY`.

The layer refuses a path it cannot hold and shortens nothing:

| Input | Result |
| --- | --- |
| empty path | `ENOENT` |
| no terminator within 4096 bytes | `ENAMETOOLONG` |
| component over 1024 bytes | `ENAMETOOLONG` |
| resolved path over 4096 bytes at any step | `ENAMETOOLONG` |
| unreadable path memory | `EFAULT` |

The length check runs at each step. A detour that a later `..` would shorten
under the limit is refused too, which keeps the pass free of scratch storage
larger than its output. Names the VFS or a filesystem rejects come back as
the VFS status; the layer substitutes no character and folds no case.

A resolved path of at most 216 bytes travels in the request message. A longer
one travels in a one-page memory object attached to the request, the transport
native clients use. The kernel creates the object, charges the process for it,
and moves the handle to the VFS with the request; the charge returns when the
VFS closes the handle.

The legacy `open` call is `openat` from the working directory. musl uses it
for `open()` on x86_64.

## Consequences

Linux programs see one namespace, the one native programs see. Every path the
VFS receives from the layer is canonical and bounded, and the working
directory needs no revalidation per call. Text normalization succeeds for
`missing/../file`, where Linux reports `ENOENT`; GoOS accepts that difference.
`chdir` and `getcwd` can build on the stored working directory.
