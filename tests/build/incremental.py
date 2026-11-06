#!/usr/bin/env python3
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
OUT = Path('build/variants/hz250-opt2')
ARTIFACTS = {
    'netd_elf': OUT / 'userspace/netd.elf',
    'netd': OUT / 'userspace/netd',
    'config_elf': OUT / 'userspace/config.elf',
    'config': OUT / 'userspace/config',
    'vfs_elf': OUT / 'userspace/vfs.elf',
    'vfs': OUT / 'userspace/vfs',
    'command_elf': OUT / 'userspace/bin-elf/netbench4.elf',
    'command': OUT / 'userspace/bin/netbench4',
    'other_command_elf': OUT / 'userspace/bin-elf/echo.elf',
    'other_command': OUT / 'userspace/bin/echo',
    'kernel': OUT / 'kernel/goos.efi',
    'archive': OUT / 'initramfs/initramfs.tar',
    'efi': OUT / 'iso/efi.img',
    'iso': OUT / 'goos-x86_64.iso',
}


def snapshot(root):
    return {name: (root / path).stat().st_mtime_ns for name, path in ARTIFACTS.items()}


def check(root, label, changed=(), unchanged=(), before=None):
    after = snapshot(root)
    failures = []
    for name in changed:
        if before[name] == after[name]:
            failures.append(f'{name} did not rebuild')
    for name in unchanged:
        if before[name] != after[name]:
            failures.append(f'{name} rebuilt')
    if failures:
        raise AssertionError(f'{label}: ' + ', '.join(failures))
    print(f'PASS {label}', flush=True)
    return after


def make(root, *targets, **variables):
    args = ['make', '-j4', *targets]
    args.extend(f'{name}={value}' for name, value in variables.items())
    subprocess.run(args, cwd=root, check=True)


def change(root, source):
    time.sleep(1.1)
    os.utime(root / source, None)


def run(root):
    make(root, 'userspace', 'iso')
    before = snapshot(root)
    make(root, 'userspace', 'iso')
    before = check(root, 'no-op build', unchanged=ARTIFACTS, before=before)

    change(root, 'userspace/cmd/netd/main.go')
    make(root, 'userspace', 'iso')
    before = check(root, 'service isolation', changed=('netd_elf', 'netd'),
                   unchanged=('config_elf', 'config', 'command_elf', 'command',
                              'vfs_elf', 'vfs', 'kernel', 'archive', 'efi', 'iso'), before=before)

    change(root, 'userspace/bin/netbench4/main.go')
    make(root, 'userspace', 'iso')
    before = check(root, 'command isolation', changed=('command_elf', 'command'),
                   unchanged=('netd_elf', 'netd', 'config_elf', 'config',
                              'other_command_elf', 'other_command', 'kernel'), before=before)

    change(root, 'userspace/tcpapi/protocol.go')
    make(root, 'userspace', 'iso')
    before = check(root, 'shared dependency', changed=('netd_elf', 'netd', 'command_elf', 'command'),
                   before=before)

    change(root, 'tools/elf2native/main.go')
    make(root, 'userspace', 'iso')
    before = check(root, 'image converter', changed=('netd', 'config', 'command', 'vfs'),
                   unchanged=('netd_elf', 'config_elf', 'command_elf', 'vfs_elf', 'kernel'), before=before)

    change(root, 'tools/mkiso/fat.go')
    make(root, 'userspace', 'iso')
    before = check(root, 'ISO tool', changed=('efi', 'iso'),
                   unchanged=('netd_elf', 'netd', 'vfs_elf', 'vfs', 'kernel', 'archive'), before=before)

    change(root, 'kernel/arch/amd64/ap_uefi.go')
    make(root, 'userspace', 'iso')
    before = check(root, 'kernel packaging', changed=('kernel', 'efi', 'iso'),
                   unchanged=('archive', 'netd_elf', 'netd', 'vfs_elf', 'vfs'), before=before)

    change(root, 'userspace/cmd/vfs/main.go')
    make(root, 'userspace', 'iso')
    before = check(root, 'boot service packaging', changed=('vfs_elf', 'vfs', 'archive', 'efi', 'iso'),
                   unchanged=('kernel', 'netd_elf', 'netd'), before=before)

    disk = root / 'existing-disk.img'
    disk.touch()
    make(root, 'run', QEMU='true', DISK=disk)
    check(root, 'run after build', unchanged=ARTIFACTS, before=before)


def main():
    with tempfile.TemporaryDirectory(prefix='goos-incremental-') as directory:
        root = Path(directory) / 'src'
        shutil.copytree(ROOT, root, symlinks=True,
                        ignore=lambda path, names: {'.git', '.claude', 'build'} & set(names)
                        if Path(path) == ROOT else set())
        run(root)


if __name__ == '__main__':
    main()
