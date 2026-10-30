# Static Linux application compatibility

This catalog covers static x86_64 Linux ELF programs on GoOS. Each result
applies to the operations in its row, not to every feature of the program.
The external BusyBox and 7-Zip binaries are supplied by the tester. The
repository does not pin their versions or SHA-256 hashes, so the release
results below do not certify a different build of either program.

The [Linux ABI matrix](../architecture/linux-x86_64-abi.md) lists individual
syscalls and resource bounds. GoOS accepts static `ET_EXEC` and supported
static PIE binaries, including musl and glibc builds. An ELF with `PT_INTERP`
requires a dynamic linker and fails at launch.

## Application results

`Scoped pass` means a named workload passed a release gate. It says nothing
about commands or options outside that workload. `Blocked` means a required
GoOS interface is missing. `Untested` means the repository has no run result.

| Program | Status and evidence | Operations covered | Limits and usable paths |
| --- | --- | --- | --- |
| Static BusyBox, version and hash not recorded | Scoped pass in [0.2.0](../releases/0.2.0.md) and [0.2.1](../releases/0.2.1.md); [QEMU test script](../../tests/boot/linux_busybox.sh) requires each operation | `echo`, `pwd`, `cat`, `ls`, `mkdir`, `rm`, `touch`, `stat`, `chmod`; file reads and writes | The gate reports other applets as observations only. `sendfile` is absent; BusyBox `cat` and `cp` can fall back to `read` and `write`. Run the binary by full path if it is in `/apps`. |
| BusyBox `sh` from that same static binary | Scoped pass in the [QEMU gate](../../tests/boot/linux_busybox.sh) | Non-interactive commands, child programs, exit status, two- and three-stage pipelines, redirection, a script, background job and `wait` | Start scripts explicitly with `busybox sh /path/script`; Linux `execve` does not interpret a `#!` line. |
| Interactive BusyBox `sh`, BusyBox v1.38.0 (2026-09-01), SHA-256 `9209cc9efa5840f06e0e61f78c1ca4b9a374eb6508ac0bd831cf53181a86f2db` | Scoped pass in the [terminal gate](../../tests/boot/linux_tty.sh), `make test-linux-nano` | Line editing, a command, a foreground child in its own process group, Ctrl-C ending it and the rest of the list with status 130, the shell taking the terminal back, `cd`, launching nano, nano's suspend, the `Stopped` report, `fg`, `exit` | Applets run as `busybox name`: `/system/linux` holds no applet links. `tty` prints nothing, since it reads `/proc`. Run alone in the foreground; in a pipeline or with a redirection the shell has no terminal. |
| GNU nano 9.2, static musl with ncursesw, SHA-256 `fee997775e4c63ebc6ab687ddf1d51175f7d5523b1c1f7075901db99dc318d2d`, built `--disable-libmagic --disable-nls --enable-utf8` | Scoped pass in the [terminal gate](../../tests/boot/linux_tty.sh) under QEMU and in `make test-pve-nano` under KVM | `nano -I FILE` from the native shell and from BusyBox `sh`: draw on the serial console, type, save with ^O, exit with ^X, `fsync` before the save is reported, reopen and show the saved text; ^T^Z suspends and `fg` redraws | TERM is `goos` on the local console, from `/system/linux/terminfo`. Nano's redraw after a serial resize is gated; a remote SSH session is not. Launched from the native shell, nano treats ^T^Z as a suspend and an immediate continue: nothing could continue a stopped root. |
| Static glibc 7-Zip `7zzs`; 26.03 appears in the [Proxmox findings](proxmox-lab.md#findings), current supplied binary unpinned | Scoped pass reported in [0.2.4](../releases/0.2.4.md); the [QEMU gate](../../tests/boot/linux_7zip.sh) verifies file hashes and concurrent execution | Show formats, create an archive, test it, extract it and compare SHA-256 hashes of extracted files | This proves the tested archive workflow, not every codec or option. The [Proxmox scenario](proxmox-lab.md#7-zip-workflow) also checks the round trip, but treats a stalled workload as an expected diagnostic result unless `PVE_FLAGS='--require-pass'` is set. |

The table records repository tests and prior release results. This document
does not claim a fresh BusyBox or 7-Zip run against the current source tree.
For a new binary, record its version and SHA-256 hash alongside the test log
before adding a passing result here.

## Known blockers

| Program or feature | What happens | Workaround |
| --- | --- | --- |
| Static network clients such as `curl` or BusyBox `wget` | Linux socket syscalls return `ENOSYS`. An offline option may run, but HTTP or DNS through Linux sockets cannot. No specific client binary is certified here. | Use native `fetch`, `dnsq`, `udp` or `tcp` commands for those operations. There is no socket compatibility layer for the same Linux binary. |
| Programs requiring `/proc`, `/sys`, symbolic links, file-backed `mmap`, `epoll`, or PTY pairs | Those interfaces are absent or limited in the [ABI matrix](../architecture/linux-x86_64-abi.md). A successful ELF load does not imply the program can complete its workload. | Use a mode that avoids the missing interface if the program provides one. Otherwise the application needs ABI work; no general workaround exists. |

## Reproduce and report a result

```sh
make test-linux-busybox BUSYBOX=/absolute/path/to/static/busybox
make test-linux-tty
make test-linux-nano BUSYBOX=/absolute/path/to/static/busybox NANO=/absolute/path/to/static/nano
make test-linux-7zip LINUX_EXTRA=/absolute/path/to/static/7zzs SMP=4
make test-pve-7zip LINUX_EXTRA=/absolute/path/to/static/7zzs PVE_FLAGS='--require-pass'
```

The BusyBox gate places the file in `/system/linux` and writes
`build/logs/linux-busybox.log`. The 7-Zip QEMU gate writes
`build/logs/linux-7zip.log` and `build/reports/linux-7zip.md`; the Proxmox
scenario stores its result under `build/pve/runs/<run-id>/`. The repository
does not carry either external binary, so these targets are separate from
the default test suite.

For another static program, place its directory under `/apps` with
[`LINUX_APPS_DIR`](building.md#testing-linux-applications-from-a-host-directory),
run it by full path, and use `DIAG=1` plus `diag` to collect refused Linux
syscalls, bad pointers and VFS failures. A report needs the binary's version,
SHA-256 hash, libc and static ELF type, exact command, GoOS revision, VM mode,
output, and the `DIAG linux` line. Mark operations that were not run as
untested.
