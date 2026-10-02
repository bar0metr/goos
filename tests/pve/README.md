# Proxmox hardware tests

These tests boot GoOS on a remote Proxmox VE node under KVM through
`tools/pvelab`. They need the node and the credentials in `.env`, so they
are opt-in and run by no other target:

```sh
make pve-doctor          # check the settings; creates nothing
make test-pve-smoke      # boot, uname, ACPI poweroff
make test-pve-smp        # the same with 4 vCPUs
make test-pve-linux      # the static Linux programs
make test-pve-7zip LINUX_EXTRA=/path/to/7zzs
make test-hardware       # smoke and linux
```

The scenarios are Go code in `tools/pvelab/scenarios.go`. The host-side
tests of the tool need no node and run with `make test-pvelab` and with
`go test ./...`.

Setup, the security model, reports, and cleanup are described in
[docs/development/proxmox-lab.md](../../docs/development/proxmox-lab.md).
