# ADR 0064: Asynchronous exec and background jobs

## Status

Accepted. Changes the exec protocol of ADR 0043 and the PID 1 loop of
ADR 0029.

## Context

PID 1 ran each shell request to completion before it read the next event.
While a command ran, PID 1 blocked on the command's own process handle. For
that time it did not see lifecycle events, so a service that crashed during a
long command was restarted only after the command finished, and PID 1 could not
serve another exec or control request. A single shell never noticed, because it
also blocked until the reply. The shell gave up after 30 seconds and printed
`exec: no reply` even for a command that was still running normally.

A request, holding every stage of a pipeline, had to fit in one 256-byte IPC
payload. A command line of at most 128 characters can exceed this: take a
four-stage pipeline with a working directory 50 bytes deep and one long
argument.

## Decision

**Shared exec page.** The kernel creates a one-page shared-memory object for
exec requests and gives it to PID 1 (ADR 0063). PID 1 maps it and grants the
shell a `Read|Write` handle named `exec-page`. Both map the page at
`exec.PageAddress`. The page has four slots of 1024 bytes.

To run a command, the shell encodes the request into a free slot and sends an
exec version 6 submit message that carries only the slot and the size. The
request format is the version 5 payload with one flags byte added to its
header. The only flag marks a background request.

**Jobs in PID 1.** PID 1 copies the slot into a job record before it validates
the request, so the shell cannot change a request that is running. It then
starts the pipeline (`supervisor.StartPipeline`) and returns to its loop.
Launching remains synchronous. A job holds each stage's process handle and
reaps them in stage order. The loop's `WaitAny` covers the lifecycle channel,
the exec channel, and the next process of every job, at most 6 of the 16
objects. When a job's last process has been reaped, PID 1 sends the reply
carrying the same exit code rule as before: the first non-zero code in stage
order. PID 1 rejects a request whose slot is still in use, or that arrives
while all four job records are busy, with `LaunchResourceExhausted`.

**Background jobs.** A line ending in an unquoted `&` runs in the background.
The shell prints `[N] started`, where N is the slot number plus one, and
returns to the prompt. The first stage's standard input is a stream that is
already at EOF, so a background job never competes with the shell for the
console. Replies to background jobs are picked up in two places: while the
shell waits for a foreground reply, and while it polls the console for input.
The shell then prints `[N] done`, `[N] exit status: C`, or
`[N] launch failed: S` and redraws the prompt with the line typed so far. One
slot is always kept free for the foreground, so at most three background jobs
run at once. Builtins and `;` sequences cannot run in the background.

A foreground command waits for its reply without a time limit. If PID 1 dies,
the shell sees the exec channel close.

## Alternatives

Splitting a large request into several IPC messages would avoid the page, but
PID 1 would have to reassemble them per client, and the channel's 16-message
queue would limit how many requests can be in flight.

Waiting for the lifecycle `ProcessExited` event instead of the process handles
would need no per-job wait entries. The process handle, however, is the
capability that proves PID 1 started the process, and reaping through it keeps
the exit code with the job.

A job table indexed by PID with arbitrary waits would lift the limit of four
jobs. It is not needed while there is a single shell.

## Consequences

PID 1 keeps supervising services and answers `services` while commands run.
Several pipelines can run at once, and each request has 1024 bytes instead of
256. The boot test runs two background jobs next to a foreground command and
`services`, and a four-stage pipeline whose request exceeds 256 bytes.

Background jobs cannot be stopped from the shell. There are no signals yet, so
jobs have no job control beyond the completion notice.
