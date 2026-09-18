# ADR 0042: Bounded sequential shell execution

## Status

Accepted.

## Context

Milestone 8 and roadmap Priority 3 require sequential execution. ADR 0041
already waits for a single command or two-process pipeline to complete.
The shell uses a non-freeing arena, so a sequence must not retain allocations
from completed commands.

## Decision

Parse up to eight semicolon-separated commands within the existing line limit.
Validate every stage before executing the first command. Preserve the existing
quote and backslash rules. Accept a trailing separator, reject empty intermediate
commands, and reject shell builtins inside pipelines. Reuse the existing exec
request, PID 1 completion reply, and directional stream capability model.

Execute commands in order, including after reported runtime failures. Preserve
shell-local cwd changes and reclaim the shell arena after each completed command.
The parsed sequence borrows the static line-editor buffer across these resets.
Builtin dispatch uses parsed arguments so quoted cwd paths behave consistently.
No startup ABI, kernel-call, or exec wire protocol changes are required.

## Consequences

Host tests cover syntax bounds, quoting, escaping, malformed tails, and pipeline
composition. The QEMU gate exercises sequential external commands, a pipeline,
cd inheritance, continuation after failure, and rejection before side effects.
File redirection, longer pipelines, conditional execution, and job control remain
outside this iteration. Priority 3 is not yet complete.
