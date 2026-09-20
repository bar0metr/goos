# Testing

GoOS keeps fast host validation separate from the complete QEMU gate.

## Targets

- `make test-unit` runs all host-side Go tests.
- `make test-integration` builds and validates the UEFI kernel artifact.
- `make test-security` runs the capability, handle, IPC, VM, filesystem, and
  userspace security-focused host tests explicitly.
- `make test` runs the three fast targets above and does not start QEMU.
- `make test-boot` builds the ISO and runs the two-phase QEMU boot and reboot
  persistence test.
- `make test-all` is the canonical complete Definition-of-Done gate. It runs
  the fast tests, lint, documentation checks, the ISO build, and the real QEMU
  boot/persistence test in sequence so host builds cannot contend with QEMU.
- `make test-performance` writes microbenchmark reports under `build/reports/`.

Use `make test` during the normal edit loop. Use `make test-all` before delivery
of changes that affect the kernel, native userspace, startup protocols, storage,
shell composition, or boot behavior.

## QEMU boot test

`tests/boot/qemu_boot.sh` captures the complete serial streams in:

- `build/logs/boot.log` for the main validation boot;
- `build/logs/reboot.log` for the persistence boot.

The target creates a fresh `build/boot-test-virtio-blk.img` for each run. Tests
therefore cannot corrupt or overwrite the persistent `build/virtio-blk.img`
used by `make run`; both QEMU phases share the fresh test image to validate
reboot persistence.

The main phase has a 120-second bound. It validates kernel initialization,
memory isolation, scheduler/process behavior, IPC and capabilities, userspace
services, storage, filesystems, shell commands, pipelines, failure recovery,
resource-reuse stress, redirection, and the on-disk persistent bytes. The
reboot phase has a 60-second bound and reads the persisted file in a fresh VM
without recreating it.

Successful runs print one `PASS` line per architectural group and a final check
count. Repeated guest output remains only in the full serial log. Failed checks
print a stable name, expected and observed values, the log path, and a bounded
context excerpt. Timeouts identify the phase and completion marker. The harness
never prints an entire serial log automatically.

Inspect the raw artifacts directly when more context is required:

```sh
less build/logs/boot.log
less build/logs/reboot.log
```

The diagnostic reporting path has a deterministic self-test that intentionally
returns failure without starting QEMU:

```sh
tests/boot/qemu_boot.sh --self-test-failure
```

## Adding a boot assertion

Keep the guest command in the existing serial input sequence. Add the assertion
to the matching architectural group using `require_marker`, `require_regex`,
`forbid_regex`, `require_count`, or `require_min_count`. Give every assertion a
stable dotted name and, when useful, an anchor matching the initiating command.
Do not add another monolithic condition or remove stress iterations to reduce
host output.
