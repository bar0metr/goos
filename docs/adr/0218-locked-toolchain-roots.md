# ADR 0218: Locked toolchain roots

## Status

Accepted. Covers the generated roots of ADR 0204 (goroutines) and the
crypto, TLS and SSH compatibility roots.

## Context

GNU make orders a stamp before its consumers inside one make process.
Two independent processes on one `BUILD_DIR` share
`build/toolchains/{goroutines,crypto,tls,ssh}` and their TinyGo
`goroot-*` caches with no coordination. The goroutine recipe deleted its
root before regenerating it; `prepare-compat.sh` moved the old root aside
and the stage in with two renames; either could run while another
process's TinyGo read the root. `make clean` removed it all at once.

## Decision

`tools/rootlock` takes `flock(2)` locks, as `tools/mkfat32` does.

- **Lock files.** A root under `<build>/toolchains` locks
  `<build>/.locks/<kind>-<hash>.lock`, the hash taken over the root's
  canonical absolute path. Another root locks `.locks` beside itself.
  Every process derives one path from one root, and separate build
  directories never share a lock.
- **Writers** (`prepare-compat.sh`, `prepare-goroutines.sh`) hold the
  exclusive lock for the whole update: removing stale stages, generating
  into a private stage, removing their own cache namespace's `goroot-*`,
  and publishing. `rootlock publish` swaps the stage and the root with
  one `renamex_np`/`renameat2` exchange, so readers see the old complete
  root or the new one; the stamp lives inside the root. A writer that
  waited skips its work when the root was published meanwhile.
- **Readers** hold the shared lock until TinyGo or `go test` exits:
  `GOROUTINE_BUILD`, `build-tls.sh`, `build-ssh.sh` (builds and host tests)
  and `crypto-check.sh` with `CRYPTO_COMPAT=1`. The scripts run their
  `make` step first, then re-execute under the lock, so a reader never
  waits for a writer while holding a lock. Goroutine builds get their own
  TinyGo cache namespace.
- **clean** locks all four roots exclusively in one sorted order, renames
  the build directory aside and removes it, lock files included.

The OS drops a lock when its holder dies. A process that waited on a lock
file that `clean` removed wakes on an inode no longer at the lock path; it
compares the two after every `flock` and locks again. No process ever
keeps a lock on a removed file.

## Consequences

`make test-build-concurrency` copies the tree and runs separate make and
`rootlock` processes: two forced TLS writers, `remoted` and
`remoted-worker` from two makes, goroutine and crypto builds beside a
forced rebuild, readers that a writer must wait for, a killed writer, two
build directories, and `clean` against a reader. It checks stamps, stages,
symlinks and caches, and that the host TinyGo cache did not change.
`CONCURRENCY_ITERATIONS` repeats the light scenarios.

`clean` waits only for its own build directory's roots: a `BUILD_DIR`
nested inside another goes with its parent. Two roots of one kind that
share a cache directory share their `goroot-*` invalidation. Two processes
writing the same final ELF stay unsynchronized.
