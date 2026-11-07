#!/usr/bin/env python3
"""Independent make processes that share one build directory.

Each scenario starts separate OS processes, not jobs of one make, against a
copy of the source tree. Readers that must stay put while a writer or clean
waits are real `rootlock shared` processes: the same path TinyGo builds use.
CONCURRENCY_ITERATIONS repeats the light scenarios; the full TinyGo builds
run in the first iteration only.
"""
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
KINDS = ('crypto', 'goroutines', 'ssh', 'tls')
STAMPS = {'crypto': '.stamp', 'goroutines': 'goroutineroot.json', 'ssh': '.stamp', 'tls': '.stamp'}
OUT = 'build/variants/hz250-opt2'
ENV = {name: value for name, value in os.environ.items()
       if name not in ('MAKEFLAGS', 'MFLAGS', 'MAKELEVEL', 'GOOS_ROOT_LOCK')}
ENV['GOTOOLCHAIN'] = 'local'


def log(message):
    print(message, flush=True)


def start(tree, *args, **kwargs):
    return subprocess.Popen(args, cwd=tree, env=ENV, start_new_session=True, **kwargs)


def make(tree, *targets):
    return start(tree, 'make', '-j4', *targets)


def finish(label, *processes, timeout=1800):
    for process in processes:
        if process.wait(timeout=timeout) != 0:
            raise AssertionError(f'{label}: {process.args} exited {process.returncode}')


def stamp(tree, kind, build='build'):
    return tree / build / 'toolchains' / kind / STAMPS[kind]


class Holder:
    """A process that holds a root lock until released."""

    def __init__(self, tree, tool, mode, root):
        self.process = start(tree, str(tool), mode, str(root), '--', 'sh', '-c',
                             'echo held; exec sleep 600', stdout=subprocess.PIPE, text=True)
        if self.process.stdout.readline().strip() != 'held':
            raise AssertionError(f'{mode} lock on {root} was not taken')

    def release(self):
        self.process.terminate()
        self.process.wait(timeout=30)
        self.process.stdout.close()


def tinygo_cache():
    state = {}
    for path in (Path.home() / 'Library/Caches/tinygo', Path.home() / '.cache/tinygo'):
        if path.exists():
            state[path] = (path.stat().st_mtime_ns, sorted(os.listdir(path)))
    return state


def check_tree(tree, label, build='build'):
    toolchains = tree / build / 'toolchains'
    if not toolchains.exists():
        return
    failures = []
    for entry in toolchains.iterdir():
        if '.stage.' in entry.name:
            failures.append(f'stage left at {entry}')
    for kind in KINDS:
        root = toolchains / kind
        if not root.exists():
            continue
        if not stamp(tree, kind, build).is_file():
            failures.append(f'{kind} root has no stamp')
        for directory, names, files in os.walk(root):
            for name in names + files:
                path = Path(directory) / name
                if '.stage.' in name:
                    failures.append(f'nested stage {path}')
                if path.is_symlink() and not path.exists():
                    failures.append(f'broken symlink {path}')
    if failures:
        raise AssertionError(f'{label}: ' + '; '.join(failures[:10]))


def sentinel(tree, kind):
    path = tree / 'build/cache' / f'tinygo-home-{kind}' / ('Library/Caches' if sys.platform == 'darwin' else 'cache') / 'tinygo' / 'goroot-sentinel'
    path.mkdir(parents=True, exist_ok=True)
    return path


def writer_waits_for_readers(tree, tool, kind):
    """Two readers hold the root; a forced rebuild must not touch it until both leave."""
    target = str(stamp(tree, kind).relative_to(tree))
    root = tree / 'build/toolchains' / kind
    before = stamp(tree, kind).stat().st_ino
    cache = sentinel(tree, kind)
    readers = [Holder(tree, tool, 'shared', root) for _ in range(2)]
    writer = make(tree, '-B', target)
    time.sleep(3)
    try:
        if writer.poll() is not None:
            raise AssertionError(f'{kind}: the writer finished while readers held the root')
        if stamp(tree, kind).stat().st_ino != before or list(root.parent.glob(f'{kind}.stage.*')):
            raise AssertionError(f'{kind}: the writer changed the root under its readers')
        if not cache.exists():
            raise AssertionError(f'{kind}: the writer removed a goroot cache entry under its readers')
    finally:
        for reader in readers:
            reader.release()
    finish(f'{kind} writer', writer)
    if stamp(tree, kind).stat().st_ino == before or cache.exists():
        raise AssertionError(f'{kind}: the writer did not publish after its readers left')
    check_tree(tree, f'{kind} readers and writer')
    log(f'PASS {kind} writer waits for readers')


def forced_writers(tree):
    target = str(stamp(tree, 'tls').relative_to(tree))
    finish('forced TLS writers', make(tree, '-B', target), make(tree, '-B', target))
    check_tree(tree, 'forced TLS writers')
    log('PASS two forced TLS writers')


def killed_writer(tree, tool):
    target = str(stamp(tree, 'tls').relative_to(tree))
    root = tree / 'build/toolchains/tls'
    before = stamp(tree, 'tls').stat().st_ino
    writer = make(tree, '-B', target)
    deadline = time.monotonic() + 120
    while not list(root.parent.glob('tls.stage.*')):
        if writer.poll() is not None or time.monotonic() > deadline:
            raise AssertionError('the TLS writer never staged')
        time.sleep(0.05)
    os.killpg(writer.pid, signal.SIGKILL)
    writer.wait()
    if stamp(tree, 'tls').stat().st_ino != before:
        raise AssertionError('a killed writer replaced the TLS root')
    probe = start(tree, str(tool), 'exclusive', str(root), '--', 'true')
    finish('lock after a killed writer', probe, timeout=30)
    finish('writer after a killed writer', make(tree, '-B', target))
    check_tree(tree, 'killed writer')
    log('PASS killed writer leaves the root and no lock')


def separate_build_dirs(tree, tool):
    holder = Holder(tree, tool, 'exclusive', tree / 'build/toolchains/tls')
    try:
        other = 'build/other'
        finish('second build directory',
               make(tree, f'BUILD_DIR={other}', f'{other}/toolchains/tls/.stamp'), timeout=600)
        check_tree(tree, 'second build directory', other)
    finally:
        holder.release()
    log('PASS separate build directories do not wait for each other')


def clean_waits(tree, tool):
    reader = Holder(tree, tool, 'shared', tree / 'build/toolchains/tls')
    cleaner = make(tree, 'clean')
    time.sleep(3)
    try:
        if cleaner.poll() is not None:
            raise AssertionError('clean finished while a reader held a root')
        if not stamp(tree, 'tls').is_file() or not (tree / 'build/cache').is_dir():
            raise AssertionError('clean removed state under an active reader')
    finally:
        reader.release()
    finish('clean', cleaner)
    leftovers = list(tree.glob('build*'))
    if leftovers:
        raise AssertionError(f'clean left {leftovers}')
    log('PASS clean waits for readers and removes the build directory')


def full_builds(tree):
    finish('SSH services', make(tree, f'{OUT}/userspace/remoted.elf'),
           make(tree, f'{OUT}/userspace/remoted-worker.elf'))
    log('PASS remoted and remoted-worker from separate make processes')
    readers = [make(tree, f'{OUT}/userspace/vfs.elf'), make(tree, f'{OUT}/userspace/bin-elf/goprobe.elf')]
    time.sleep(2)
    writer = make(tree, '-B', str(stamp(tree, 'goroutines').relative_to(tree)))
    finish('goroutine readers and writer', *readers, writer)
    check_tree(tree, 'goroutine builds')
    log('PASS goroutine builds beside a root rebuild')
    work = tree / 'build/crypto-probes'
    probes = subprocess.Popen(['sh', 'tools/crypto-check.sh', str(work)], cwd=tree,
                              start_new_session=True,
                              env=dict(ENV, CRYPTO_COMPAT='1', CRYPTO_PACKAGES='platform sha512 hmac'))
    time.sleep(2)
    writer = make(tree, '-B', str(stamp(tree, 'crypto').relative_to(tree)))
    finish('crypto probes and writer', probes, writer)
    check_tree(tree, 'crypto builds')
    log('PASS crypto probes beside a root rebuild')


def main():
    iterations = int(os.environ.get('CONCURRENCY_ITERATIONS', '1'))
    cache_before = tinygo_cache()
    with tempfile.TemporaryDirectory(prefix='goos-concurrency-') as directory:
        tree = Path(directory) / 'src'
        shutil.copytree(ROOT, tree, symlinks=True,
                        ignore=lambda path, names: {'.git', '.claude', 'build'} & set(names)
                        if Path(path) == ROOT else set())
        tool = Path(directory) / 'rootlock'
        subprocess.run(['go', 'build', '-o', str(tool), './tools/rootlock'], cwd=tree, env=ENV, check=True)
        for iteration in range(1, iterations + 1):
            log(f'iteration {iteration}/{iterations}')
            forced_writers(tree)
            finish('roots', make(tree, *(str(stamp(tree, kind).relative_to(tree)) for kind in ('goroutines', 'crypto'))))
            if iteration == 1:
                full_builds(tree)
            writer_waits_for_readers(tree, tool, 'goroutines')
            writer_waits_for_readers(tree, tool, 'crypto')
            killed_writer(tree, tool)
            separate_build_dirs(tree, tool)
            clean_waits(tree, tool)
    if tinygo_cache() != cache_before:
        raise AssertionError('the global TinyGo cache changed')
    log('PASS build concurrency')


if __name__ == '__main__':
    main()
