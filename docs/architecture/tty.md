# Userspace TTY

The first TTY service is a native ring-3 process. Clients do not receive a UART
handle. They send versioned TTY messages over their channel and may block using
`IPCWaitReceive`. Only the TTY process owns a `Write` capability for the serial
device. The kernel validates that process-local handle and copies the requested
bytes from userspace before touching COM1.

Protocol version 1 supports output, input delivery, echo control, and screen
clear operations. Messages use the stable IPC envelope and reserve the first
payload byte for the operation. Output remains bounded by the IPC inline payload
limit; larger output must be split by the client.

The line editor has a fixed 128-byte buffer, accepts printable bytes, handles
backspace/delete, completes on CR or LF, and rejects overflow and unsupported
control bytes. It performs no allocation.

The boot test runs two isolated native processes. The client submits a TTY write
request. The service waits through the scheduler, receives the wire message into
its writable stack, and writes only the operation data through its serial
capability. The serial markers prove both the IPC wake path and service-owned
device access.

The native shell currently uses the smaller console-session kernel ABI rather
than the bootstrap TTY channel. A device handle with `Read|Write` authority
permits single-byte nonblocking input and bounded output; processes without the
handle are rejected. Monotonic timer reads are exposed separately and carry no
device authority. `DebugWrite` remains restricted to PID 1 and the legacy ABI
probe, so the shell cannot bypass its manifest.

Consolidating this session ABI behind a persistent userspace TTY server is the
next terminal-architecture step.
