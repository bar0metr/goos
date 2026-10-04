# Proxmox hardware lab

`tools/pvelab` boots GoOS on a remote Proxmox VE node under KVM, with the
host's CPU instead of QEMU's TCG emulation. Each run makes a disposable VM
from the same ISO the local tests use and a freshly built installed disk,
drives the serial console through a scenario, writes a report, and removes
everything it made.

The lab is optional. Building and testing GoOS never needs it, and no
Proxmox target is part of `make test-all` or `make release-check`: the node is
private infrastructure that may be offline or busy, and a release must not
depend on it. `make test-hardware` runs the smoke, lscpu, and Linux scenarios
and is recommended before a release, not required.

## What a run does

```text
make pve-artifacts           fresh build/pve/input/goos.iso and goos-disk.img
pvelab run
  create VM                  q35, OVMF, EFI disk, no NIC, serial0 socket,
                             in PVE_POOL, tagged goos-pvelab
  upload ISO                 REST API, unique name, SHA-256 checked by Proxmox
  stage disk                 gzip stream over SSH to /var/tmp/goos-pvelab/<run>
  import disk                qm disk import, then attached as virtio0
  attach console             SSH -> socat -> /var/run/qemu-server/<vmid>.serial0
  start VM, run scenario
  cleanup                    stop, destroy with disks, delete ISO, remove staging
```

The REST API creates, configures, starts, and destroys the VM. SSH does the
two things the API cannot do well: importing a raw disk image and connecting
to the serial socket. The disk travels compressed because a fresh 512 MiB
test disk is almost all zeros and compresses to a few MiB.

The VM matches local QEMU (`make run`) where Proxmox allows: q35, OVMF,
virtio-blk, COM1. Proxmox's own `virtio0` is a transitional virtio-blk device
behind a PCI bridge; GoOS scans every bus and drives it in legacy mode, so no
extra QEMU arguments are needed. Secure Boot keys are not enrolled.

The console connects before the VM starts and waits for QEMU to create the
socket, so `serial.log` holds the whole boot. Closing the console ends
`socat` only; the VM keeps running.

## Threat model

The node is shared work infrastructure. The tool limits what a mistake or a
leaked credential can do:

- The API token is scoped to one pool and three storages. It cannot touch VMs
  outside the pool or change permissions. `pvelab` never calls `pveum`.
- The token secret lives only in `.env`, which git ignores; `pvelab` refuses
  to run if `.env` exists and is not ignored. The secret is never printed,
  never written to `state.json` or `report.json`, and never passed to a child
  process. API errors are scrubbed of it.
- TLS is verified. A node with a Proxmox-issued certificate needs its CA in
  `PVE_CA_FILE`.
- SSH runs with `BatchMode=yes` and `StrictHostKeyChecking=yes`: an unknown
  or changed host key stops the run.
- `pvelab` destroys a VM only when the run journal names it and the VM still
  carries the run's marker: tag `goos-pvelab`, description lines
  `managed-by=goos-pvelab` and `run-id=<run>`, the run's VM name, and
  membership of `PVE_POOL`. Any mismatch stops cleanup and leaves the VM for
  a person to inspect. It never finds VMs by name pattern.
- Remote removal is limited to `/var/tmp/goos-pvelab/<run-id>`; the path is
  checked before `rm -rf`. ISO deletion names the exact volume of the run.
- The VM has no network interface.

SSH access to the node with `sudo` is close to root on the hypervisor. Use a
key kept for this purpose and, where policy allows, a sudo rule limited to
`qm`, `socat`, `sh`, and `pvesm`.

## One-time setup on the node

Run these as an administrator. Names are examples.

Create the pool, a user, and a token with privilege separation:

```sh
pveum pool add goos-lab --comment "GoOS hardware lab"
pveum user add goos-ci@pve
pveum user token add goos-ci@pve goos-lab --privsep 1
```

Grant the user and the token the same roles. A privilege-separated token holds
only what both have.

```sh
for who in "--users goos-ci@pve" "--tokens goos-ci@pve!goos-lab"; do
  pveum acl modify /pool/goos-lab $who --roles PVEVMAdmin,PVEPoolUser
  pveum acl modify /storage/local-lvm $who --roles PVEDatastoreUser
  pveum acl modify /storage/local $who --roles PVEDatastoreAdmin
  pveum acl modify /nodes/pve01 $who --roles PVEAuditor
done
```

`pvelab doctor` lists the exact privileges per path and reports any that are
missing:

| Path | Privileges |
| --- | --- |
| `/pool/<pool>` | `Pool.Audit`, `VM.Allocate`, `VM.Audit`, `VM.PowerMgmt`, `VM.Config.CPU`, `VM.Config.Memory`, `VM.Config.Disk`, `VM.Config.CDROM`, `VM.Config.HWType`, `VM.Config.Options` |
| `/storage/<disk and EFI storage>` | `Datastore.AllocateSpace`, `Datastore.Audit` |
| `/storage/<ISO storage>` | `Datastore.AllocateTemplate` (upload), `Datastore.Allocate` (delete), `Datastore.Audit` |
| `/nodes/<node>` | `Sys.Audit` |

`Datastore.Allocate` on the ISO storage also allows deleting other files
there. A storage used only for lab ISOs keeps that out of reach of everything
else.

The ISO storage must hold `iso` content and the disk and EFI storage `images`
content. The node needs `socat`, `gzip`, and `sha256sum`; a standard Proxmox
install has all three.

## Local setup

```sh
cp .env.example .env
$EDITOR .env
```

Put the node's CA certificate where `PVE_CA_FILE` points. Fetch it over the
SSH connection, which checks the host key:

```sh
ssh -p 22 root@pve.example.invalid cat /etc/pve/pve-root-ca.pem > ~/.config/goos-pvelab/pve-ca.pem
```

Add the node's host key to `~/.ssh/known_hosts` by connecting once by hand.
Then check everything:

```sh
make pve-doctor
```

`doctor` creates nothing. It checks the configuration, `.env` being ignored,
TLS, the API version, the node, the token's privileges, the pool, the three
storages, SSH, `sudo -n`, the tools on the node, the architecture, and
`/dev/kvm`.

Settings come from command-line flags first, then the process environment,
then `.env`, then the defaults in `.env.example`. The token secret has no
flag.

## Runs

```sh
make test-pve-smoke     # boot, uname, ACPI poweroff
make test-pve-lscpu     # lscpu on 2 sockets x 2 cores
make test-pve-smp       # alias for test-pve-lscpu
make test-pve-linux     # the static Linux programs in /system/linux
make test-hardware      # smoke, lscpu, and linux
```

Each target rebuilds the lab inputs with `make pve-artifacts`: the ISO of the
current configuration and a new installed disk at `build/pve/input`. The
developer's `build/goos-disk.img` is never used. `HZ`, `DIAG`, `OPT`,
`VALIDATION`, and `LINUX_EXTRA` apply as for any other target. Extra flags for
`pvelab run` go in `PVE_FLAGS`:

```sh
make test-pve-smoke PVE_FLAGS='--cpu x86-64-v2-AES --memory 2048 --require-clean'
```

The tool can also be run directly:

```sh
go run ./tools/pvelab run --scenario linux --cores 2 --verbose
```

`--verbose` logs every API call, task, and remote command.

Each run writes `build/pve/runs/<run-id>/`:

| File | Contents |
| --- | --- |
| `state.json` | the journal of what exists on the node, updated after every change |
| `report.json` | result, failure stage, checks passed, git commit, VM shape, artifact hashes, cleanup outcome |
| `serial.log` | every byte COM1 sent, from firmware on |
| `vm-config.json` | the VM configuration Proxmox stored |
| `host-info.json` | Proxmox and kernel version, host CPU, `/dev/kvm` |
| `artifacts.json` | path, size, SHA-256, and time of the ISO and disk |

Exit statuses:

| Status | Meaning |
| --- | --- |
| 0 | passed, or a diagnostic workload reached its expected failure |
| 1 | the guest failed: boot failure, panic, or a scenario check |
| 2 | local configuration: settings, `.env`, artifacts, missing `7zzs` or `diag` |
| 3 | infrastructure: API, token, storage, SSH, import, console, interruption |
| 4 | the run finished but cleanup failed |

## Debugging a run

`--keep` (or `PVE_KEEP_VM=1`) leaves the VM and its ISO in place and prints
how to reach them. The smoke scenario skips poweroff so the VM stays up.

```sh
make pve-run
```

```text
[pvelab] VM retained for debugging
[pvelab] run-id: 20261002T111607Z-da38c020
[pvelab] node:   pve01
[pvelab] vmid:   10437
[pvelab] state:  build/pve/runs/20261002T111607Z-da38c020/state.json
[pvelab] serial: ssh -t -p 22 root@pve.example.invalid qm terminal 10437
[pvelab] remove: go run ./tools/pvelab destroy --run 20261002T111607Z-da38c020
```

`qm terminal` exits with Ctrl-O.

## Cleanup after an interrupted run

Ctrl-C and `SIGTERM` stop the scenario and run the normal cleanup. If the
process was killed outright, the journal still names what exists:

```sh
go run ./tools/pvelab destroy --run <run-id>
```

`destroy` applies the ownership checks and treats resources that are already
gone as removed, so it is safe to repeat. To clean every run whose journal
was not marked clean and has not changed for six hours:

```sh
go run ./tools/pvelab cleanup --stale 6h          # asks first
go run ./tools/pvelab cleanup --stale 6h --yes
```

Both read only local journals. A VM made from another checkout is invisible
to them; remove it from that checkout or by hand.

## 7-Zip workflow

The `7zip` scenario runs a static `7zzs` that you supply through a round
trip and records which Linux calls GoOS refused. The repository carries no
7-Zip binary and the tool downloads nothing.

```sh
make test-pve-7zip LINUX_EXTRA=/path/to/7zzs
```

This builds the inputs with `DIAG=1`, so the shell has `diag`, and places
`7zzs` in `/system/linux`. The scenario runs, in order, and stops at the first
step that fails:

| Step | Command |
| --- | --- |
| info | `7zzs i` |
| add | `7zzs a /tmp/pvelab.7z /system/linux/7zzs`, with 7-Zip's own thread count |
| test | `7zzs t /tmp/pvelab.7z` |
| extract | `7zzs x -y -o/tmp/pvelab-out /tmp/pvelab.7z` |
| hash | `7zzs h -scrcSHA256` of the copy and the original; the hashes must match |

A step that holds the prompt past `--workload-timeout` (60s) gets Ctrl-C.
Then the scenario runs `diag` and parses its `DIAG linux` line:

```text
[pvelab] linux syscalls=451 unsupported=2 refused=280 recent=280,280 bad-pointers=0
[pvelab] 7zip PASS
```

`report.json` carries the steps that passed, the ending of the last one, its
fault lines, and every field of `DIAG linux`. A workload that does not
complete is a diagnostic result, `expected-failure`, and exits 0;
`PVE_FLAGS=--require-pass` makes it a failure. The add step runs 7-Zip's
multithreaded LZMA2 encoder (ADR 0177).

glibc needs `AT_RANDOM`, which GoOS supplies only on a CPU with RDRAND (ADR
0151). `cpu=host` on any recent x86_64 has it; under local QEMU use
`-cpu max`.

To see what a workload asks for, trace it on Linux and compare the calls with
the refused set:

```sh
strace -f ./7zzs i
```

## Findings

The first runs on a Xeon Gold 6240 node (PVE 7.4, QEMU 7.2):

- With one vCPU, the smoke and Linux scenarios pass under KVM with
  `cpu=host`, including ACPI poweroff.
- With four vCPUs the kernel panicked before leaving boot services: OVMF
  returned 138 memory descriptors and `boot.Info` held 128. The kernel now
  merges adjacent regions of one kind and holds 256; four and eight vCPUs
  boot.
- The static glibc `7zzs` 26.03 stopped before `main`, because the loader's
  `mprotect` of its relocated data was refused. ADR 0176 lets a Linux program
  narrow its own image and adds the other calls glibc and 7-Zip ask for.
- 7-Zip's multithreaded encoder needed Linux threads, and then wrote corrupt
  archives: two threads shared one kernel file I/O buffer. ADR 0177 adds
  threads with a buffer each; the round trip passes with 7-Zip's default
  thread count.
