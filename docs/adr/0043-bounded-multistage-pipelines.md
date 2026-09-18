# ADR 0043: Bounded multistage pipelines

## Status

Accepted. Extends ADR 0041; the two-stage limit and exec version 3 are superseded.

## Context

Priority 3 already has native byte streams and sequential shell execution.
The two-stage representation duplicates launch logic and cannot express a
middle process with both stream input and stream output. PID 1 must preserve
endpoint ownership when any launch fails, without increasing kernel authority
or the 256-byte IPC payload limit.

## Decision

Support one to four invocation records in version 4 of the shell exec protocol.
Each record carries a bounded system executable path and structured arguments;
all records share cwd and environment. Reject malformed vectors, unexpected
attachments, invalid paths, excess stages, trailing bytes, and oversized payloads
before launching processes. Version 3 requests are explicitly rejected.
The GST2 startup ABI and kernel-call ABI are unchanged.

Create all N-1 streams, then start children from the final consumer back to the
first producer. Each middle process receives only its upstream reader and
its downstream writer. Existing manifest rules attenuate endpoint rights and
consume the source handles on successful launch. Stderr remains a separate
write-only console capability.

Track retained endpoints and process handles in fixed arrays. On creation or
launch failure, close all retained endpoints before waiting for started children.
On success start all children before waiting. Reap every started child and retain
the first nonzero exit code in left-to-right pipeline order. Keep this policy
in the host-testable init package, with a narrow resource-operation interface
implemented by native PID 1.

## Alternatives

Unbounded pipelines would conflict with the fixed IPC, process, and handle
budgets. Increasing IPC payload size is unnecessary for the current shell.
Producer-first launch can leave a console reader running after a downstream
launch fails; downstream-first launch avoids starting that reader in this case.

## Consequences

Host tests inject failure at every stream-creation and child-launch position,
verify directional endpoint ownership, check cleanup before waits, and verify
exit-code selection. Decoder tests cover truncation and malformed size fields.
Repeated QEMU launches exposed stale lifecycle attachment/reserved fields in
reused freestanding message storage. The launch encoder now initializes those
fields explicitly, matching the exec protocol encoders.
QEMU tests exercise four-stage data flow, repeated resource reuse, failures at
first/middle/last stages, subsequent commands, and reboot persistence.

Closing endpoints is not forced cancellation. A program that ignores stream
closure or intentionally never exits can still prevent pipeline completion.
Pipelines longer than four stages and requests exceeding 256 bytes are rejected.
File redirection remains the next Priority 3 increment.
