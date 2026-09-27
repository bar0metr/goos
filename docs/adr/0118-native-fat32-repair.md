# ADR 0118: Native FAT32 repair mode

## Status

Accepted. Completes GoOS 0.1.4 Phase 5.

## Context

Check mode identifies whole-volume inconsistencies but deliberately has no
write authority. Repair must remain explicit, operate only on an unmounted
exclusive partition slice, and avoid choosing between ambiguous live owners.

## Decision

`fat32.Repair` applies bounded safe corrections, flushes the device, and runs
`fat32.Check` against the resulting state. It can restore a
corrupt primary boot sector from a valid named backup, restore the backup from
a valid primary, synchronize mirrored FAT copies from the active read copy,
remove invalid or orphan LFN slots, detach an empty file's cluster chain, trim
a file chain beyond logical EOF, reclaim unowned non-bad clusters after a
complete trustworthy namespace walk, and rewrite both FSInfo copies with an
exact free count and next-free hint.

Cross-links, short or invalid live chains, unreadable directories and other
ambiguous damage are reported but not guessed at. Such findings also suppress
lost-cluster reclamation because ownership could not be proven complete.

`fsck-fat --repair auto` is the only repair entry point. It uses the same PID 1
unmount and exclusive block-slice transition as check mode, but its device
adapter permits bounded block writes. Every applied correction is printed as
`repair CODE cluster=N entry=N value=N`; the final report contains the
post-repair error count and repair count. Exit status is non-zero when errors
remain or I/O failed. Running repair again on the repaired volume makes no
changes.

## Consequences

- Merely running check mode can never modify a volume.
- Repairs cannot race a mounted filesystem or address outside its selected
  partition.
- Clear metadata and allocation damage is repairable without importing a host
  utility.
- Cross-linked live files require a future explicit owner-selection policy.
- There is no transactional journal; each individual correction is ordered so
  interruption leaves a state check mode can diagnose and a later repair can
  resume.
