# ADR 0130: Application launch and nested foreground ownership

## Status

Accepted.

## Context

`gfm` has to open `gview` and `edit` on the selected file and come back when
they exit. Until now only the shell could start a program: it writes a job to
its exec page and PID 1 builds the pipeline, grants each stage what its
manifest in the system image declares, and reports the job's end (ADR 0064,
ADR 0080). The shell also owns raw terminal mode: it switches the session to
raw for a single-stage foreground job whose manifest declares
`GrantTerminal`, and back to line mode when the job's reply arrives
(ADR 0124).

Three shortcuts were available and each breaks something:

- Linking the viewer and editor into `gfm` makes one program of three and
  leaves every later application with the same problem.
- Handing applications the shell's exec channel lets any of them start
  pipelines, background jobs and programs from files, with every grant PID 1
  would give the shell's commands.
- A generic spawn call with caller-chosen handles lets a caller pass on
  whatever it holds and makes PID 1's policy advisory.

Two programs also must not read one terminal at the same time, and the shell
must not leave raw mode while `gfm` still owns the screen.

## Decision

### The caller stays the foreground job

`gfm` remains the shell's foreground job for the whole session. The shell
keeps the terminal raw and keeps writing keys to the job's input stream. The
child reads that same stream and writes the same terminal, while `gfm` blocks
waiting for the child's result and reads nothing. When the child exits, `gfm`
lists both panels again and repaints the whole screen. The shell sees none of
this and needs no change.

### A launch channel, granted by manifest

A new manifest grant, `commands.GrantLaunch`, asks PID 1 for a launch
channel. PID 1 grants it only to the single stage of a foreground job whose
command comes from `/system/bin` in the system image. When it does, PID 1
grants the stage's standard input as a counted copy and keeps the reader
itself, together with the server end of the channel, in the job's record.

Over the channel the caller sends one request at a time. It writes the
request into a memory object of its own with the exec protocol's `Builder`
(working directory, `NAME=VALUE` environment, one stage) and attaches the
object to a `GLNCH001` submit message. PID 1 maps the object read-only, copies
at most one exec slot out of it, unmaps it and closes the handle before it
decodes anything.

### What PID 1 accepts

`supervisor.ChildLaunch` decides, and host tests pin it:

- the request is one foreground stage without diverted stderr;
- the program is a command of `/system/bin`: a path outside it, a service
  under `/system` or a file on a writable volume is refused;
- the index describes the command, otherwise `not found`;
- its manifest declares `GrantTerminal`, because the child inherits a raw
  terminal. A line-oriented command such as `ls` is refused.

The child gets its manifest's grants through the ordinary launch path, with a
new VFS session of its own. It never gets `GrantLaunch`, so nesting stops at
one level. Its standard input is PID 1's copy of the caller's input and its
output and error go to the session's terminal. The caller attaches no handle
the child receives and cannot name a grant.

### Lifetime

PID 1 waits on the child's process handle in its main loop and answers the
request with the exit code, or with the launch status if the child never
started. Only one child runs per caller: the channel is not read again until
that child has been reaped.

When the caller's job ends, whether by exit, crash or Ctrl-C, PID 1 stops a
child that is still running, reaps it, and closes the channel and the kept
input before it answers the shell. The shell then leaves raw mode as before.
Ctrl-C reaches the job through the shell as before, and the kill stops the
child together with its caller.

## Consequences

- `gfm` starts `gview` and `edit` with F3 and F4, on COM1 and on the
  framebuffer, and a failed or crashed program is a status-line message.
- Only one program reads the keys at a time, and raw mode keeps the scope
  ADR 0124 gave it: one foreground job.
- An application can start full-screen commands from the system image only,
  one at a time, one level deep, with the authority their manifests declare.
- A launched child gets a fresh VFS session and none of the caller's.
- Keys typed after F3 but read by `gfm` in the same chunk stay with `gfm`.
- PID 1 waits on at most one more object per job, within the 64-object wait
  bound.
- Pipelines, background launch and programs from files stay the shell's.
