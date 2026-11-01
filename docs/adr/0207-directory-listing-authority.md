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

### A scope remembers its nodes

The first check after an invalidation walks a subtree scope once, with the same
bounds as before, and records each node it reaches. Later checks compare against
that record. An exact scope likewise remembers the node its name resolved to, or
that the name was missing, and each check compares against it. Each of the eight
scope slots holds up to `maxScopeWalk` node IDs, about 16 KiB in all.

The three outcomes of the old walk keep their meaning:

- A complete walk answers for every node.
- A walk stopped by `maxScopeWalk` or `maxScopeDepth` recognizes the nodes it
  reached and denies any other. The bound stays hit until something changes, so
  this result is remembered.
- A backend failure denies the check, and the next check walks again. A failed
  exact lookup behaves the same way.

The record says which scopes hold a node. `permits` still applies the session's
grants on each request, so a changed authority takes effect at once.

### Identity alone decides membership

An entry's membership comes from its own node ID, never from the directory that
listed it. Deriving it from the parent would be cheaper to state and wrong: FAT32
mount does not look for cross-links (ADR 0117), and `fsck-fat` runs separately on
an unmounted volume. With `/apps/foo` sharing a cluster with the protected
`/state/security/ssh`, `/apps/foo` itself lies outside the scope, but its children
have the node IDs of the protected children, because a FAT32 node ID is a directory
cluster and an entry index. An early version of this change derived masks from the
parent, and `TestCrossLinkedDirectoryListingHidesScopedChildren` shows the names it
leaked through such a directory.

Comparison by node ID also lets the backend's own rules decide case folding and
short-name aliases.

### Invalidation

`Namespace.invalidate` clears the records with the resolved paths. It runs on
mount, unmount, protection changes, scope installation, rename, and replace. Create
and unlink now invalidate on failure as well, because a backend can leave the name
behind when its durability step fails. The service invalidates when a filesystem
service attaches or detaches and when it creates a volume directory directly. These
paths are complete because the VFS is the filesystem service's only client
(ADR 0096).

Rename and move checks keep an unrecorded ancestor walk from the moved node, which
runs only on the requests that change names.

### Diagnostics

A `DIAG=1` build prints one line when a directory scan ends:

```text
VFS_LISTING requests=120 scope_checks=372 scope_contains=1 scope_openname=1 scope_backend_readdir=3 fat32_readdir=123
```

`scope_contains` counts subtree walks. The counts cover the time since the previous
line, including other requests that checked scopes. The line goes out before the
final reply, so it cannot join the next shell prompt.

## Measurements

One boot each, DIAG build, TCG, one processor, a disk built with
`LINUX_APPS_DIR=/Volumes/Data/linux_static`. The Before column is the third
`time ls /apps > /tmp/apps-list` of its boot. The After column gives the second and
third, because the redirect's create on `/tmp` invalidates the records and each run
pays for one or two walks.

| Measure | Before | After |
| --- | --- | --- |
| `time ls /apps > /tmp/apps-list` | 1413 ms | 384, 424 ms |
| Subtree walks (`scope_contains`) | 368 | 1, 2 |
| Authority directory reads (`scope_backend_readdir`) | 1104 | 3, 6 |
| Exact lookups (`scope_openname`) | 368 | 1, 2 |
| FAT32 directory exchanges (`fat32_readdir`) | 1224 | 123, 126 |
| `time cat /tmp/apps-list` | 115 ms | 109 ms |
| `bench vfs 100` | 783 us/op | 899 us/op |

`bench vfs` stats `/` on the volatile root, which holds no scopes, so this change
does not reach its hot path. Single runs under TCG vary by this much: the first
report of this problem measured it at 610 us/op on an unchanged tree.

## Consequences

The remaining 400 ms is about 3.4 ms per entry: one VFS request and one FAT32
exchange each, plus launching `ls`. Batched enumeration is the next thing to measure
and stays a separate change.

A mutation on any backend, `/tmp` included, clears the records of every backend, so
a workload that creates files while listing a large directory walks once per
mutation. Each of those walks is still bounded, and the old code walked for each
check regardless.
