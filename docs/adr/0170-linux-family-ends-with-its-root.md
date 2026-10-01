# ADR 0170: Ctrl-C on a Linux root ends its whole family

## Status

Accepted.

## Context

The native shell starts one Linux process, the family's root, and waits for
that process alone. Ctrl-C kills it through the native kill path. Its forked
descendants are ordinary GoOS Processes the shell knows nothing about. Without
a rule they would run on after the job ended, holding streams the terminal
reads and slots of the family budget. GoOS has no controlling terminal or
process groups to deliver a terminal `SIGINT`.

## Decision

When the native kill path ends a Linux launch root, the kernel also ends every
other running member of its family. Each member's waits and continuation are
cancelled and its task retires with `SIGKILL` as its termination status. The
root's own status stays the native kill code the shell reports. The members
then exit by the ordinary path: zombie records, `SIGCHLD`, and parent wakes.
A forked process killed some other way, such as by `kill` from its parent,
ends alone.

## Consequences

Ctrl-C on a Linux job ends the job, its pipeline stages, and its background
children together. It is not a terminal-generated `SIGINT`: no handler runs,
and a program cannot catch it. That arrives with terminal job control.
