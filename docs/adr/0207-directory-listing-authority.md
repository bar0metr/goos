# ADR 0207: Directory listing authority work

## Status

Accepted and implemented.

## Context

Listing `/apps` with 119 entries on a persistent FAT32 volume took 1.41 s under
TCG. Every `ReadDir` request checked scopes three times: the service authorized
the path, `ReadDirFor` authorized it again, and `ReadDirFor` then checked the
returned entry. Each check walked every subtree scope on the backend and looked
up every exact scope, so listing `/apps` read `/state/security/ssh` over and
over although the two directories share nothing but a volume.

## Decision

### The namespace remembers scope decisions

`nodeScopes` keeps its last 16 answers per backend and node. An exact scope keeps
the node its name resolved to, or that the name was missing. Lookup failures stay
unremembered, so a check that cannot establish identity still fails closed.

The memos record which scopes hold a node. `permits` still applies the session's
grants on each request, so a changed authority takes effect at once.

### A listed entry takes its mask from its directory

`ReadDirFor` computes the directory's mask once and derives each entry's mask:

- The entry lies in a subtree scope when the directory does, or when it is the
  scope's root.
- The entry matches an exact scope only when the directory is the scope's parent
  and the scope's name resolves to the entry's node.

Comparison stays by node identity, so the backend's own rules decide case folding
and short-name aliases. An entry from the mount phase of a scan can alias any node
of its backend, so it still takes a full `nodeScopes` check.

### Invalidation

`Namespace.invalidate` clears the memos with the resolved paths. It runs on mount,
unmount, protection changes, scope installation, rename, and replace. Create and
unlink now invalidate on failure as well, because a backend can leave the name
behind when its durability step fails. The service invalidates when a filesystem
service attaches or detaches and when it creates a volume directory directly.
These paths are complete because the VFS is the filesystem service's only client
(ADR 0096).

### Diagnostics

A `DIAG=1` build prints one line when a directory scan ends:

```text
VFS_LISTING requests=120 scope_checks=373 scope_contains=3 scope_openname=1 scope_backend_readdir=9 fat32_readdir=129
```

The counts cover the time since the previous line, including other requests that
checked scopes. The line goes out before the final reply, so it cannot join the
next shell prompt.

## Measurements

One boot each, DIAG build, TCG, one processor, the same disk with
`LINUX_APPS_DIR=/Volumes/Data/linux_static`. Counts and times come from the third
`time ls /apps > /tmp/apps-list`, once caches have settled.

| Measure | Before | After |
| --- | --- | --- |
| `time ls /apps > /tmp/apps-list` | 1413 ms | 549 ms |
| Subtree walks (`scope_contains`) | 368 | 3 |
| Authority directory reads (`scope_backend_readdir`) | 1104 | 9 |
| Exact lookups (`scope_openname`) | 368 | 1 |
| FAT32 directory exchanges (`fat32_readdir`) | 1224 | 129 |
| `time cat /tmp/apps-list` | 115 ms | 137 ms |
| `bench vfs 100` | 783 us/op | 1028 us/op |

`bench vfs` stats `/` on the volatile root, which holds no scopes, and `cat` writes
to the serial console. Neither reads scopes on the persistent volume in its hot
path, so this change does not explain their difference. Single runs under TCG vary
by this much: the first report of this problem measured `bench vfs` at 610 us/op
on an unchanged tree.

## Consequences

The remaining 549 ms is about 4.5 ms per entry: one VFS request and one FAT32
exchange each, plus launching `ls`. Batched enumeration is the next thing to
measure and stays a separate change.

Deriving a mask assumes a backend's directories form a tree. RAMFS, the initramfs
archive, and a valid FAT32 volume satisfy that. A corrupt FAT32 volume with two
directory entries sharing a cluster breaks it: a listing through the unprotected
name could show names that a full walk would hide. Opening one of those files still
takes the full check and is refused. Such a volume can already be read outside the
VFS by whoever wrote it, so this ADR accepts the limit.

Rename and move checks keep their uncached ancestor walk, which runs only on the
requests that change names.
