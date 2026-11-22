# ADR 0230: generation-safe VFS component cache and narrow invalidation

## Status

Accepted. Replaces the resolved-path cache of ADR 0228 in `kernel/vfs` and
changes its invalidation rule.

## Context

ADR 0228 made repeated name lookups cheap by caching resolved full paths and
their parent directories, with a clock that replaces entries in turn. It kept
correctness by emptying the whole cache on every namespace mutation. That
global flush is the dominant cost of the 0.3.0 `fsbench deep` workload: the
100-level deep path is built and torn down one `mkdir`/`rmdir` at a time, and
each step re-walks the whole prefix from the mount root because the previous
step dropped the cache. Each component on an ext4 mount costs one `OpenName`
plus one `Stat` exchange with the filesystem service, so the workload is
quadratic in depth (about 0.83 s under KVM, against Linux's 4.0 ms).

The release requires this cost removed before account, metadata and xattr work
widens the request surface (§17.1). The cache must not, for the speed, reuse a
result across an incompatible VFS authority, a root confinement, an old mount
or filesystem-service generation, a renamed or replaced parent, a stale
symlink target, or a different session root.

## Decision

**Component cache.** The cache stores one resolved component, keyed by the
filesystem backend instance, the parent node identity and the component bytes,
mapping them to the child node identity and whether the child is a symbolic
link. Keying by node identity rather than a path string is what makes narrow
invalidation sound: two spellings a case-insensitive filesystem folds together,
and two mount aliases of one backend, name the same node, so a change through
one alias drops the shared entry. A rename of a directory keeps every moved
node's identity, so nothing below the moved name is dropped and the cache stays
valid across it.

**Bounds.** One namespace holds `MaxCache` (128) entries. A component name
longer than 64 bytes is resolved every time rather than cached; such names are
rare and caching them would cost a `NameMax` buffer per entry. Replacement
keeps the clock of ADR 0228, sparing an entry a lookup used since the clock
last passed it.

**Generation safety.** The namespace keeps a `cacheGeneration` epoch. A full
flush steps it; every entry carries the epoch it was written under and a
lookup ignores an entry from an older epoch. The full flush runs on a mount or
unmount, a protection change, a scope install, a filesystem-service replacement
and a restart (the existing `Invalidate`), so no entry survives a mount or
service generation. A mount also steps its own generation, which the mount ID
carries, so a stale mount identity can never name a later occupant of the same
slot.

**Narrow invalidation.** Create, mkdir, unlink, rename, replace, symlink and
hard link drop only the touched (parent, name) entries, plus the scope
decisions those name changes can invalidate (what an exact name resolved to,
and the nodes a subtree walk reached). They no longer drop the whole component
cache. Rename and replace drop the source and target names; everything else
stays cached.

**Authority.** The cache resolves a path to a node, never to an authorization
decision. Rooted confinement (`insideRoot`) and scope checks (`nodeScopes`,
`permits`) still run around every resolution, so a warm cache cannot widen a
session's authority, and no result crosses session or root boundaries.

**Symlinks.** Only that a child is a symbolic link is cached; the link's target
is read fresh on every follow, so a stale target is never reused.

**Counters.** `AuthorityCounters` gains `PathResolves`, `ComponentHits` and
`ComponentMisses`, reported by a DIAG build with the existing counters, so a
measurement can show how many backend exchanges the cache avoided.

## Evidence

- `TestNarrowInvalidationKeepsUnrelatedPrefixes`, `TestRenameAndReplaceInvalidateNarrowly`,
  `TestFullInvalidateForgetsAllComponents`, `TestMountGenerationForgetsComponents`,
  `TestCacheDoesNotWidenRootedAuthority` and `TestResolutionCountersProveFewerBackendExchanges`
  exercise narrow invalidation, full-flush fencing, mount generation, rooted
  authority and the counters. `TestMutationsInvalidateResolutionCache`,
  `TestResolveRemembersTheParentDirectory` and `TestResolveDoesNotAllocate` still
  pass unchanged: a sibling still costs one lookup and resolution allocates
  nothing.

## Consequences

The deep-path workload resolves each new level with one component exchange
instead of a full re-walk, turning the `mkdir` and `rmdir` phases from
quadratic to linear in depth. A directory with many names still costs one
lookup per name. The cache is a fixed table of 128 entries of a few dozen bytes
each, about ten kilobytes per namespace.
