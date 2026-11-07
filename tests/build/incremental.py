#!/usr/bin/env python3
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
OUT = None
ARTIFACTS = {
    'netd_elf': Path('userspace/netd.elf'),
    'netd': Path('userspace/netd'),
    'config_elf': Path('userspace/config.elf'),
    'config': Path('userspace/config'),
    'vfs_elf': Path('userspace/vfs.elf'),
    'vfs': Path('userspace/vfs'),
    'command_elf': Path('userspace/bin-elf/netbench4.elf'),
    'command': Path('userspace/bin/netbench4'),
    'other_command_elf': Path('userspace/bin-elf/echo.elf'),
    'other_command': Path('userspace/bin/echo'),
    'kernel': Path('kernel/goos.efi'),
    'archive': Path('initramfs/initramfs.tar'),
    'efi': Path('iso/efi.img'),
    'iso': Path('goos-x86_64.iso'),
}


def snapshot(root):
    return {name: (root / OUT / path).stat().st_mtime_ns for name, path in ARTIFACTS.items()}


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
    args = ['make', '-j4', *targets, 'BUILD_DIR=build']
    args.extend(f'{name}={value}' for name, value in variables.items())
    subprocess.run(args, cwd=root, check=True)


def change(root, source):
    time.sleep(1.1)
    os.utime(root / source, None)


def check_compatibility_roots(root):
    targets = ('build/toolchains/tls/.stamp', 'build/toolchains/ssh/.stamp')
    make(root, *targets)
    stamps = [root / target for target in targets]
    before = [path.stat().st_mtime_ns for path in stamps]
    make(root, *targets)
    if [path.stat().st_mtime_ns for path in stamps] != before:
        raise AssertionError('unchanged compatibility roots were rebuilt')

    caches = []
    for kind in ('tls', 'ssh'):
        paths = [root / 'build/cache' / f'tinygo-home-{kind}' / suffix / 'tinygo'
                 for suffix in ('Library/Caches', 'cache')]
        for path in paths:
            (path / 'goroot-test').mkdir(parents=True, exist_ok=True)
            (path / 'keep').mkdir(exist_ok=True)
        caches.append(paths)

    change(root, 'tools/sshcompat/main.go')
    make(root, *targets)
    after = [path.stat().st_mtime_ns for path in stamps]
    if after[0] != before[0] or after[1] <= before[1]:
        raise AssertionError('SSH input did not rebuild only the SSH root')
    for path in caches[0]:
        if not (path / 'goroot-test').is_dir():
            raise AssertionError('SSH rebuild removed the TLS cache')
    for path in caches[1]:
        if (path / 'goroot-test').exists() or not (path / 'keep').is_dir():
            raise AssertionError('SSH rebuild invalidated the wrong cache entries')
    change(root, 'tools/tlscompat/main.go')
    make(root, *targets)
    final = [path.stat().st_mtime_ns for path in stamps]
    if final[0] <= after[0] or final[1] <= after[1]:
        raise AssertionError('TLS input did not rebuild both compatibility roots')
    for path in caches[0]:
        if (path / 'goroot-test').exists() or not (path / 'keep').is_dir():
            raise AssertionError('TLS rebuild invalidated the wrong cache entries')
    make(root, 'clean')
    if (root / 'build/cache').exists() or (root / 'build/toolchains').exists():
        raise AssertionError('clean left compatibility roots or caches')
    print('PASS compatibility root reuse and invalidation', flush=True)


def run(root):
    global OUT
    out = subprocess.run(
        ['make', '-s', '-j4', '-f', 'Makefile', '-f', '-', '__incremental_out', 'BUILD_DIR=build'],
        input='.PHONY: __incremental_out\n__incremental_out:\n\t@printf "%s\\n" "$(OUT_DIR)"\n',
        cwd=root, text=True, check=True, stdout=subprocess.PIPE,
    ).stdout.strip()
    OUT = Path(out)
    if OUT.is_absolute() or '..' in OUT.parts or not OUT.parts or OUT.parts[0] != 'build':
        raise ValueError(f'build output must be inside the temporary build directory: {out}')
    make(root, 'userspace', 'iso')
    before = snapshot(root)
    make(root, 'userspace', 'iso')
    before = check(root, 'no-op build', unchanged=ARTIFACTS, before=before)

    change(root, 'userspace/cmd/netd/main.go')
    make(root, 'userspace', 'iso')
    before = check(root, 'service isolation', changed=('netd_elf', 'netd'),
                   unchanged=('config_elf', 'config', 'command_elf', 'command',
                              'vfs_elf', 'vfs', 'kernel', 'archive', 'efi', 'iso'), before=before)

    extra = root / 'userspace/cmd/netd/zz_incremental.go'
    time.sleep(1.1)
    extra.write_text('package main\n')
    make(root, 'service-netd')
    before = check(root, 'source addition', changed=('netd_elf', 'netd'),
                   unchanged=('config_elf', 'config', 'command_elf', 'command', 'kernel'), before=before)
    time.sleep(1.1)
    extra.unlink()
    make(root, 'service-netd')
    before = check(root, 'source deletion', changed=('netd_elf', 'netd'),
                   unchanged=('config_elf', 'config', 'command_elf', 'command', 'kernel'), before=before)

    manifest = root / 'userspace/bin/netbench4/main.go'
    held_manifest = root / 'netbench4-manifest.saved'
    time.sleep(1.1)
    manifest.rename(held_manifest)
    try:
        result = subprocess.run(['make', '-j4', 'command-index', 'BUILD_DIR=build'], cwd=root,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if result.returncode == 0:
            raise AssertionError('missing command manifest did not invalidate index')
    finally:
        held_manifest.rename(manifest)
    make(root, 'command-index')
    print('PASS missing command manifest', flush=True)

    change(root, 'userspace/bin/netbench4/main.go')
    make(root, 'userspace', 'iso')
    before = check(root, 'command isolation', changed=('command_elf', 'command'),
                   unchanged=('netd_elf', 'netd', 'config_elf', 'config',
                              'other_command_elf', 'other_command', 'kernel', 'archive', 'efi', 'iso'), before=before)

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
    check_compatibility_roots(root)


def main():
    with tempfile.TemporaryDirectory(prefix='goos-incremental-') as directory:
        root = Path(directory) / 'src'
        shutil.copytree(ROOT, root, symlinks=True,
                        ignore=lambda path, names: {'.git', '.claude', 'build'} & set(names)
                        if Path(path) == ROOT else set())
        run(root)


if __name__ == '__main__':
    main()
