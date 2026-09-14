# GoOS

GoOS is an experimental x86_64 operating system whose primary implementation
language is Go compiled with TinyGo. It is designed around a small privileged
kernel, capability-based authorization, typed IPC, isolated processes, and
userspace services.

The repository currently implements Milestone 0: a deterministic TinyGo UEFI
kernel artifact, bootable ISO, COM1 boot log, QEMU workflow, automated boot
test, and the first host-tested capability primitive. Scheduler, memory
management, process isolation, IPC, VFS, and shell are not implemented yet.

## Build and run

The supported development host is macOS. Required tools are Go, TinyGo, GNU
Make, QEMU x86_64 with OVMF, `hdiutil`, `nm`, and `tar`.

```sh
make toolchain-check
make
make test
make test-boot
make run
```

The ISO is written to `build/goos-x86_64.iso`. A successful headless boot emits:

```text
GoOS 0.1-dev
TinyGo kernel

CPU: x86_64
Boot: UEFI
Security: capabilities (foundation)
Kernel initialized
BOOT_OK
```

`make run-debug` starts QEMU paused with its GDB server on TCP port 1234.

## Architecture

```text
applications (future)
        |
userspace services (future)
        |
typed IPC + capabilities (foundation)
        |
minimal TinyGo kernel
        |
UEFI / x86_64 / QEMU
```

See [architecture](docs/architecture/overview.md), [building](docs/development/building.md),
and the [roadmap](docs/development/roadmap.md).
