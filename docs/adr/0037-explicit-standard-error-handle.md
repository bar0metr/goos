# ADR 0037: Explicit standard error handle

## Status

Accepted.

## Context

External native utilities already receive a named write-only `stdout` device
capability, but diagnostic output used the same handle. Redirection and pipelines
require output roles to be explicit before their destinations can differ.

## Decision

PID 1 installs separate named `stdout` and `stderr` capabilities in every
external utility process. Both are currently derived as write-only views of the
serial console device. The startup block binds both names independently, and
the utility runtime routes successful output to `stdout` and non-zero-exit
diagnostics to `stderr`.

The handles deliberately refer to the same device in this iteration. Their
separate identities and rights are the stable boundary needed for later
redirection without introducing POSIX file descriptors. A representative long
boot command is tested to remain within the bounded GST2 startup envelope.

## Consequences

Utilities no longer assume that normal and diagnostic output share one startup
role. Existing serial behavior is unchanged because both capabilities target
the console. Explicit `stdin`, alternate stream endpoints, redirection, and
pipes remain future Priority 3 work.
