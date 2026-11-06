# ADR 0215: Classified resource bounds

## Status

Accepted. Applies to every bound in `kernel/` and `userspace/`. ADR 0111
made kernel tables pay for what they hold; this ADR asks each bound why it
has its value.

## Context

The tree has 267 package-level `Max*` constants and several dozen fixed
tables with other names. Their comments justify them in mixed ways. Some
cite a wire field or the hardware. Others say a number is "bounded", "an
intentional system limit", or "room to spare", and the number came from
the first program that needed the table. ADR 0213 found the cost: a TCP
table sized for a prototype capped `linuxsockd` at 96 connections, four
releases after anyone chose 96.

Nobody could answer four questions for a given bound without reading its
code: what it is for, who pays for it, what a caller sees when it runs out,
and where an operator sees how close the system is.

## Decision

**Every release-visible bound has one class.**

- *protocol*: a wire format, ABI or standard fixes it;
- *hardware*: the processor, firmware or a device fixes it;
- *policy*: a security or quota decision about one principal's share;
- *profile*: a memory size picked for a target machine, set by a
  configuration key or a named build profile;
- *artifact*: none of the above.

**Every release-visible bound records six facts**: its default, its
maximum, the account that pays for it, its memory cost, the failure a
caller gets when it runs out, and the status field that shows use and
limit. A missing status field is written "none"; it is a fact, and a
follow-up.

**A comment that calls a bound "bounded" or "intentional" does not justify
it.** The justification is the class and the facts. A bound with neither
is an artifact.

**Artifacts are defects.** [The resource policy](../architecture/resource-policy.md)
lists them. A change removes one by fixing the code or by giving the bound
a class with its evidence.

**The inventory lives in one place and a test enforces it.**
`docs/architecture/resource-policy.md` has a table row for each bound.
`tools/resourcepolicy` parses the non-test Go files under `kernel/` and
`userspace/`, collects every `Max*` constant and variable, and fails when
one has no row or a row names a bound that is gone. A row may name
`pkg.Max*` for a package whose bounds share one class, which keeps
field-width packages such as `dnswire` to one line. `make test-unit` runs
the check.

**The production reference profile is a 1 GiB VM.** The policy document
states what that machine must provide without a rebuild and which code
constant or configuration key provides each item. A bound that keeps the
reference machine short of the profile is a gap and the document names it.

## Alternatives

- **Comments only.** The tree already had them. They drifted from the code
  and nothing checked that a new bound had one.
- **A naming convention instead of a table.** A `Proto`/`Policy` prefix
  would put the class in the name, but not the owner, cost, failure or
  status, and renaming 267 constants touches every caller.
- **Check every fixed array.** Array lengths come from expressions, local
  constants and types; a parser that finds them all would flag thousands of
  buffers. The `Max*` names catch the bounds people chose on purpose, and
  the document lists the important tables with other names by hand.

## Consequences

- A new `Max*` constant fails `make test-unit` until someone classifies it.
- The first audit found 24 artifacts and 13 missing status fields, listed
  in the policy document. Among them: 64 processes beside 256 task slots,
  16 thread slots per process, 46 TCP sessions in `netd`, one 4096-entry
  open-description pool for all Linux processes, and TCP memory budgets
  absent from `net tcp`.
- Tables with names other than `Max*` (`klog.Capacity`, `kcall.ThreadSlots`,
  launch quotas) depend on reviewers to keep their rows current.
