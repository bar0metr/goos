"""Summarizes the qemu-user syscall traces written by trace-tty.sh.

For every scenario directory it prints the distinct syscalls with counts,
the distinct ioctl requests with their file descriptors and results, and
the full lines of the calls that matter for a terminal implementation
(signals, process groups, waits, opens, file changes on save).

usage: python3 -I summarize.py TRACEDIR [scenario ...]
"""

import collections
import json
import os
import re
import sys

LINE = re.compile(r"^(\d+) ([a-z_0-9]+)\((.*)$")
SIGNAL = re.compile(r"^--- (\w+) \{si_signo=\w+, si_code=(\w+)")

# Calls whose every occurrence is printed in full.
DETAIL = {
    "ioctl", "rt_sigaction", "kill", "tkill", "tgkill", "wait4", "waitid",
    "setpgid", "getpgid", "getpgrp", "setsid", "getsid", "getppid", "fork",
    "vfork", "clone", "execve", "open", "openat", "creat", "rename",
    "renameat", "renameat2", "link", "linkat", "unlink", "unlinkat",
    "fsync", "fdatasync", "flock", "fcntl", "fchown", "fchmod", "chown",
    "chmod", "fchmodat", "fchownat", "truncate", "ftruncate", "utimensat",
    "pause", "rt_sigsuspend", "sigaltstack", "dup", "dup2", "dup3",
    "select", "pselect6", "poll", "ppoll", "nanosleep", "alarm",
    "setitimer", "lstat", "stat", "statx", "newfstatat", "access",
    "faccessat", "faccessat2", "readlink", "getcwd", "chdir", "umask",
    "getuid", "geteuid", "getgid", "getegid", "rt_sigprocmask", "pipe",
    "pipe2", "exit_group", "exit", "close", "read", "write", "writev",
    "readv", "lseek", "getdents64", "mkdir", "uname", "sysinfo",
    "prlimit64", "getrlimit", "sched_yield", "setfsuid", "setfsgid",
}
NOISE = {"mmap", "munmap", "brk", "mprotect", "clock_gettime", "madvise",
         "mremap"}


def calls(path):
    with open(path, errors="replace") as f:
        for raw in f:
            m = SIGNAL.match(raw)
            if m:
                yield "-", "signal", "%s %s" % m.groups()
                continue
            m = LINE.match(raw.rstrip("\n"))
            if m:
                yield m.group(1), m.group(2), m.group(3)


def ioctl_key(args):
    parts = args.split(",", 2)
    fd = parts[0]
    req = parts[1] if len(parts) > 1 else "?"
    return fd, req


def result(args):
    i = args.rfind(") = ")
    return args[i + 4:] if i >= 0 else "(no result)"


def short_result(res):
    res = res.split(" ({", 1)[0]
    return res


def summarize(scenario_dir, full):
    name = os.path.basename(scenario_dir.rstrip("/"))
    counts = collections.Counter()
    ioctls = collections.Counter()
    sigactions = collections.Counter()
    delivered = collections.Counter()
    detail = []
    pids = set()
    for log in sorted(os.listdir(scenario_dir)):
        if not log.startswith("strace."):
            continue
        for pid, call, args in calls(os.path.join(scenario_dir, log)):
            pids.add(pid)
            counts[call] += 1
            if call == "signal":
                delivered[args] += 1
                if full:
                    detail.append("--- signal %s ---" % args)
                continue
            if call == "ioctl":
                fd, req = ioctl_key(args)
                ioctls[(req, fd, short_result(result(args)))] += 1
            if call == "rt_sigaction":
                sig, act = args.split(",", 2)[:2]
                sigactions[(sig, "set" if act != "NULL" else "query")] += 1
            if full and call in DETAIL and call not in ("read", "write",
                                                         "writev"):
                detail.append("%s %s(%s" % (pid, call, args[:300]))
    print("## %s (%d processes)" % (name, len(pids)))
    events = os.path.join(scenario_dir, "events.json")
    if os.path.exists(events):
        for e in json.load(open(events)):
            if e["event"] in ("termios", "child-stopped", "child-continued",
                              "child-exited", "fg-pgrp", "timeout", "file"):
                e = dict(e)
                e.pop("cc", None)
                print("  event", json.dumps(e, sort_keys=True))
    print("  syscalls:", ", ".join(
        "%s %d" % kv for kv in sorted(counts.items()) if kv[0] not in NOISE))
    print("  noise:", ", ".join(
        "%s %d" % kv for kv in sorted(counts.items()) if kv[0] in NOISE))
    print("  ioctls:")
    for (req, fd, res), n in sorted(ioctls.items()):
        print("    %-14s fd=%-3s -> %-30s x%d" % (req, fd, res, n))
    print("  rt_sigaction:", ", ".join(
        "%s/%s x%d" % (s, k, n) for (s, k), n in sorted(sigactions.items())))
    print("  delivered:", ", ".join(
        "%s x%d" % kv for kv in sorted(delivered.items())))
    if full:
        print("  detail:")
        for line in detail:
            print("    " + line)
    print()


def main():
    root = sys.argv[1]
    names = sys.argv[2:] or sorted(os.listdir(root))
    full = os.environ.get("DETAIL") == "1"
    for name in names:
        path = os.path.join(root, name)
        if os.path.isdir(path):
            summarize(path, full)


if __name__ == "__main__":
    main()
