/*
 * Linux ABI conformance probe. One static musl program that checks what a
 * GoOS Linux process gets, result by result, and prints a line for each
 * group. The same binary runs on a Linux kernel, where it must print the
 * same lines: that run is the reference the GoOS results are held to.
 *
 * With two ordinary arguments it checks, in order: arguments and
 * environment, TLS, the heaps, the VFS session, path resolution, and then in
 * a scratch directory under /tmp file I/O, file status in both forms,
 * positioned I/O, stdio, directory listing, vectors, descriptors, and
 * naming. Last come the devices, identity, the system name, the clocks, a
 * limit, a sleep, the signal state, and random bytes.
 *
 * GoOS starts it as /system/linux/abi-probe with /system/linux as the
 * working directory. GOOS_LINUX_LONG, when set, names an existing file whose
 * path is too long for one request message. A few results differ on GoOS by
 * design; the program tells GoOS from Linux by the version field of uname.
 *
 * A Linux run under user-mode emulation answers a few hostile calls in its
 * own way, so the program accepts those answers there. ABI_PROBE_STRICT in
 * the environment says the kernel underneath is a real one and must give
 * the exact Linux result, which GoOS is always held to.
 *
 * A first argument selects another mode:
 *
 *   hostile          bad pointers, lengths, and descriptors for every call
 *   exhaust          descriptor table and page quota run out and recover
 *   isolation        the native GoOS call gate refuses a Linux process
 *   port-io          an I/O port access ends the process
 *   pair NAME [drain] two instances keep separate state in one pipeline
 *   block            wait on standard input until it ends
 *   sleep            sleep for a minute
 *   bench            time four calls and print nanoseconds per call
 *   session-loss     report whether the VFS session survives a VFS restart
 *   session-timeout  open a name the validation VFS answers late
 *   stack-overflow   recurse until the stack guard ends the process
 *   random           say whether the kernel has a random source
 *   exec              replace this image and verify the retained state
 *   fork              create a child and check what it shares and copies
 *   vfork             suspend in vfork until the child exits; clone a child
 *   orphan            a grandchild whose parent exits is reparented to PID 1
 *   wait              reap children by PID and by any, with and without WNOHANG
 *   pipe              pipe and pipe2: direction, EOF, EPIPE, lifetime, PIPE_BUF
 *   signal            SIGPIPE and SIGCHLD delivery, handlers, masks, rt_sigreturn
 *   kill              kill, tkill, tgkill, and SA_NOCLDWAIT on the process tree
 *   eintr             signals interrupt read, wait4, nanosleep, and poll; SA_RESTART
 *   poll              poll readiness, HUP, ERR, NVAL, timeouts, and wake-ups
 *   ppoll             ppoll and rt_sigsuspend with a temporary signal mask
 *   family            leave a child asleep and wait on standard input
 *   path              print the PATH the program started with
 *   run PATH ARGS...  replace this image with PATH and ARGS
 *   hardening         failed exec, fork memory, descriptors, pipes, signal frames
 *   limits            fork until the family budget refuses, with zombies too
 *   fork-fault        a validation kernel fails a fork mid-copy; another succeeds
 *   fpstate           x87 and SSE state survives calls, sleeps, fork, and handlers
 *   exec-vfs-loss     the validation VFS dies while execve prepares its image
 *   libc              what glibc asks at startup: RELRO mprotect, robust list,
 *                     umask, readlink, futex, gettimeofday, time, sysinfo
 *   threads           pthreads: a mutex and a condition shared by three threads,
 *                     join values, TIDs, TLS, and exit_group from a thread
 *   smp               workers on every online processor at once, a contended
 *                     mutex, remaps and faults beside readers, fork and exec
 *                     beside running threads
 *   kernel            threads and processes making calls that share no lock,
 *                     so several processors run kernel code at once
 *   domains [tag]     files, a pipe, and fork in four threads, the lock
 *                     profile's workload; copies run with different tags
 *
 * exhaust, isolation, and port-io make sense on GoOS only.
 */
#define _GNU_SOURCE
#include <dirent.h>
#include <elf.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sched.h>
#include <signal.h>
#include <sys/auxv.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/random.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/sysinfo.h>
#include <sys/time.h>
#include <sys/sysmacros.h>
#include <sys/uio.h>
#include <sys/utsname.h>
#include <sys/wait.h>
#include <time.h>
#include <ucontext.h>
#include <unistd.h>

#define IMAGE "/system/linux/abi-probe"

/* GoOS names itself in the version field of uname. A few results differ
 * there by design, and the checks that expect them ask this first. */
static int on_goos(void)
{
	struct utsname name;

	return uname(&name) == 0 && !strncmp(name.version, "GoOS ", 5);
}

/* A kernel whose answers must be the exact Linux ones: GoOS, or a Linux
 * kernel the caller vouched for. */
static int strict_kernel(void)
{
	return on_goos() || getenv("ABI_PROBE_STRICT") != NULL;
}

/* The scratch directory of this instance; enter_scratch names it. */
static char scratch[48];

static _Thread_local int tls_data = 40;
static _Thread_local int tls_zero;

static int heap_ok(void)
{
	size_t large_size = 1 << 20;
	char *small = malloc(64);
	char *large = malloc(large_size);
	int ok = small && large;

	if (ok) {
		memset(small, 0x5a, 64);
		memset(large, 0xa5, large_size);
		ok = small[63] == 0x5a && (unsigned char)large[large_size - 1] == 0xa5;
	}
	free(large);
	free(small);
	return ok;
}

static int session_ok(const char *self)
{
	for (int round = 0; round < 2; round++) {
		int fd = openat(AT_FDCWD, self, O_RDONLY | O_CLOEXEC);

		/* Descriptors 0 to 2 are the standard streams; 3 is free again
		 * after each close. */
		if (fd != 3 || close(fd) != 0)
			return 0;
		errno = 0;
		if (close(fd) != -1 || errno != EBADF)
			return 0;
	}
	errno = 0;
	return openat(AT_FDCWD, "/system/linux/absent", O_RDONLY) == -1 && errno == ENOENT;
}

static int opens(int dirfd, const char *path)
{
	int fd = openat(dirfd, path, O_RDONLY);

	return fd == 3 && close(fd) == 0;
}

static int refuses(int dirfd, const char *path, int reason)
{
	errno = 0;
	return openat(dirfd, path, O_RDONLY) == -1 && errno == reason;
}

static int paths_ok(void)
{
	static char name[1200];
	const char *existing = getenv("GOOS_LINUX_LONG");
	/* musl's open() is the legacy open call on x86_64, not openat. */
	int fd = open("abi-probe", O_RDONLY);

	if (fd != 3 || close(fd) != 0)
		return 0;
	if (!opens(AT_FDCWD, "./abi-probe") ||
	    !opens(AT_FDCWD, "..//linux/./abi-probe") ||
	    !opens(AT_FDCWD, "../../../system/linux/abi-probe") ||
	    !opens(1, IMAGE))
		return 0;
	if (!refuses(AT_FDCWD, "", ENOENT) ||
	    !refuses(1, "abi-probe", ENOTDIR) ||
	    !refuses(42, "abi-probe", EBADF))
		return 0;
	if (existing && !opens(AT_FDCWD, existing))
		return 0;
	/* A 250-byte name is legal on Linux and on GoOS, and with its
	 * directory it is too long for one request message. 1100 bytes are
	 * over both name limits. */
	memset(name, 'n', sizeof name - 1);
	name[250] = 0;
	if (!refuses(AT_FDCWD, name, ENOENT))
		return 0;
	name[250] = 'n';
	name[1100] = 0;
	return refuses(AT_FDCWD, name, ENAMETOOLONG);
}

/* The bundled musl has no stat directory, which is where mkdir lives. */
static int make_directory(int dirfd, const char *path)
{
	return syscall(SYS_mkdirat, dirfd, path, 0755);
}

static int fails(long result, int reason)
{
	int failed = result == -1 && errno == reason;

	errno = 0;
	return failed;
}

static int position_ok(int fd, off_t offset, int whence, off_t want)
{
	return lseek(fd, offset, whence) == want;
}

static int data_ok(void)
{
	static char pattern[8192];
	char back[300];
	struct iovec pieces[2] = {{"ab", 2}, {"cde", 3}};
	int fd = open("data", O_RDWR | O_CREAT | O_EXCL, 0644);

	if (fd != 3 || !fails(open("data", O_WRONLY | O_CREAT | O_EXCL, 0644), EEXIST))
		return 0;
	for (size_t index = 0; index < sizeof pattern; index++)
		pattern[index] = (char)(index * 7 + 3);
	if (write(fd, pattern, sizeof pattern) != sizeof pattern ||
	    !position_ok(fd, 0, SEEK_CUR, 8192) ||
	    !position_ok(fd, -4096, SEEK_END, 4096) ||
	    !position_ok(fd, 100, SEEK_SET, 100) ||
	    !fails(lseek(fd, -1, SEEK_SET), EINVAL) ||
	    !fails(lseek(1, 0, SEEK_CUR), ESPIPE))
		return 0;
	if (read(fd, back, sizeof back) != sizeof back ||
	    memcmp(back, pattern + 100, sizeof back) ||
	    !position_ok(fd, 0, SEEK_CUR, 400) ||
	    !position_ok(fd, 8000, SEEK_SET, 8000) ||
	    read(fd, back, sizeof back) != 192 ||
	    memcmp(back, pattern + 8000, 192) ||
	    read(fd, back, sizeof back) != 0 || close(fd) != 0)
		return 0;

	fd = open("data", O_WRONLY | O_APPEND);
	if (fd != 3 || !fails(read(fd, back, 1), EBADF) ||
	    write(fd, "tail", 4) != 4 || !position_ok(fd, 0, SEEK_CUR, 8196) ||
	    close(fd) != 0)
		return 0;
	fd = open("data", O_RDONLY);
	if (fd != 3 || !position_ok(fd, -4, SEEK_END, 8192) ||
	    read(fd, back, sizeof back) != 4 || memcmp(back, "tail", 4) ||
	    !fails(write(fd, "x", 1), EBADF) || close(fd) != 0)
		return 0;

	fd = open("data", O_WRONLY | O_TRUNC);
	if (fd != 3 || !position_ok(fd, 0, SEEK_END, 0) ||
	    writev(fd, pieces, 2) != 5 || close(fd) != 0)
		return 0;
	fd = open("data", O_RDONLY);
	return fd == 3 && read(fd, back, sizeof back) == 5 &&
	       !memcmp(back, "abcde", 5) && close(fd) == 0;
}

/* The bundled musl has no stat wrappers either; the calls below are the ones
 * its stat(), lstat(), fstat(), and fstatat() make on x86_64. */
static int status_ok(const char *self, int read_only_image)
{
	struct stat by_path, by_fd, other, directory, image;
	int dirfd, fd = open("data", O_RDONLY);

	if (fd != 3 || syscall(SYS_stat, "data", &by_path) != 0 ||
	    syscall(SYS_fstat, fd, &by_fd) != 0 || close(fd) != 0)
		return 0;
	/* One file has one identity, by name and by descriptor. */
	if (!S_ISREG(by_path.st_mode) || (by_path.st_mode & 0777) != 0644 ||
	    by_path.st_size != 5 || by_fd.st_size != 5 ||
	    by_path.st_nlink != 1 || by_path.st_blksize <= 0 ||
	    by_path.st_ino == 0 || by_path.st_ino != by_fd.st_ino ||
	    by_path.st_dev != by_fd.st_dev)
		return 0;
	if (syscall(SYS_lstat, "data", &other) != 0 ||
	    other.st_ino != by_path.st_ino ||
	    syscall(SYS_newfstatat, AT_FDCWD, "./data", &other, AT_SYMLINK_NOFOLLOW) != 0 ||
	    other.st_ino != by_path.st_ino || other.st_size != 5)
		return 0;
	/* The directory is another node of the same filesystem. */
	dirfd = open(".", O_RDONLY | O_DIRECTORY);
	if (dirfd != 3 || syscall(SYS_stat, ".", &directory) != 0 ||
	    !S_ISDIR(directory.st_mode) || directory.st_nlink < 1 ||
	    directory.st_dev != by_path.st_dev ||
	    directory.st_ino == by_path.st_ino ||
	    syscall(SYS_newfstatat, dirfd, "", &other, AT_EMPTY_PATH) != 0 ||
	    !S_ISDIR(other.st_mode) || other.st_ino != directory.st_ino ||
	    syscall(SYS_fstat, dirfd, &other) != 0 ||
	    other.st_ino != directory.st_ino ||
	    syscall(SYS_newfstatat, dirfd, "data", &other, 0) != 0 ||
	    other.st_ino != by_path.st_ino || close(dirfd) != 0)
		return 0;
	/* The program image is a different file. Standard output is a pipe
	 * here and may be a terminal elsewhere. */
	if (syscall(SYS_stat, self, &image) != 0 || !S_ISREG(image.st_mode) ||
	    image.st_size <= 0 ||
	    (image.st_dev == by_path.st_dev && image.st_ino == by_path.st_ino) ||
	    (read_only_image && (image.st_mode & 0222)) ||
	    syscall(SYS_fstat, 1, &other) != 0 ||
	    (!S_ISFIFO(other.st_mode) && !S_ISCHR(other.st_mode)))
		return 0;
	/* access answers from the same mode. */
	if (access("data", F_OK) != 0 || access("data", R_OK | W_OK) != 0 ||
	    access(".", R_OK | X_OK) != 0 || !fails(access("missing", F_OK), ENOENT) ||
	    !fails(access("data/", F_OK), ENOTDIR) ||
	    (read_only_image && !fails(access(self, W_OK), EACCES)))
		return 0;
	errno = 0;
	return fails(syscall(SYS_stat, "missing", &other), ENOENT) &&
	       fails(syscall(SYS_stat, "data/", &other), ENOTDIR) &&
	       fails(syscall(SYS_fstat, 42, &other), EBADF) &&
	       fails(syscall(SYS_fstat, AT_FDCWD, &other), EBADF) &&
	       fails(syscall(SYS_newfstatat, AT_FDCWD, "data", &other, 0x10000000), EINVAL) &&
	       fails(syscall(SYS_stat, "data", (void *)8), EFAULT);
}

/* struct statx as the kernel defines it. The bundled musl predates it. */
struct extended_time {
	long long seconds;
	unsigned int nanoseconds;
	int unused;
};

struct extended_status {
	unsigned int mask, block_size;
	unsigned long long attributes;
	unsigned int links, uid, gid;
	unsigned short mode, padding;
	unsigned long long inode, size, blocks, attributes_mask;
	struct extended_time accessed, born, changed, modified;
	unsigned int rdev_major, rdev_minor, dev_major, dev_minor;
	unsigned long long spare[14];
};

#define EXTENDED_BASIC 0x7ffU
#define EXTENDED_BORN 0x800U

static long extended(int dirfd, const char *path, int flags, unsigned int mask, void *status)
{
	return syscall(SYS_statx, dirfd, path, flags, mask, status);
}

/* statx reports what stat reports, in its own layout. */
static int extended_status_ok(int strict)
{
	struct extended_status by_path, by_fd;
	struct stat plain;
	int fd = open("data", O_RDONLY);

	if (sizeof by_path != 256 || fd != 3 || syscall(SYS_stat, "data", &plain) != 0)
		return 0;
	memset(&by_path, 0xff, sizeof by_path);
	if (extended(AT_FDCWD, "data", 0, EXTENDED_BASIC, &by_path) != 0 ||
	    (by_path.mask & EXTENDED_BASIC) != EXTENDED_BASIC ||
	    by_path.size != 5 || by_path.mode != (S_IFREG | 0644) ||
	    by_path.links != 1 || by_path.block_size == 0 ||
	    by_path.inode != plain.st_ino || by_path.uid != plain.st_uid ||
	    by_path.gid != plain.st_gid || by_path.blocks != (unsigned long long)plain.st_blocks ||
	    makedev(by_path.dev_major, by_path.dev_minor) != plain.st_dev ||
	    by_path.rdev_major != 0 || by_path.rdev_minor != 0 ||
	    by_path.modified.seconds != plain.st_mtim.tv_sec ||
	    by_path.modified.nanoseconds != (unsigned int)plain.st_mtim.tv_nsec ||
	    ((by_path.mask & EXTENDED_BORN) && by_path.born.seconds <= 0))
		return 0;
	/* An empty path with AT_EMPTY_PATH names the descriptor; so does a
	 * null one on Linux 6.11 and later. The sync flags change nothing. */
	if (extended(fd, "", AT_EMPTY_PATH, EXTENDED_BASIC | EXTENDED_BORN, &by_fd) != 0 ||
	    by_fd.inode != by_path.inode || by_fd.size != 5 ||
	    (strict && (extended(fd, NULL, AT_EMPTY_PATH, 0, &by_fd) != 0 ||
			by_fd.inode != by_path.inode)) ||
	    extended(AT_FDCWD, "data", 0x2000, 0, &by_fd) != 0 ||
	    extended(AT_FDCWD, ".", 0x4000 | AT_SYMLINK_NOFOLLOW, EXTENDED_BASIC, &by_fd) != 0 ||
	    !S_ISDIR(by_fd.mode) || by_fd.inode == by_path.inode ||
	    makedev(by_fd.dev_major, by_fd.dev_minor) != plain.st_dev ||
	    close(fd) != 0)
		return 0;
	errno = 0;
	return fails(extended(AT_FDCWD, "missing", 0, EXTENDED_BASIC, &by_fd), ENOENT) &&
	       fails(extended(AT_FDCWD, "data/", 0, EXTENDED_BASIC, &by_fd), ENOTDIR) &&
	       fails(extended(AT_FDCWD, "data", 0x6000, EXTENDED_BASIC, &by_fd), EINVAL) &&
	       fails(extended(AT_FDCWD, "data", 0, 0x80000000U, &by_fd), EINVAL) &&
	       fails(extended(AT_FDCWD, "data", 0x10000000, EXTENDED_BASIC, &by_fd), EINVAL) &&
	       fails(extended(42, "data", 0, EXTENDED_BASIC, &by_fd), EBADF) &&
	       fails(extended(AT_FDCWD, "data", 0, EXTENDED_BASIC, (void *)8), EFAULT);
}

/* pread and pwrite move bytes at an offset and leave the file position
 * where it was. */
static int positions_ok(void)
{
	char back[16];
	int dirfd, fd = open("placed", O_RDWR | O_CREAT | O_TRUNC, 0644);

	if (fd != 3 || write(fd, "0123456789", 10) != 10 ||
	    pread(fd, back, 4, 3) != 4 || memcmp(back, "3456", 4) ||
	    !position_ok(fd, 0, SEEK_CUR, 10) ||
	    pwrite(fd, "XY", 2, 1) != 2 || !position_ok(fd, 0, SEEK_CUR, 10) ||
	    pread(fd, back, sizeof back, 0) != 10 || memcmp(back, "0XY3456789", 10) ||
	    pread(fd, back, sizeof back, 10) != 0 || pread(fd, back, sizeof back, 1000) != 0 ||
	    pread(fd, back, 0, 0) != 0 || pwrite(fd, back, 0, 0) != 0)
		return 0;
	/* A write at the end extends the file, and the position still holds:
	 * the next plain write lands at 10, over what pwrite put there. */
	if (pwrite(fd, "ab", 2, 10) != 2 || !position_ok(fd, 0, SEEK_CUR, 10) ||
	    write(fd, "c", 1) != 1 || !position_ok(fd, 0, SEEK_CUR, 11) ||
	    pread(fd, back, sizeof back, 8) != 4 || memcmp(back, "89cb", 4) ||
	    close(fd) != 0)
		return 0;
	/* Access modes hold for the positioned calls too. */
	fd = open("placed", O_RDONLY);
	if (fd != 3 || !fails(pwrite(fd, "x", 1, 0), EBADF) || pread(fd, back, 1, 11) != 1 ||
	    back[0] != 'b' || close(fd) != 0)
		return 0;
	fd = open("placed", O_WRONLY);
	if (fd != 3 || !fails(pread(fd, back, 1, 0), EBADF) || close(fd) != 0)
		return 0;
	/* O_APPEND wins over the offset, as Linux documents. */
	fd = open("placed", O_RDWR | O_APPEND);
	if (fd != 3 || pwrite(fd, "z", 1, 0) != 1 || !position_ok(fd, 0, SEEK_CUR, 0) ||
	    pread(fd, back, sizeof back, 0) != 13 || memcmp(back, "0XY345678", 9) ||
	    back[12] != 'z' || close(fd) != 0)
		return 0;
	dirfd = open(".", O_RDONLY | O_DIRECTORY);
	errno = 0;
	return dirfd == 3 && fails(pread(dirfd, back, 1, 0), EISDIR) &&
	       fails(pwrite(dirfd, back, 1, 0), EBADF) && close(dirfd) == 0 &&
	       fails(pread(1, back, 1, 0), ESPIPE) && fails(pwrite(1, back, 1, 0), ESPIPE) &&
	       fails(pread(42, back, 1, 0), EBADF) && fails(pwrite(42, back, 1, 0), EBADF) &&
	       /* Linux refuses a negative offset before it looks at the
		* descriptor. */
	       fails(syscall(SYS_pread64, 42, back, 1, -1L), EINVAL) &&
	       fails(syscall(SYS_pwrite64, 1, back, 1, -1L), EINVAL) &&
	       unlink("placed") == 0;
}

/* musl stdio writes with writev. It fills its buffer with read, and reads a
 * larger request with readv: the caller's memory first, its buffer second. */
static int stdio_ok(void)
{
	static const char text[] = "first\nsecond 2\n";
	char line[32];
	FILE *file = fopen("lines", "w");

	if (!file || fprintf(file, "first\nsecond %d\n", 2) < 0 || fclose(file) != 0)
		return 0;
	file = fopen("lines", "r");
	if (!file || !fgets(line, sizeof line, file) || strcmp(line, "first\n") ||
	    !fgets(line, sizeof line, file) || strcmp(line, "second 2\n") ||
	    fgets(line, sizeof line, file) || !feof(file))
		return 0;
	rewind(file);
	if (fread(line, 1, sizeof line, file) != sizeof text - 1 ||
	    memcmp(line, text, sizeof text - 1) || fclose(file) != 0)
		return 0;
	/* Standard input is at its end in every run of this mode. */
	return fread(line, 1, sizeof line, stdin) == 0 && feof(stdin) &&
	       unlink("lines") == 0;
}

/* The record getdents64 fills. The bundled musl has no readdir. */
struct listed {
	unsigned long long inode;
	long long next;
	unsigned short length;
	unsigned char type;
	char name[];
};

static int touch(const char *path)
{
	int fd = open(path, O_WRONLY | O_CREAT, 0644);

	return fd == 3 && close(fd) == 0;
}

static int listing_ok(void)
{
	static char long_name[201], buffer[512];
	struct stat first;
	int fd, dirfd, ok = 1;
	long got;

	/* 200 bytes fit a Linux name and no VFS reply message. */
	memset(long_name, 'L', sizeof long_name - 1);
	if (make_directory(AT_FDCWD, "list") != 0 || chdir("list") != 0 ||
	    !touch("a") || !touch("b") || !touch(long_name) ||
	    make_directory(AT_FDCWD, "d") != 0 ||
	    syscall(SYS_stat, "a", &first) != 0)
		return 0;
	dirfd = open(".", O_RDONLY | O_DIRECTORY);
	if (dirfd != 3)
		return 0;
	/* A 256-byte buffer holds the long name and little else, so one
	 * listing takes several calls. The second pass follows a rewind. */
	for (int pass = 0; pass < 2 && ok; pass++) {
		int count = 0, found = 0;

		while ((got = syscall(SYS_getdents64, dirfd, buffer, 256)) > 0) {
			for (long at = 0; at < got;) {
				struct listed *entry = (void *)(buffer + at);

				if (entry->length < 24 || entry->length % 8 ||
				    at + entry->length > got || entry->inode == 0)
					return 0;
				at += entry->length;
				if (!strcmp(entry->name, ".") || !strcmp(entry->name, ".."))
					continue;
				count++;
				if (!strcmp(entry->name, "a"))
					found |= entry->type == DT_REG && entry->inode == first.st_ino;
				else if (!strcmp(entry->name, "b"))
					found |= (entry->type == DT_REG) << 1;
				else if (!strcmp(entry->name, "d"))
					found |= (entry->type == DT_DIR) << 2;
				else if (!strcmp(entry->name, long_name))
					found |= (entry->type == DT_REG) << 3;
			}
		}
		ok = got == 0 && count == 4 && found == 15 &&
		     lseek(dirfd, 0, SEEK_SET) == 0;
	}
	errno = 0;
	fd = open("a", O_RDONLY);
	ok = ok && fd == 4 &&
	     fails(syscall(SYS_getdents64, dirfd, buffer, 16), EINVAL) &&
	     fails(syscall(SYS_getdents64, dirfd, (void *)8, 256), EFAULT) &&
	     fails(syscall(SYS_getdents64, fd, buffer, 256), ENOTDIR) &&
	     fails(syscall(SYS_getdents64, 42, buffer, 256), EBADF) &&
	     close(fd) == 0 && close(dirfd) == 0;
	return unlink("a") == 0 && unlink("b") == 0 && unlink(long_name) == 0 &&
	       rmdir("d") == 0 && chdir("..") == 0 && rmdir("list") == 0 && ok;
}

static int vectors_ok(int goos)
{
	static struct iovec many[17];
	char first[4], second[8];
	struct iovec out[3] = {{"vec", 3}, {"", 0}, {"tors!", 5}};
	struct iovec in[2] = {{first, sizeof first}, {second, sizeof second}};
	int fd = open("vector", O_RDWR | O_CREAT | O_TRUNC, 0644);

	if (fd != 3 || writev(fd, out, 3) != 8 || lseek(fd, 0, SEEK_SET) != 0 ||
	    readv(fd, in, 2) != 8 || memcmp(first, "vect", 4) ||
	    memcmp(second, "ors!", 4) || readv(fd, in, 2) != 0)
		return 0;
	errno = 0;
	if (!fails(readv(fd, in, -1), EINVAL) ||
	    !fails(writev(fd, (void *)8, 1), EFAULT) ||
	    !fails(readv(42, in, 2), EBADF))
		return 0;
	/* Linux takes 1024 elements; GoOS bounds a vector at 16. */
	if (goos && !fails(writev(fd, many, 17), EINVAL))
		return 0;
	return close(fd) == 0 && unlink("vector") == 0;
}

static int system_ok(void)
{
	struct timespec wall, before, after, nap = {0, 20000000}, bad = {0, 1000000000};
	struct utsname name;
	struct rlimit files;
	long long elapsed;
	pid_t pid = getpid();

	if (pid <= 0 || syscall(SYS_gettid) != pid || getppid() <= 0 ||
	    getuid() != geteuid() || getgid() != getegid() ||
	    setuid(getuid()) != 0 || setgid(getgid()) != 0 ||
	    getgroups(0, NULL) < 0)
		return 0;
	if (uname(&name) != 0 || strcmp(name.sysname, "Linux") ||
	    strcmp(name.machine, "x86_64") || !name.release[0] || !name.nodename[0])
		return 0;
	/* The sleep may run long; it must not end early. */
	if (clock_gettime(CLOCK_REALTIME, &wall) != 0 || wall.tv_sec < 1000000000 ||
	    time(NULL) < wall.tv_sec ||
	    clock_gettime(CLOCK_MONOTONIC, &before) != 0 ||
	    nanosleep(&nap, NULL) != 0 ||
	    clock_gettime(CLOCK_MONOTONIC, &after) != 0)
		return 0;
	elapsed = (after.tv_sec - before.tv_sec) * 1000000000LL + after.tv_nsec - before.tv_nsec;
	if (elapsed < nap.tv_nsec || elapsed > 5000000000LL)
		return 0;
	errno = 0;
	return fails(clock_gettime(99, &wall), EINVAL) &&
	       fails(nanosleep(&bad, NULL), EINVAL) &&
	       getrlimit(RLIMIT_NOFILE, &files) == 0 && files.rlim_cur > 0 &&
	       files.rlim_cur <= files.rlim_max;
}

static int names_ok(int read_only_image)
{
	char back[1];
	int fd, dirfd = open(".", O_RDONLY | O_DIRECTORY);

	if (dirfd != 3 || !fails(open("data", O_RDONLY | O_DIRECTORY), ENOTDIR) ||
	    !fails(open(".", O_WRONLY), EISDIR) ||
	    !fails(read(dirfd, back, 1), EISDIR))
		return 0;
	fd = openat(dirfd, "data", O_RDONLY);
	if (fd != 4 || close(fd) != 0)
		return 0;
	if (rename("data", "moved") != 0 || !fails(open("data", O_RDONLY), ENOENT) ||
	    make_directory(dirfd, "sub") != 0 || rename("sub", "sub2") != 0 ||
	    !fails(unlink("sub2"), EISDIR) || !fails(rmdir("moved"), ENOTDIR) ||
	    unlinkat(dirfd, "sub2", AT_REMOVEDIR) != 0 ||
	    !fails(rmdir(scratch), ENOTEMPTY) ||
	    unlink("moved") != 0 || close(dirfd) != 0)
		return 0;
	return !read_only_image || fails(make_directory(AT_FDCWD, IMAGE "-dir"), EROFS);
}

/* /dev/null and /dev/zero are character devices the kernel serves; /dev/tty
 * has a status, and no open for a process without a controlling terminal. */
static int devices_ok(int goos, int strict)
{
	struct extended_status wide;
	struct stat status;
	char zeros[64], first[3], second[5];
	struct iovec in[2] = {{first, sizeof first}, {second, sizeof second}};
	struct iovec out[2] = {{"dis", 3}, {"carded", 6}};
	struct winsize window;
	int tty, fd = open("/dev/null", O_RDWR);

	if (fd != 3 || write(fd, "gone", 4) != 4 || read(fd, zeros, sizeof zeros) != 0 ||
	    writev(fd, out, 2) != 9 || readv(fd, in, 2) != 0 ||
	    pwrite(fd, "gone", 4, 100) != 4 || pread(fd, zeros, sizeof zeros, 100) != 0 ||
	    lseek(fd, 5, SEEK_SET) != 0 || lseek(fd, 0, SEEK_END) != 0 ||
	    /* The device never looks at the memory it is handed. An
	     * emulator that checks the buffer itself refuses it. */
	    (strict && (write(fd, NULL, 16) != 16 || read(fd, NULL, 16) != 0)) ||
	    syscall(SYS_fstat, fd, &status) != 0 || !S_ISCHR(status.st_mode) ||
	    status.st_rdev != makedev(1, 3) || status.st_size != 0 ||
	    !fails(ioctl(fd, TIOCGWINSZ, &window), ENOTTY) || isatty(fd) ||
	    (fcntl(fd, F_GETFL) & O_ACCMODE) != O_RDWR || dup2(fd, 9) != 9 ||
	    write(9, "x", 1) != 1 || close(9) != 0 || close(fd) != 0)
		return 0;
	errno = 0;
	if (syscall(SYS_stat, "/dev/null", &status) != 0 || !S_ISCHR(status.st_mode) ||
	    status.st_rdev != makedev(1, 3) ||
	    extended(AT_FDCWD, "/dev/null", 0, EXTENDED_BASIC, &wide) != 0 ||
	    !S_ISCHR(wide.mode) || wide.rdev_major != 1 || wide.rdev_minor != 3 ||
	    access("/dev/null", R_OK | W_OK) != 0 || !fails(access("/dev/null", X_OK), EACCES))
		return 0;

	memset(zeros, 0xff, sizeof zeros);
	memset(first, 0xff, sizeof first);
	memset(second, 0xff, sizeof second);
	fd = open("/dev/zero", O_RDONLY);
	if (fd != 3 || read(fd, zeros, sizeof zeros) != sizeof zeros ||
	    readv(fd, in, 2) != 8 || pread(fd, zeros, 8, 1 << 20) != 8 ||
	    !fails(write(fd, "x", 1), EBADF) ||
	    syscall(SYS_fstat, fd, &status) != 0 || !S_ISCHR(status.st_mode) ||
	    status.st_rdev != makedev(1, 5) || close(fd) != 0)
		return 0;
	for (size_t index = 0; index < sizeof zeros; index++)
		if (zeros[index])
			return 0;
	if (first[0] || first[2] || second[0] || second[4])
		return 0;
	fd = open("/dev/zero", O_WRONLY);
	if (fd != 3 || write(fd, "gone", 4) != 4 || !fails(read(fd, zeros, 1), EBADF) ||
	    close(fd) != 0)
		return 0;

	/* A run with a controlling terminal, which no reference run has, may
	 * open it. */
	errno = 0;
	tty = open("/dev/tty", O_RDWR | O_NOCTTY);
	if (tty >= 0 ? (goos || close(tty) != 0) : errno != ENXIO)
		return 0;
	errno = 0;
	return syscall(SYS_stat, "/dev/tty", &status) == 0 && S_ISCHR(status.st_mode) &&
	       status.st_rdev == makedev(5, 0) &&
	       fails(open("/dev/null/below", O_RDONLY), ENOTDIR) &&
	       fails(open("/dev/null", O_RDONLY | O_DIRECTORY), ENOTDIR) &&
	       fails(open("/dev/zero", O_WRONLY | O_CREAT | O_EXCL, 0644), EEXIST) &&
	       fails(chdir("/dev/null"), ENOTDIR) &&
	       fails(make_directory(AT_FDCWD, "/dev/null"), EEXIST) &&
	       /* Removing the names is tried on GoOS only: a Linux run as root
		* would succeed and take the device away. */
	       (!goos || (fails(unlink("/dev/null"), EACCES) &&
			  fails(rmdir("/dev/zero"), ENOTDIR) &&
			  fails(rename("/dev/null", "/tmp/null"), EACCES) &&
			  open("/dev/null", O_WRONLY) == 3 && close(3) == 0));
}

/* struct sigaction as the kernel takes it, which is not the C library's. */
struct kernel_action {
	void (*handler)(int);
	unsigned long flags;
	void (*restorer)(void);
	unsigned long mask;
};

#define SET_SIZE 8
#define ACTION_RESTORER 0x04000000UL
#define ACTION_RESTART 0x10000000UL
#define BIT(signal) (1UL << ((signal) - 1))

static void never_called(int signal)
{
	(void)signal;
}

static void never_returned(void)
{
}

/* The kernel records dispositions and the blocked mask and reports them
 * back. No check here raises a signal; the signal mode delivers them. An
 * emulator that keeps the signal table itself leaves SIGKILL and SIGSTOP in
 * a handler's mask, so only a strict run requires them gone. */
static int signals_ok(int strict)
{
	struct kernel_action fresh = {never_called, ACTION_RESTORER | ACTION_RESTART, never_returned,
				      BIT(12) | BIT(9) | BIT(19)};
	struct kernel_action old, back;
	unsigned long set, before;

	memset(&old, 0xff, sizeof old);
	if (syscall(SYS_rt_sigaction, 10, &fresh, &old, SET_SIZE) != 0 || old.handler != NULL ||
	    syscall(SYS_rt_sigaction, 10, NULL, &back, SET_SIZE) != 0 ||
	    back.handler != never_called || back.flags != fresh.flags ||
	    back.restorer != never_returned ||
	    /* SIGKILL and SIGSTOP leave a handler's mask. */
	    (back.mask & BIT(12)) == 0 || (strict && back.mask != BIT(12)) ||
	    syscall(SYS_rt_sigaction, 12, NULL, &back, SET_SIZE) != 0 || back.handler != NULL ||
	    syscall(SYS_rt_sigaction, 64, &fresh, NULL, SET_SIZE) != 0 ||
	    syscall(SYS_rt_sigaction, 9, NULL, &back, SET_SIZE) != 0 || back.handler != NULL)
		return 0;
	errno = 0;
	if (!fails(syscall(SYS_rt_sigaction, 9, &fresh, NULL, SET_SIZE), EINVAL) ||
	    !fails(syscall(SYS_rt_sigaction, 19, &fresh, NULL, SET_SIZE), EINVAL) ||
	    !fails(syscall(SYS_rt_sigaction, 0, NULL, &back, SET_SIZE), EINVAL) ||
	    !fails(syscall(SYS_rt_sigaction, 65, NULL, &back, SET_SIZE), EINVAL) ||
	    !fails(syscall(SYS_rt_sigaction, 10, NULL, &back, 4), EINVAL) ||
	    !fails(syscall(SYS_rt_sigaction, 10, (void *)8, NULL, SET_SIZE), EFAULT) ||
	    !fails(syscall(SYS_rt_sigaction, 10, NULL, (void *)8, SET_SIZE), EFAULT))
		return 0;
	/* Put the default back, so the process leaves as it arrived. */
	memset(&fresh, 0, sizeof fresh);
	if (syscall(SYS_rt_sigaction, 10, &fresh, NULL, SET_SIZE) != 0 ||
	    syscall(SYS_rt_sigaction, 64, &fresh, NULL, SET_SIZE) != 0)
		return 0;

	if (syscall(SYS_rt_sigprocmask, SIG_SETMASK, NULL, &before, SET_SIZE) != 0)
		return 0;
	set = BIT(10) | BIT(12) | BIT(9) | BIT(19);
	if (syscall(SYS_rt_sigprocmask, SIG_BLOCK, &set, NULL, SET_SIZE) != 0 ||
	    /* A call with no new set reports the mask whatever "how" says. */
	    syscall(SYS_rt_sigprocmask, 99, NULL, &back.mask, SET_SIZE) != 0 ||
	    back.mask != (before | BIT(10) | BIT(12)))
		return 0;
	set = BIT(12);
	if (syscall(SYS_rt_sigprocmask, SIG_UNBLOCK, &set, &back.mask, SET_SIZE) != 0 ||
	    back.mask != (before | BIT(10) | BIT(12)) ||
	    syscall(SYS_rt_sigprocmask, SIG_SETMASK, &before, &back.mask, SET_SIZE) != 0 ||
	    back.mask != (before | BIT(10)) ||
	    syscall(SYS_rt_sigprocmask, SIG_SETMASK, NULL, &back.mask, SET_SIZE) != 0 ||
	    back.mask != before)
		return 0;
	errno = 0;
	return fails(syscall(SYS_rt_sigprocmask, 99, &set, NULL, SET_SIZE), EINVAL) &&
	       fails(syscall(SYS_rt_sigprocmask, SIG_BLOCK, &set, NULL, 4), EINVAL) &&
	       fails(syscall(SYS_rt_sigprocmask, SIG_BLOCK, (void *)8, NULL, SET_SIZE), EFAULT) &&
	       fails(syscall(SYS_rt_sigprocmask, SIG_BLOCK, NULL, (void *)8, SET_SIZE), EFAULT);
}

/* 1 when the kernel hands out random bytes in both places a program looks,
 * 0 when it has none in either, -1 when the two disagree. */
static int random_source(void)
{
	unsigned char byte;
	long got;

	errno = 0;
	got = getrandom(&byte, 1, 0);
	if (got == 1 && getauxval(AT_RANDOM) != 0)
		return 1;
	if (got == -1 && errno == ENOSYS && getauxval(AT_RANDOM) == 0)
		return 0;
	return -1;
}

/* Linux and GoOS always have a source: static glibc takes its stack canary
 * from AT_RANDOM before main and faults when the entry is missing. */
static int random_ok(int goos)
{
	static unsigned char large[300];
	unsigned char first[32], second[32], *start;
	int source = random_source(), zero = 0;
	long got;

	errno = 0;
	if (source != 1)
		return 0;
	memset(first, 0, sizeof first);
	memset(second, 0, sizeof second);
	if (getrandom(first, sizeof first, 0) != sizeof first ||
	    getrandom(second, sizeof second, GRND_NONBLOCK) != sizeof second ||
	    !memcmp(first, second, sizeof first) || getrandom(first, 0, 0) != 0)
		return 0;
	for (size_t index = 0; index < sizeof first; index++)
		zero += first[index] == 0;
	/* Sixteen bytes at AT_RANDOM, and not the bytes a call returns. */
	start = (unsigned char *)getauxval(AT_RANDOM);
	if (zero == sizeof first || !memcmp(start, first, 16) || !memcmp(start, second, 16))
		return 0;
	/* Up to 256 bytes come whole; GoOS returns no more than that. */
	got = getrandom(large, sizeof large, 0);
	if (got != (goos ? 256 : (long)sizeof large))
		return 0;
	errno = 0;
	return fails(getrandom(first, sizeof first, 0x100), EINVAL) &&
	       fails(getrandom(first, sizeof first, GRND_RANDOM | 4), EINVAL) &&
	       fails(getrandom((void *)8, sizeof first, 0), EFAULT);
}

static int report(const char *what, int ok)
{
	printf("%s %s\n", what, ok ? "ok" : "failed");
	return !ok;
}

/* Returns how many of its checks failed. */
/* dup, dup2, dup3, fcntl, and ioctl. Duplicates share one file position;
 * the close-on-exec flag belongs to each descriptor. */
static int descriptors_ok(void)
{
	struct winsize window;
	int flags, other, fd = open("dup", O_RDWR | O_CREAT | O_TRUNC, 0644);

	if (fd != 3 || dup(fd) != 4 || write(fd, "abc", 3) != 3 ||
	    !position_ok(4, 0, SEEK_CUR, 3))
		return 0;
	if (dup2(fd, 10) != 10 || dup2(fd, fd) != fd ||
	    dup3(fd, 11, O_CLOEXEC) != 11 ||
	    fcntl(11, F_GETFD) != FD_CLOEXEC || fcntl(10, F_GETFD) != 0 ||
	    fcntl(10, F_SETFD, FD_CLOEXEC) != 0 || fcntl(10, F_GETFD) != FD_CLOEXEC ||
	    fcntl(fd, F_GETFD) != 0)
		return 0;
	if (fcntl(fd, F_DUPFD, 20) != 20 || fcntl(fd, F_DUPFD_CLOEXEC, 20) != 21 ||
	    fcntl(21, F_GETFD) != FD_CLOEXEC || fcntl(20, F_GETFD) != 0)
		return 0;
	flags = fcntl(fd, F_GETFL);
	if (flags < 0 || (flags & O_ACCMODE) != O_RDWR || (flags & O_APPEND) ||
	    fcntl(fd, F_SETFL, flags | O_APPEND) != 0 ||
	    !(fcntl(10, F_GETFL) & O_APPEND) ||
	    lseek(fd, 0, SEEK_SET) != 0 || write(fd, "de", 2) != 2 ||
	    !position_ok(20, 0, SEEK_CUR, 5) ||
	    fcntl(fd, F_SETFL, flags) != 0 || (fcntl(fd, F_GETFL) & O_APPEND))
		return 0;
	/* dup2 onto an open file closes that file first. */
	other = open("other", O_RDWR | O_CREAT, 0644);
	if (other != 5 || dup2(fd, other) != other ||
	    !position_ok(other, 0, SEEK_CUR, 5))
		return 0;
	errno = 0;
	if (!fails(dup(42), EBADF) || !fails(dup2(42, 6), EBADF) ||
	    !fails(dup2(fd, 1 << 30), EBADF) || !fails(dup3(fd, fd, 0), EINVAL) ||
	    !fails(syscall(SYS_dup3, fd, 12, 1), EINVAL) ||
	    !fails(fcntl(42, F_GETFD), EBADF) ||
	    !fails(fcntl(fd, F_DUPFD, 1 << 30), EINVAL) ||
	    !fails(ioctl(fd, TIOCGWINSZ, &window), ENOTTY) ||
	    !fails(ioctl(42, TIOCGWINSZ, &window), EBADF) ||
	    isatty(fd))
		return 0;
	for (int index = 3; index <= 21; index++)
		close(index);
	errno = 0;
	return fails(close(fd), EBADF) && unlink("dup") == 0 && unlink("other") == 0;
}

/* Every instance works in a directory of its own, named by its PID. */
static int enter_scratch(void)
{
	snprintf(scratch, sizeof scratch, "/tmp/abi-%d", (int)getpid());
	return make_directory(AT_FDCWD, scratch) == 0 && chdir(scratch) == 0;
}

static int leave_scratch(void)
{
	return chdir("/system/linux") == 0 && rmdir(scratch) == 0;
}

/* Returns how many of its checks failed. */
static int scratch_checks(const char *self)
{
	int read_only_image = on_goos();
	char cwd[64];
	int failed;

	errno = 0;
	if (!enter_scratch() ||
	    !fails(make_directory(AT_FDCWD, scratch), EEXIST) ||
	    !fails(chdir(self), ENOTDIR) ||
	    !getcwd(cwd, sizeof cwd) || strcmp(cwd, scratch) ||
	    getcwd(cwd, 4) || !fails(-1, ERANGE))
		return report("scratch directory", 0);
	failed = report("files", data_ok());
	failed += report("status", status_ok(self, read_only_image));
	failed += report("statx", extended_status_ok(strict_kernel()));
	failed += report("positions", positions_ok());
	failed += report("stdio", stdio_ok());
	failed += report("listing", listing_ok());
	failed += report("vectors", vectors_ok(read_only_image));
	failed += report("descriptors", descriptors_ok());
	failed += report("names", names_ok(read_only_image));
	if (!leave_scratch())
		failed += report("scratch cleanup", 0);
	return failed;
}

/* Pointers no process can use: null, a noncanonical address, and an address
 * in the kernel half. */
static void *const unusable[] = {
	NULL, (void *)0x8000000000000000ULL, (void *)0xffff800000000000ULL,
};

/* A reference run under user-mode emulation leaves out nanosleep: the
 * emulator sleeps on whatever the bad pointer holds instead of refusing it. */
static int bad_pointers(int fd, int dirfd, int goos)
{
	struct extended_status wide;
	struct stat status;
	struct rlimit limit;
	int random = random_source();

	for (int index = 0; index < 3; index++) {
		void *bad = unusable[index];

		errno = 0;
		if (!fails(read(fd, bad, 16), EFAULT) ||
		    !fails(write(fd, bad, 16), EFAULT) ||
		    !fails(readv(fd, bad, 1), EFAULT) ||
		    !fails(writev(fd, bad, 1), EFAULT) ||
		    !fails(open(bad, O_RDONLY), EFAULT) ||
		    !fails(syscall(SYS_stat, bad, &status), EFAULT) ||
		    !fails(syscall(SYS_stat, "/", bad), EFAULT) ||
		    !fails(syscall(SYS_fstat, fd, bad), EFAULT) ||
		    !fails(syscall(SYS_getcwd, bad, 64), EFAULT) ||
		    !fails(syscall(SYS_uname, bad), EFAULT) ||
		    !fails(syscall(SYS_clock_gettime, CLOCK_MONOTONIC, bad), EFAULT) ||
		    (goos && !fails(syscall(SYS_nanosleep, bad, 0), EFAULT)) ||
		    !fails(syscall(SYS_arch_prctl, 0x1003, bad), EFAULT) ||
		    lseek(dirfd, 0, SEEK_SET) != 0 ||
		    !fails(syscall(SYS_getdents64, dirfd, bad, 256), EFAULT) ||
		    !fails(chdir(bad), EFAULT) || !fails(unlink(bad), EFAULT) ||
		    !fails(rename(bad, "x"), EFAULT) || !fails(rename("data", bad), EFAULT) ||
		    !fails(make_directory(AT_FDCWD, bad), EFAULT) ||
		    !fails(pread(fd, bad, 16, 0), EFAULT) ||
		    !fails(pwrite(fd, bad, 16, 0), EFAULT) ||
		    !fails(extended(AT_FDCWD, bad, 0, EXTENDED_BASIC, &wide), EFAULT) ||
		    !fails(extended(AT_FDCWD, "/", 0, EXTENDED_BASIC, bad), EFAULT) ||
		    (random == 1 && !fails(getrandom(bad, 16, 0), EFAULT)))
			return 0;
		/* A null action or set means "leave it", so only the other two
		 * pointers are bad for the signal calls. */
		if (bad && (!fails(syscall(SYS_rt_sigaction, 10, bad, NULL, SET_SIZE), EFAULT) ||
			    !fails(syscall(SYS_rt_sigaction, 10, NULL, bad, SET_SIZE), EFAULT) ||
			    !fails(syscall(SYS_rt_sigprocmask, SIG_BLOCK, bad, NULL, SET_SIZE), EFAULT) ||
			    !fails(syscall(SYS_rt_sigprocmask, SIG_BLOCK, NULL, bad, SET_SIZE), EFAULT)))
			return 0;
		/* A null old-limit pointer means "do not report", so only the
		 * other two are bad here. */
		if (bad && !fails(syscall(SYS_prlimit64, 0, RLIMIT_NOFILE, 0, bad), EFAULT))
			return 0;
	}
	(void)limit;
	return 1;
}

/* A buffer that runs off the end of a mapping. Linux moves the bytes before
 * the fault and reports them; an emulator that checks the whole range first
 * reports EFAULT, and a run that is not strict accepts that too. */
static int short_or_fault(long result, long moved, int strict)
{
	int fault = result == -1 && errno == EFAULT;

	errno = 0;
	return result == moved || (!strict && fault);
}

static int mapping_edges(int fd, int strict)
{
	struct stat *straddling;
	char *region = mmap(NULL, 8192, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	char *edge;

	if (region == MAP_FAILED || munmap(region + 4096, 4096) != 0)
		return 0;
	edge = region + 4096 - 8;
	memset(edge, 'e', 8);
	errno = 0;
	if (lseek(fd, 0, SEEK_SET) != 0 || !short_or_fault(write(fd, edge, 16), 8, strict) ||
	    lseek(fd, 0, SEEK_SET) != 0 || !short_or_fault(read(fd, edge, 16), 8, strict) ||
	    !short_or_fault(pwrite(fd, edge, 16, 0), 8, strict) ||
	    !short_or_fault(pread(fd, edge, 16, 0), 8, strict))
		return 0;
	/* The longest possible lengths. A length that carries the address past
	 * the user half is refused before a byte moves. A run under user-mode
	 * emulation skips these: the emulator walks the whole range before it
	 * answers. */
	if (strict && (!fails(write(fd, edge, (size_t)-1), EFAULT) ||
		       !fails(read(fd, edge, (size_t)-1), EFAULT) ||
		       !fails(pwrite(fd, edge, (size_t)-1, 0), EFAULT) ||
		       mmap(NULL, (size_t)-1, PROT_READ, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0) != MAP_FAILED ||
		       !fails(-1, ENOMEM)))
		return 0;
	/* A path with no terminator before the mapping ends, and a stat
	 * buffer that straddles the end. */
	memset(edge, 'p', 8);
	straddling = (struct stat *)(region + 4096 - 64);
	if (!fails(open(edge, O_RDONLY), EFAULT) ||
	    !fails(syscall(SYS_stat, "/", straddling), EFAULT))
		return 0;
	/* Lengths and addresses no mapping call can take. */
	if (mmap(NULL, 0, PROT_READ, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0) != MAP_FAILED ||
	    !fails(-1, EINVAL) ||
	    !fails(munmap(region, 0), EINVAL) ||
	    !fails(munmap(region + 1, 4096), EINVAL) ||
	    !fails(syscall(SYS_mprotect, region + 1, 4096, PROT_READ), EINVAL) ||
	    !fails(mprotect(region, 8192, PROT_READ), ENOMEM))
		return 0;
	return munmap(region, 4096) == 0;
}

/* Transfers longer than one call moves. The program loops, as POSIX asks. */
static int long_transfers(int fd)
{
	static unsigned char sent[100000], received[100000];
	size_t done = 0;
	long moved;

	for (size_t index = 0; index < sizeof sent; index++)
		sent[index] = (unsigned char)(index * 31 + 7);
	if (lseek(fd, 0, SEEK_SET) != 0)
		return 0;
	while (done < sizeof sent && (moved = write(fd, sent + done, sizeof sent - done)) > 0)
		done += moved;
	if (done != sizeof sent || lseek(fd, 0, SEEK_SET) != 0)
		return 0;
	done = 0;
	while (done < sizeof received && (moved = read(fd, received + done, sizeof received - done)) > 0)
		done += moved;
	return done == sizeof received && !memcmp(sent, received, sizeof sent) &&
	       read(fd, received, 1) == 0;
}

static int stale_descriptors(int fd, int dirfd)
{
	struct extended_status wide;
	struct winsize window;
	struct stat status;
	char byte;

	if (close(fd) != 0 || close(dirfd) != 0)
		return 0;
	errno = 0;
	return fails(read(fd, &byte, 1), EBADF) && fails(write(fd, &byte, 1), EBADF) &&
	       fails(lseek(fd, 0, SEEK_SET), EBADF) &&
	       fails(syscall(SYS_fstat, fd, &status), EBADF) &&
	       fails(syscall(SYS_getdents64, dirfd, &status, sizeof status), EBADF) &&
	       fails(dup(fd), EBADF) && fails(fcntl(fd, F_GETFL), EBADF) &&
	       fails(ioctl(fd, TIOCGWINSZ, &window), EBADF) &&
	       fails(syscall(SYS_newfstatat, dirfd, "data", &status, 0), EBADF) &&
	       fails(pread(fd, &byte, 1, 0), EBADF) && fails(pwrite(fd, &byte, 1, 0), EBADF) &&
	       fails(extended(fd, "", AT_EMPTY_PATH, EXTENDED_BASIC, &wide), EBADF) &&
	       fails(openat(dirfd, "data", O_RDONLY), EBADF);
}

/* GoOS only. Anonymous memory is never executable: a mapping cannot ask for
 * it and an existing mapping cannot gain it. Linux allows both. */
static int no_executable_memory(void)
{
	char *page = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	int ok = page != MAP_FAILED &&
		 mmap(NULL, 4096, PROT_READ | PROT_EXEC, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0) == MAP_FAILED &&
		 mmap(NULL, 4096, PROT_READ | PROT_WRITE | PROT_EXEC, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0) == MAP_FAILED &&
		 mprotect(page, 4096, PROT_READ | PROT_EXEC) == -1 &&
		 mprotect(page, 4096, PROT_READ | PROT_WRITE | PROT_EXEC) == -1;

	errno = 0;
	return ok && munmap(page, 4096) == 0;
}

/* O_NONBLOCK changes nothing for a regular file, as on Linux, and F_GETFL
 * reports it. */
static int nonblocking_open(void)
{
	int fd = open("data", O_RDONLY | O_NONBLOCK);
	int flags = fd < 0 ? -1 : fcntl(fd, F_GETFL);
	char byte;
	int ok = fd >= 0 && flags >= 0 && (flags & O_NONBLOCK) && read(fd, &byte, 1) == 1 &&
		 fcntl(fd, F_SETFL, 0) == 0 && !(fcntl(fd, F_GETFL) & O_NONBLOCK);

	if (fd >= 0)
		close(fd);
	return ok;
}

static int hostile(void)
{
	int goos = on_goos(), strict = strict_kernel();
	int failed, fd, dirfd;
	char byte;

	if (!enter_scratch())
		return report("scratch directory", 0);
	fd = open("data", O_RDWR | O_CREAT | O_TRUNC, 0644);
	dirfd = open(".", O_RDONLY | O_DIRECTORY);
	if (fd != 3 || dirfd != 4 || write(fd, "0123456789abcdef", 16) != 16 ||
	    lseek(fd, 0, SEEK_SET) != 0)
		return report("scratch file", 0);
	failed = report("bad pointers", bad_pointers(fd, dirfd, strict));
	failed += report("mapping edges", mapping_edges(fd, strict));
	failed += report("long transfers", long_transfers(fd));
	errno = 0;
	failed += report("arguments",
			 read(fd, NULL, 0) == 0 && write(fd, NULL, 0) == 0 &&
			 fails(lseek(fd, 0, 99), EINVAL) &&
			 fails(syscall(9999), ENOSYS) &&
			 fails(syscall(SYS_getdents64, fd, &byte, 256), ENOTDIR) &&
			 fails(lseek(1, 0, SEEK_SET), ESPIPE) &&
			 (!goos || fails(open("data", O_RDONLY | O_DIRECT), EOPNOTSUPP)) &&
			 nonblocking_open() &&
			 (!goos || no_executable_memory()));
	failed += report("stale descriptors", stale_descriptors(fd, dirfd));
	if (unlink("data") != 0 || !leave_scratch())
		failed += report("scratch cleanup", 0);
	if (!failed)
		puts("hostile ok");
	return failed;
}

/* GoOS only. The descriptor table holds 64 and the page quota a fixed
 * number of pages. Both must refuse the call that overruns them, and both
 * must work again once something is released. */
static int exhaust(void)
{
	enum { chunk = 1 << 20, most = 128 };
	static char *mapped[most];
	int count = 0, chunks = 0, fd;
	char *small;

	errno = 0;
	while ((fd = open(IMAGE, O_RDONLY)) >= 0)
		count++;
	if (errno != EMFILE || count != 61 || !fails(dup(0), EMFILE) ||
	    !fails(open("/tmp", O_RDONLY | O_DIRECTORY), EMFILE))
		return report("descriptor table", 0);
	for (fd = 3; fd < 64; fd++)
		if (close(fd) != 0)
			return report("descriptor table", 0);
	if (open(IMAGE, O_RDONLY) != 3 || close(3) != 0)
		return report("descriptor table", 0);
	puts("descriptor table ok");

	/* Every mapped page is touched, so the quota is what runs out. */
	while (chunks < most) {
		char *memory = mmap(NULL, chunk, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);

		if (memory == MAP_FAILED)
			break;
		memset(memory, 0x77, chunk);
		mapped[chunks++] = memory;
	}
	if (chunks == most || chunks < 8 || errno != ENOMEM || malloc(chunk) != NULL)
		return report("page quota", 0);
	for (int index = 0; index < chunks; index++)
		if (munmap(mapped[index], chunk) != 0)
			return report("page quota", 0);
	small = malloc(chunk);
	if (!small)
		return report("page quota", 0);
	memset(small, 1, chunk);
	free(small);
	printf("page quota ok: %d MiB mapped before ENOMEM\n", chunks);
	puts("exhaust ok");
	return 0;
}

/* GoOS only. A native GoOS program enters the kernel with int $0x80. A
 * Linux process that tries it must get the same refusal for every call
 * number and must not have run any of them: several of those numbers would
 * end the process or write to the console if the gate were open. */
static int isolation(void)
{
	long refusal = 0;
	char byte;

	for (long number = 0; number < 128; number++) {
		long result;

		__asm__ volatile("int $0x80"
				 : "=a"(result)
				 : "a"(number), "D"(1L), "S"(0L), "d"(0L)
				 : "rcx", "r11", "memory");
		if (number == 0)
			refusal = result;
		if (result != refusal || result == 0)
			return report("native gate", 0);
	}
	puts("native gate ok");
	/* A native handle is a generation and an index. No such number is a
	 * Linux descriptor. */
	errno = 0;
	if (!fails(syscall(SYS_read, 0x100000001UL, &byte, 1), EBADF) ||
	    !fails(syscall(SYS_write, 0x100000002UL, &byte, 1), EBADF) ||
	    !fails(syscall(SYS_close, 0x100000003UL), EBADF))
		return report("handle guesses", 0);
	puts("handle guesses ok");
	puts("isolation ok");
	return 0;
}

/* GoOS only. A Linux process holds no port authority, so the instruction
 * faults and the kernel ends the process before the line below. */
static int port_io(void)
{
	puts("port-io: writing to an I/O port");
	fflush(stdout);
	__asm__ volatile("outb %%al, $0x80" : : "a"(0));
	puts("port-io survived");
	return 1;
}

/* Two instances run at once in one pipeline. Each keeps a working
 * directory, an open file, a TLS value, and heap data of its own, sleeps so
 * that the other runs in between, and checks that nothing changed. */
static int pair(const char *name, int drain)
{
	char cwd[64], back[16], line[128];
	size_t length = strlen(name);
	char *heap = malloc(4096);
	int fd, ok;

	if (!heap || length == 0 || length >= sizeof back || !enter_scratch())
		return report("pair setup", 0);
	fd = open("mine", O_RDWR | O_CREAT | O_TRUNC, 0644);
	tls_data = (int)getpid();
	memset(heap, name[0], 4096);
	ok = fd == 3 && write(fd, name, length) == (ssize_t)length;
	for (int round = 0; round < 5 && ok; round++) {
		struct timespec nap = {0, 30000000};

		nanosleep(&nap, NULL);
		ok = getcwd(cwd, sizeof cwd) && !strcmp(cwd, scratch) &&
		     lseek(fd, 0, SEEK_SET) == 0 &&
		     read(fd, back, sizeof back) == (ssize_t)length &&
		     !memcmp(back, name, length) &&
		     tls_data == (int)getpid() && heap[4095] == name[0];
	}
	ok = ok && close(fd) == 0 && unlink("mine") == 0 && leave_scratch();
	printf("pair %s %s pid %d\n", name, ok ? "ok" : "failed", (int)getpid());
	fflush(stdout);
	/* The last stage passes on what the stage before it printed. */
	while (drain && fgets(line, sizeof line, stdin))
		fputs(line, stdout);
	return !ok;
}

/* Each call keeps a page of stack and hands its address on, so no compiler
 * can turn the recursion into a loop. The stack reservation is 8 MiB with a
 * guard below it; the touch that reaches the guard ends the process. */
__attribute__((noinline)) static int descend(volatile char *above)
{
	volatile char frame[4096];

	frame[0] = above ? above[0] + 1 : 1;
	return descend(frame) + frame[0];
}

static int stack_overflow(void)
{
	puts("stack-overflow: recursing");
	fflush(stdout);
	printf("stack-overflow survived: %d\n", descend(NULL));
	return 1;
}

static long long monotonic(void)
{
	struct timespec now;

	clock_gettime(CLOCK_MONOTONIC, &now);
	return now.tv_sec * 1000000000LL + now.tv_nsec;
}

/* Times four calls and prints nanoseconds per call on standard error, so
 * standard output is free to be the stream under test. */
static int bench(void)
{
	enum { calls = 20000, writes = 2000, reads = 2000 };
	static char block[4096];
	struct timespec sample;
	long long began;
	int fd;

	began = monotonic();
	for (int index = 0; index < calls; index++)
		syscall(SYS_getpid);
	fprintf(stderr, "linux-bench getpid %lld ns/call\n", (monotonic() - began) / calls);

	began = monotonic();
	for (int index = 0; index < calls; index++)
		syscall(SYS_clock_gettime, CLOCK_MONOTONIC, &sample);
	fprintf(stderr, "linux-bench clock_gettime %lld ns/call\n", (monotonic() - began) / calls);

	memset(block, '.', 63);
	block[63] = '\n';
	began = monotonic();
	for (int index = 0; index < writes; index++)
		if (write(1, block, 64) != 64)
			return 1;
	fprintf(stderr, "linux-bench stdout-write-64 %lld ns/call\n", (monotonic() - began) / writes);

	fd = open(IMAGE, O_RDONLY);
	if (fd < 0)
		return 1;
	began = monotonic();
	for (int index = 0; index < reads; index++)
		if (read(fd, block, sizeof block) <= 0 && lseek(fd, 0, SEEK_SET) != 0)
			return 1;
	fprintf(stderr, "linux-bench vfs-read-4k %lld ns/call\n", (monotonic() - began) / reads);
	return close(fd) != 0;
}

static int session_loss(void)
{
	char byte;
	int fd;

	while (read(0, &byte, 1) > 0)
		;
	errno = 0;
	fd = open(IMAGE, O_RDONLY);
	if (fd == -1 && errno == EIO)
		puts("session lost: EIO");
	else
		printf("session kept: fd=%d errno=%d\n", fd, errno);
	return 0;
}

static int session_timeout(void)
{
	int fd;

	errno = 0;
	fd = open("/tmp/.validation-vfs-stall", O_RDONLY);
	if (fd != -1 || errno != ETIMEDOUT) {
		printf("stalled open: fd=%d errno=%d\n", fd, errno);
		return 1;
	}
	/* The late reply arrives with this request and must be discarded. */
	fd = open(IMAGE, O_RDONLY);
	if (fd != 3 || close(fd) != 0) {
		printf("open after timeout: fd=%d errno=%d\n", fd, errno);
		return 1;
	}
	puts("session timeout: ETIMEDOUT, then ok");
	return 0;
}

static void exec_handler(int signal)
{
	(void)signal;
}

static int after_exec(int argc, char **argv)
{
	struct sigaction action;
	sigset_t mask;
	char cwd[32];
	char byte;
	int kept, closed;

	if (argc != 5 || strcmp(argv[1], "after-exec"))
		return 1;
	kept = atoi(argv[3]);
	closed = atoi(argv[4]);
	memset(&action, 0, sizeof action);
	if (getpid() != atoi(argv[2]) || !getcwd(cwd, sizeof cwd) || strcmp(cwd, "/tmp") ||
		!getenv("EXEC_VALUE") || strcmp(getenv("EXEC_VALUE"), "raw") ||
		read(kept, &byte, 1) != 1 || byte != 'E' ||
		fcntl(closed, F_GETFD) != -1 || errno != EBADF ||
		sigaction(SIGUSR1, NULL, &action) || action.sa_handler != SIG_DFL)
		return 1;
	if (sigaction(SIGUSR2, NULL, &action) || action.sa_handler != SIG_IGN ||
		sigprocmask(SIG_SETMASK, NULL, &mask) || !sigismember(&mask, SIGTERM))
		return 1;
	/* The new image starts from the initial x87 and SSE state. */
	{
		unsigned int mxcsr;
		unsigned short fcw;

		__asm__ volatile("stmxcsr %0\n\tfnstcw %1" : "=m"(mxcsr), "=m"(fcw));
		if (mxcsr != 0x1f80 || fcw != 0x037f)
			return 1;
	}
	printf("exec ok pid %d\n", getpid());
	return 0;
}

static int exec_test(void)
{
	struct sigaction action;
	sigset_t mask;
	char pid[24], kept_text[24], closed_text[24];
	char byte;
	int kept, closed;
	char *arguments[] = {IMAGE, "after-exec", pid, kept_text, closed_text, NULL};
	char *environment[] = {"EXEC_VALUE=raw", "without-equals", NULL};

	kept = open(IMAGE, O_RDONLY);
	closed = open(IMAGE, O_RDONLY);
	if (kept < 0 || closed < 0 || read(kept, &byte, 1) != 1 || byte != 0x7f ||
		fcntl(closed, F_SETFD, FD_CLOEXEC) != 0 || chdir("/tmp") != 0)
		return 1;
	memset(&action, 0, sizeof action);
	action.sa_handler = exec_handler;
	sigemptyset(&action.sa_mask);
	if (sigaction(SIGUSR1, &action, NULL) != 0)
		return 1;
	action.sa_handler = SIG_IGN;
	if (sigaction(SIGUSR2, &action, NULL) != 0)
		return 1;
	sigemptyset(&mask);
	sigaddset(&mask, SIGTERM);
	if (sigprocmask(SIG_BLOCK, &mask, NULL) != 0)
		return 1;
	snprintf(pid, sizeof pid, "%d", getpid());
	snprintf(kept_text, sizeof kept_text, "%d", kept);
	snprintf(closed_text, sizeof closed_text, "%d", closed);
	/* A changed MXCSR and x87 control word must not reach the new image. */
	{
		unsigned int mxcsr = 0xbf80;
		unsigned short fcw = 0x007f;

		__asm__ volatile("ldmxcsr %0\n\tfldcw %1" : : "m"(mxcsr), "m"(fcw));
	}
	execve(IMAGE, arguments, environment);
	printf("exec failed: errno=%d\n", errno);
	return 1;
}

static int fork_global = 1;

/* The child changes a global, a heap block, and a stack variable, then reads
 * one byte through an inherited descriptor. The parent must still see its own
 * values and must find the shared file position moved. Nothing reaps the
 * child yet, so the parent watches the position instead of waiting. */
static int fork_test(void)
{
	struct timespec nap = {0, 10 * 1000 * 1000};
	volatile int local = 1;
	char *heap = malloc(16);
	pid_t parent = getpid(), child;
	char byte;
	int fd, tries;

	fd = open(IMAGE, O_RDONLY);
	if (fd < 0 || !heap)
		return 1;
	strcpy(heap, "parent");
	fflush(stdout);
	child = fork();
	if (child < 0) {
		printf("fork failed: errno=%d\n", errno);
		return 1;
	}
	if (child == 0) {
		fork_global = 2;
		local = 2;
		strcpy(heap, "child");
		if (getpid() == parent || getppid() != parent || fork_global != 2 || local != 2)
			_exit(1);
		printf("fork child ok\n");
		fflush(stdout);
		if (read(fd, &byte, 1) != 1 || byte != 0x7f)
			_exit(1);
		_exit(0);
	}
	for (tries = 0; tries < 500 && lseek(fd, 0, SEEK_CUR) != 1; tries++)
		nanosleep(&nap, NULL);
	if (child == parent || fork_global != 1 || local != 1 || strcmp(heap, "parent") ||
	    read(fd, &byte, 1) != 1 || byte != 'E') {
		printf("fork parent failed\n");
		return 1;
	}
	printf("fork parent ok\n");
	return 0;
}

/* A vfork child reads one byte through the shared descriptor and exits. The
 * parent resumes only after that exit, so the position has already moved
 * when vfork returns. A clone with SIGCHLD alone is a fork; the parent
 * watches the position as fork_test does. GoOS refuses a thread with no
 * stack of its own; Linux would create one on the caller's stack, so that
 * check runs on GoOS only. */
static int vfork_test(void)
{
	struct timespec nap = {0, 10 * 1000 * 1000};
	char byte;
	long child;
	int fd, tries;

	fd = open(IMAGE, O_RDONLY);
	if (fd < 0)
		return 1;
	fflush(stdout);
	child = vfork();
	if (child == 0) {
		if (read(fd, &byte, 1) != 1)
			_exit(1);
		_exit(0);
	}
	if (child < 0 || lseek(fd, 0, SEEK_CUR) != 1) {
		printf("vfork failed: child=%ld errno=%d\n", child, errno);
		return 1;
	}
	printf("vfork ok\n");
	fflush(stdout);
	child = syscall(SYS_clone, SIGCHLD, 0, 0, 0, 0);
	if (child == 0) {
		if (getppid() == getpid() || read(fd, &byte, 1) != 1)
			_exit(1);
		_exit(0);
	}
	for (tries = 0; child > 0 && tries < 500 && lseek(fd, 0, SEEK_CUR) != 2; tries++)
		nanosleep(&nap, NULL);
	if (child < 0 || lseek(fd, 0, SEEK_CUR) != 2) {
		printf("clone failed: child=%ld errno=%d\n", child, errno);
		return 1;
	}
	if (on_goos() && !fails(syscall(SYS_clone, CLONE_VM | CLONE_FS | CLONE_FILES | CLONE_SIGHAND | CLONE_THREAD, 0, 0, 0, 0), EINVAL)) {
		printf("clone accepted a thread\n");
		return 1;
	}
	printf("clone ok\n");
	return 0;
}

/* The child forks a grandchild and exits at once. The grandchild waits until
 * PID 1 is its parent, says so, and moves the shared file position. The
 * probe watches that position, as fork_test does. */
static int orphan_test(void)
{
	struct timespec nap = {0, 10 * 1000 * 1000};
	char byte;
	pid_t child;
	int fd, tries;

	fd = open(IMAGE, O_RDONLY);
	if (fd < 0)
		return 1;
	fflush(stdout);
	child = fork();
	if (child == 0) {
		pid_t middle = getpid();

		if (fork() != 0)
			_exit(0);
		for (tries = 0; tries < 500 && getppid() == middle; tries++)
			nanosleep(&nap, NULL);
		if (getppid() != 1)
			printf("orphan failed: ppid=%d\n", getppid());
		else
			printf("orphan ok\n");
		fflush(stdout);
		if (read(fd, &byte, 1) != 1)
			_exit(1);
		_exit(0);
	}
	for (tries = 0; child > 0 && tries < 700 && lseek(fd, 0, SEEK_CUR) != 1; tries++)
		nanosleep(&nap, NULL);
	return child > 0 && lseek(fd, 0, SEEK_CUR) == 1 ? 0 : 1;
}

static int wait_fail(const char *step)
{
	printf("wait failed: %s errno=%d\n", step, errno);
	return 1;
}

/* wait4 on no child, on a running child with WNOHANG, on an exact PID that is
 * still running, on a child already reaped, on a process that is no child,
 * and on any child for one normal exit and one SIGSEGV. */
static int wait_test(void)
{
	struct timespec nap = {0, 50 * 1000 * 1000};
	struct rusage usage;
	pid_t first, second, crashed, got;
	int status, index, seen = 0;

	fflush(stdout);
	if (!fails(wait4(-1, &status, 0, NULL), ECHILD) || !fails(wait4(-1, &status, WNOHANG, NULL), ECHILD))
		return wait_fail("no children");
	if (!fails(wait4(-1, &status, 4, NULL), EINVAL))
		return wait_fail("options");
	first = fork();
	if (first == 0) {
		nanosleep(&nap, NULL);
		_exit(7);
	}
	if (first < 0 || wait4(first, &status, WNOHANG, NULL) != 0)
		return wait_fail("WNOHANG on a running child");
	memset(&usage, 0xff, sizeof usage);
	if (wait4(first, &status, 0, &usage) != first || !WIFEXITED(status) || WEXITSTATUS(status) != 7 ||
	    usage.ru_utime.tv_usec < 0 || usage.ru_utime.tv_usec >= 1000000)
		return wait_fail("exact child");
	if (!fails(wait4(first, &status, 0, NULL), ECHILD))
		return wait_fail("second wait");
	if (!fails(wait4(getppid(), &status, WNOHANG, NULL), ECHILD))
		return wait_fail("parent is no child");
	second = fork();
	if (second == 0)
		_exit(3);
	crashed = fork();
	if (crashed == 0) {
		*(volatile int *)0 = 1;
		_exit(0);
	}
	if (second < 0 || crashed < 0)
		return wait_fail("fork");
	for (index = 0; index < 2; index++) {
		got = wait4(-1, &status, 0, NULL);
		if (got == second && WIFEXITED(status) && WEXITSTATUS(status) == 3)
			seen |= 1;
		else if (got == crashed && WIFSIGNALED(status) && WTERMSIG(status) == SIGSEGV)
			seen |= 2;
	}
	if (seen != 3 || !fails(wait4(-1, &status, WNOHANG, NULL), ECHILD))
		return wait_fail("any child");
	second = fork();
	if (second == 0)
		_exit(0);
	if (second < 0 || waitpid(second, NULL, 0) != second)
		return wait_fail("null status");
	printf("wait ok\n");
	return 0;
}

static int pipe_fail(const char *step)
{
	printf("pipe failed: %s errno=%d\n", step, errno);
	return 1;
}

/* Reads until EOF and returns the byte count, or -1 on an error. */
static long drain(int fd, char *buffer, long size)
{
	long total = 0, count;

	while ((count = read(fd, buffer + total, size - total > 4096 ? 4096 : size - total)) > 0) {
		total += count;
		if (total == size)
			break;
	}
	return count < 0 ? -1 : total;
}

#define PIPE_RECORDS 16
static char pipe_data[2 * PIPE_RECORDS * 4096];

/* Two children write sixteen 4096-byte records each, one letter per child,
 * into one pipe. Every 4096-byte block the parent reads must hold one
 * letter: PIPE_BUF writes do not interleave. */
static int pipe_atomic(void)
{
	char record[4096];
	int fds[2], child, index, status;
	long total;

	if (pipe(fds) != 0)
		return pipe_fail("atomic pipe");
	for (child = 0; child < 2; child++) {
		pid_t pid = fork();

		if (pid == 0) {
			close(fds[0]);
			memset(record, 'a' + child, sizeof record);
			for (index = 0; index < PIPE_RECORDS; index++)
				if (write(fds[1], record, sizeof record) != sizeof record)
					_exit(1);
			_exit(0);
		}
		if (pid < 0)
			return pipe_fail("atomic fork");
	}
	close(fds[1]);
	total = drain(fds[0], pipe_data, sizeof pipe_data);
	close(fds[0]);
	for (child = 0; child < 2; child++)
		if (wait4(-1, &status, 0, NULL) < 0 || !WIFEXITED(status) || WEXITSTATUS(status) != 0)
			return pipe_fail("atomic writer");
	if (total != sizeof pipe_data)
		return pipe_fail("atomic total");
	for (total = 0; total < (long)sizeof pipe_data; total += 4096)
		for (index = 1; index < 4096; index++)
			if (pipe_data[total + index] != pipe_data[total])
				return pipe_fail("interleaved record");
	return 0;
}

static int pipe_test(void)
{
	struct timespec nap = {0, 50 * 1000 * 1000};
	struct stat reader, writer;
	char buffer[16];
	int fds[2], spare, status;
	pid_t child;
	long total;

	signal(SIGPIPE, SIG_IGN);
	fflush(stdout);
	if (!fails(pipe((int *)8), EFAULT) || !fails(pipe2(fds, O_DIRECT << 4), EINVAL))
		return pipe_fail("arguments");
	if (on_goos() && !fails(pipe2(fds, O_NONBLOCK), EINVAL))
		return pipe_fail("O_NONBLOCK");
	if (pipe2(fds, O_CLOEXEC) != 0 || fcntl(fds[0], F_GETFD) != FD_CLOEXEC || fcntl(fds[1], F_GETFD) != FD_CLOEXEC)
		return pipe_fail("pipe2 O_CLOEXEC");
	close(fds[0]);
	close(fds[1]);
	if (pipe(fds) != 0 || fcntl(fds[0], F_GETFD) != 0 ||
	    (fcntl(fds[0], F_GETFL) & O_ACCMODE) != O_RDONLY || (fcntl(fds[1], F_GETFL) & O_ACCMODE) != O_WRONLY)
		return pipe_fail("pipe");
	if (syscall(SYS_fstat, fds[0], &reader) != 0 || syscall(SYS_fstat, fds[1], &writer) != 0 || !S_ISFIFO(reader.st_mode) ||
	    reader.st_ino != writer.st_ino || reader.st_dev != writer.st_dev)
		return pipe_fail("fstat");
	if (!fails(lseek(fds[0], 0, SEEK_CUR), ESPIPE) || !fails(ioctl(fds[0], TIOCGWINSZ, buffer), ENOTTY) ||
	    !fails(write(fds[0], "x", 1), EBADF) || !fails(read(fds[1], buffer, 1), EBADF))
		return pipe_fail("direction");
	if (write(fds[1], "hello", 5) != 5 || read(fds[0], buffer, sizeof buffer) != 5 || memcmp(buffer, "hello", 5))
		return pipe_fail("round trip");
	spare = dup(fds[1]);
	close(fds[1]);
	if (spare < 0 || write(spare, "dup", 3) != 3 || close(spare) != 0 ||
	    read(fds[0], buffer, sizeof buffer) != 3 || read(fds[0], buffer, sizeof buffer) != 0)
		return pipe_fail("duplicate writer and EOF");
	close(fds[0]);
	if (pipe(fds) != 0 || close(fds[0]) != 0 || !fails(write(fds[1], "x", 1), EPIPE))
		return pipe_fail("broken pipe");
	close(fds[1]);

	/* A forked writer: the parent blocks in read until the child writes, and
	 * sees EOF once the child, the last writer, exits. */
	if (pipe(fds) != 0)
		return pipe_fail("fork pipe");
	child = fork();
	if (child == 0) {
		close(fds[0]);
		nanosleep(&nap, NULL);
		_exit(write(fds[1], "child", 5) == 5 ? 0 : 1);
	}
	close(fds[1]);
	if (child < 0 || read(fds[0], buffer, sizeof buffer) != 5 || memcmp(buffer, "child", 5) ||
	    read(fds[0], buffer, sizeof buffer) != 0 || wait4(child, &status, 0, NULL) != child || status != 0)
		return pipe_fail("forked writer");
	close(fds[0]);

	/* A writer larger than any pipe blocks until the reader drains it. */
	if (pipe(fds) != 0)
		return pipe_fail("large pipe");
	child = fork();
	if (child == 0) {
		close(fds[1]);
		total = drain(fds[0], pipe_data, sizeof pipe_data);
		_exit(total == sizeof pipe_data ? 0 : 1);
	}
	close(fds[0]);
	memset(pipe_data, 'z', sizeof pipe_data);
	for (total = 0; total < (long)sizeof pipe_data;) {
		long count = write(fds[1], pipe_data + total, sizeof pipe_data - total);

		if (count <= 0)
			return pipe_fail("large write");
		total += count;
	}
	close(fds[1]);
	if (child < 0 || wait4(child, &status, 0, NULL) != child || status != 0)
		return pipe_fail("large reader");
	if (pipe_atomic() != 0)
		return 1;
	printf("pipe ok\n");
	return 0;
}

static volatile int signal_count;
static volatile int signal_blocked_inside;
static volatile siginfo_t signal_info;

static void count_signal(int number, siginfo_t *info, void *context)
{
	sigset_t mask;

	(void)context;
	signal_count++;
	signal_info = *info;
	sigprocmask(SIG_SETMASK, NULL, &mask);
	signal_blocked_inside = sigismember(&mask, number);
}

/* Changes the result the interrupted write returns. */
static void rewrite_result(int number, siginfo_t *info, void *context)
{
	(void)number;
	(void)info;
	((ucontext_t *)context)->uc_mcontext.gregs[REG_RAX] = 1234;
}

/* Points the interrupted context at kernel memory. */
static void forge_return(int number, siginfo_t *info, void *context)
{
	(void)number;
	(void)info;
	((ucontext_t *)context)->uc_mcontext.gregs[REG_RIP] = (greg_t)0xffff800000001000ULL;
}

static int signal_fail(const char *step)
{
	printf("signal failed: %s errno=%d count=%d\n", step, errno, signal_count);
	return 1;
}

static int set_action(int number, void (*handler)(int, siginfo_t *, void *), int flags)
{
	struct sigaction action;

	memset(&action, 0, sizeof action);
	action.sa_sigaction = handler;
	action.sa_flags = SA_SIGINFO | flags;
	sigemptyset(&action.sa_mask);
	return sigaction(number, &action, NULL);
}

/* Forks a child that writes to a pipe with no reader after installing
 * handler, or none, and returns its wait status. */
static int broken_pipe_child(void (*handler)(int, siginfo_t *, void *))
{
	int fds[2], status = -1;
	pid_t child;

	/* The read end closes before the fork, so no reader exists when the
	 * child writes. */
	if (pipe(fds) != 0 || close(fds[0]) != 0)
		return -1;
	child = fork();
	if (child == 0) {
		long result;

		if (handler)
			set_action(SIGPIPE, handler, 0);
		else
			signal(SIGPIPE, SIG_DFL);
		result = write(fds[1], "x", 1);
		_exit(result == -1 ? 0 : 1);
	}
	close(fds[1]);
	if (child < 0 || wait4(child, &status, 0, NULL) != child)
		return -1;
	return status;
}

static int signal_test(void)
{
	struct sigaction query;
	sigset_t mask;
	int fds[2], status;
	long result;
	pid_t child;

	fflush(stdout);
	status = broken_pipe_child(NULL);
	if (!WIFSIGNALED(status) || WTERMSIG(status) != SIGPIPE)
		return signal_fail("default SIGPIPE");
	status = broken_pipe_child(forge_return);
	if (!WIFSIGNALED(status) || WTERMSIG(status) != SIGSEGV)
		return signal_fail("forged return address");
	if (!fails(sigaction(SIGKILL, &query, NULL), EINVAL))
		return signal_fail("SIGKILL handler");

	/* A handler runs before the failing write returns, with the signal
	 * blocked, and the mask comes back after it. */
	if (pipe(fds) != 0 || close(fds[0]) != 0 || set_action(SIGPIPE, count_signal, 0) != 0)
		return signal_fail("setup");
	result = write(fds[1], "x", 1);
	if (result != -1 || errno != EPIPE || signal_count != 1 || !signal_blocked_inside ||
	    signal_info.si_signo != SIGPIPE || signal_info.si_pid != getpid())
		return signal_fail("SIGPIPE handler");
	sigprocmask(SIG_SETMASK, NULL, &mask);
	if (sigismember(&mask, SIGPIPE))
		return signal_fail("mask after handler");

	/* A blocked signal stays pending and arrives when it is unblocked. */
	sigemptyset(&mask);
	sigaddset(&mask, SIGPIPE);
	sigprocmask(SIG_BLOCK, &mask, NULL);
	if (write(fds[1], "x", 1) != -1 || signal_count != 1)
		return signal_fail("blocked SIGPIPE");
	sigprocmask(SIG_UNBLOCK, &mask, NULL);
	if (signal_count != 2)
		return signal_fail("unblocked SIGPIPE");

	/* The handler's changes to the saved context take effect. */
	set_action(SIGPIPE, rewrite_result, 0);
	if (syscall(SYS_write, fds[1], "x", 1) != 1234)
		return signal_fail("context restore");

	/* SA_RESETHAND restores the default after one delivery. */
	set_action(SIGPIPE, count_signal, SA_RESETHAND);
	write(fds[1], "x", 1);
	if (signal_count != 3 || sigaction(SIGPIPE, NULL, &query) != 0 || query.sa_handler != SIG_DFL)
		return signal_fail("SA_RESETHAND");
	signal(SIGPIPE, SIG_IGN);
	if (!fails(write(fds[1], "x", 1), EPIPE) || signal_count != 3)
		return signal_fail("ignored SIGPIPE");
	close(fds[1]);

	/* SIGCHLD reports the child that ended and its status. */
	set_action(SIGCHLD, count_signal, 0);
	child = fork();
	if (child == 0)
		_exit(5);
	if (child < 0 || wait4(child, &status, 0, NULL) != child || signal_count != 4 ||
	    signal_info.si_signo != SIGCHLD || signal_info.si_pid != child ||
	    signal_info.si_code != CLD_EXITED || signal_info.si_status != 5)
		return signal_fail("SIGCHLD");
	signal(SIGCHLD, SIG_DFL);
	printf("signal ok\n");
	return 0;
}

static volatile int kill_seen;

static void note_kill(int number)
{
	kill_seen = number;
}

static int kill_fail(const char *step)
{
	printf("kill failed: %s errno=%d\n", step, errno);
	return 1;
}

/* Forks a child that runs body after it says it is ready, and returns its
 * PID once it has said so. */
static pid_t ready_child(int (*body)(int))
{
	int ready[2];
	char byte;
	pid_t child;

	if (pipe(ready) != 0)
		return -1;
	child = fork();
	if (child == 0) {
		close(ready[0]);
		_exit(body(ready[1]));
	}
	close(ready[1]);
	if (child < 0 || read(ready[0], &byte, 1) != 1)
		child = -1;
	close(ready[0]);
	return child;
}

/* Says ready, then sleeps in read on a pipe nobody writes. */
static int blocked_reader(int ready)
{
	int idle[2];
	char byte;

	if (pipe(idle) != 0 || write(ready, "r", 1) != 1)
		return 2;
	read(idle[0], &byte, 1);
	return 3;
}

/* Handles SIGUSR1, says ready, and naps until the handler has run. */
static int handled_user1(int ready)
{
	struct timespec nap = {0, 10 * 1000 * 1000};
	int tries;

	signal(SIGUSR1, note_kill);
	if (write(ready, "r", 1) != 1)
		return 2;
	for (tries = 0; tries < 500 && kill_seen != SIGUSR1; tries++)
		nanosleep(&nap, NULL);
	return kill_seen == SIGUSR1 ? 0 : 1;
}

static int blocker_go = -1;

/* Blocks SIGTERM, says ready, waits for the go byte, then unblocks it. */
static int blocked_term(int ready)
{
	sigset_t mask;
	char byte;

	sigemptyset(&mask);
	sigaddset(&mask, SIGTERM);
	sigprocmask(SIG_BLOCK, &mask, NULL);
	if (write(ready, "r", 1) != 1 || read(blocker_go, &byte, 1) != 1)
		return 2;
	sigprocmask(SIG_UNBLOCK, &mask, NULL);
	return 4;
}

static int asks_parent(int ready)
{
	if (write(ready, "r", 1) != 1)
		return 2;
	return fails(kill(getppid(), 0), EPERM) ? 0 : 1;
}

static int kill_test(void)
{
	struct sigaction action;
	int status, go[2];
	pid_t child;

	fflush(stdout);
	if (kill(getpid(), 0) != 0 || !fails(kill(getpid(), 65), EINVAL) || !fails(kill(30000, 0), ESRCH))
		return kill_fail("probes");
	if (on_goos() && (!fails(kill(0, SIGTERM), EINVAL) || !fails(kill(-1, SIGTERM), EINVAL) ||
	    !fails(kill(getppid(), 0), ESRCH) || !fails(kill(1, SIGKILL), ESRCH)))
		return kill_fail("targets outside the tree");

	/* A fatal signal ends a child asleep in a system call. */
	child = ready_child(blocked_reader);
	if (child < 0 || kill(child, 0) != 0 || kill(child, SIGTERM) != 0 ||
	    wait4(child, &status, 0, NULL) != child || !WIFSIGNALED(status) || WTERMSIG(status) != SIGTERM)
		return kill_fail("SIGTERM");
	child = ready_child(blocked_reader);
	if (child < 0 || kill(child, SIGKILL) != 0 || wait4(child, &status, 0, NULL) != child ||
	    !WIFSIGNALED(status) || WTERMSIG(status) != SIGKILL)
		return kill_fail("SIGKILL");

	/* A handled signal runs the child's handler. */
	child = ready_child(handled_user1);
	if (child < 0 || kill(child, SIGUSR1) != 0 || wait4(child, &status, 0, NULL) != child || status != 0)
		return kill_fail("SIGUSR1 handler");

	/* A blocked fatal signal waits until the child unblocks it. */
	if (pipe(go) != 0)
		return kill_fail("go pipe");
	blocker_go = go[0];
	child = ready_child(blocked_term);
	if (child < 0 || kill(child, SIGTERM) != 0 || write(go[1], "g", 1) != 1 ||
	    wait4(child, &status, 0, NULL) != child || !WIFSIGNALED(status) || WTERMSIG(status) != SIGTERM)
		return kill_fail("blocked SIGTERM");
	close(go[0]);
	close(go[1]);

	if (on_goos()) {
		child = ready_child(asks_parent);
		if (child < 0 || wait4(child, &status, 0, NULL) != child || status != 0)
			return kill_fail("child signals its parent");
	}

	/* raise uses tkill and runs the handler before it returns. */
	signal(SIGUSR2, note_kill);
	if (raise(SIGUSR2) != 0 || kill_seen != SIGUSR2 ||
	    syscall(SYS_tgkill, getpid(), getpid(), SIGUSR2) != 0 || !fails(syscall(SYS_tgkill, getpid(), getpid() + 1, SIGUSR2), ESRCH))
		return kill_fail("tkill and tgkill");
	signal(SIGUSR2, SIG_DFL);

	/* With SA_NOCLDWAIT a child leaves no zombie: wait4 sleeps until it
	 * ends and then has nothing to report. */
	memset(&action, 0, sizeof action);
	action.sa_handler = SIG_DFL;
	action.sa_flags = SA_NOCLDWAIT;
	sigemptyset(&action.sa_mask);
	if (sigaction(SIGCHLD, &action, NULL) != 0)
		return kill_fail("SA_NOCLDWAIT");
	child = fork();
	if (child == 0)
		_exit(0);
	if (child < 0 || !fails(wait4(-1, &status, 0, NULL), ECHILD))
		return kill_fail("no zombie");
	signal(SIGCHLD, SIG_DFL);
	if (on_goos()) {
		action.sa_flags = 0x20;
		if (!fails(sigaction(SIGUSR1, &action, NULL), EINVAL))
			return kill_fail("unknown sigaction flag");
	}
	printf("kill ok\n");
	return 0;
}

/* The fixture links only the musl objects the probe needs, and poll is not
 * among them. */
static int probe_poll(struct pollfd *entries, unsigned long count, int timeout)
{
	return syscall(SYS_poll, entries, count, timeout);
}

static volatile int eintr_seen;
static int eintr_pipe[2] = {-1, -1};

static void note_eintr(int number)
{
	(void)number;
	eintr_seen++;
}

static int eintr_handler(int number, int flags)
{
	struct sigaction action;

	memset(&action, 0, sizeof action);
	action.sa_handler = note_eintr;
	action.sa_flags = flags;
	sigemptyset(&action.sa_mask);
	return sigaction(number, &action, NULL);
}

static int eintr_read(int ready)
{
	char byte;

	if (eintr_handler(SIGUSR1, 0) != 0 || write(ready, "r", 1) != 1)
		return 2;
	return read(eintr_pipe[0], &byte, 1) == -1 && errno == EINTR && eintr_seen == 1 ? 0 : 1;
}

static int restart_read(int ready)
{
	char byte;

	if (eintr_handler(SIGUSR1, SA_RESTART) != 0 || write(ready, "r", 1) != 1)
		return 2;
	return read(eintr_pipe[0], &byte, 1) == 1 && byte == 'x' && eintr_seen == 1 ? 0 : 1;
}

static int eintr_sleep(int ready)
{
	struct timespec nap = {10, 0}, rem = {0, 0};

	if (eintr_handler(SIGUSR1, SA_RESTART) != 0 || write(ready, "r", 1) != 1)
		return 2;
	if (nanosleep(&nap, &rem) != -1 || errno != EINTR || eintr_seen != 1)
		return 1;
	return rem.tv_sec < 10 && (rem.tv_sec > 0 || rem.tv_nsec > 0) ? 0 : 3;
}

static int eintr_poll(int ready)
{
	struct pollfd entry = {eintr_pipe[0], POLLIN, 0};

	if (eintr_handler(SIGUSR1, SA_RESTART) != 0 || write(ready, "r", 1) != 1)
		return 2;
	return probe_poll(&entry, 1, -1) == -1 && errno == EINTR && eintr_seen == 1 ? 0 : 1;
}

static int eintr_wait(int ready)
{
	int status;
	pid_t sleeper = fork();

	if (sleeper == 0) {
		char byte;

		read(eintr_pipe[0], &byte, 1);
		_exit(0);
	}
	if (sleeper < 0 || eintr_handler(SIGUSR1, 0) != 0 || write(ready, "r", 1) != 1)
		return 2;
	if (wait4(-1, &status, 0, NULL) != -1 || errno != EINTR || eintr_seen != 1)
		return 1;
	kill(sleeper, SIGKILL);
	return wait4(sleeper, &status, 0, NULL) == sleeper ? 0 : 3;
}

/* Starts body in a child, waits until it is about to block, signals it, and
 * returns its exit status. A restarted child gets a byte to read after the
 * signal. */
static int interrupt_child(int (*body)(int), int feed)
{
	struct timespec settle = {0, 100 * 1000 * 1000};
	int status;
	pid_t child = ready_child(body);

	if (child < 0)
		return -1;
	nanosleep(&settle, NULL);
	if (kill(child, SIGUSR1) != 0)
		return -1;
	if (feed) {
		nanosleep(&settle, NULL);
		if (write(eintr_pipe[1], "x", 1) != 1)
			return -1;
	}
	if (wait4(child, &status, 0, NULL) != child || !WIFEXITED(status))
		return -1;
	return WEXITSTATUS(status);
}

static int eintr_test(void)
{
	struct timespec nap = {0, 100 * 1000 * 1000};
	char byte;
	pid_t child;
	int status;

	fflush(stdout);
	if (pipe(eintr_pipe) != 0)
		return 1;
	if ((status = interrupt_child(eintr_read, 0)) != 0) {
		printf("eintr failed: read %d\n", status);
		return 1;
	}
	if ((status = interrupt_child(restart_read, 1)) != 0) {
		printf("eintr failed: SA_RESTART read %d\n", status);
		return 1;
	}
	if ((status = interrupt_child(eintr_sleep, 0)) != 0) {
		printf("eintr failed: nanosleep %d\n", status);
		return 1;
	}
	if ((status = interrupt_child(eintr_poll, 0)) != 0) {
		printf("eintr failed: poll %d\n", status);
		return 1;
	}
	if ((status = interrupt_child(eintr_wait, 0)) != 0) {
		printf("eintr failed: wait4 %d\n", status);
		return 1;
	}
	/* SIGCHLD with a handler interrupts the parent's own read. */
	eintr_handler(SIGCHLD, 0);
	child = fork();
	if (child == 0) {
		nanosleep(&nap, NULL);
		_exit(0);
	}
	if (child < 0 || read(eintr_pipe[0], &byte, 1) != -1 || errno != EINTR || eintr_seen != 1 ||
	    wait4(child, &status, 0, NULL) != child) {
		printf("eintr failed: SIGCHLD read errno=%d\n", errno);
		return 1;
	}
	signal(SIGCHLD, SIG_DFL);
	printf("eintr ok\n");
	return 0;
}

static int poll_fail(const char *step)
{
	printf("poll failed: %s errno=%d\n", step, errno);
	return 1;
}

static long elapsed_ms(const struct timespec *start)
{
	struct timespec now;

	clock_gettime(CLOCK_MONOTONIC, &now);
	return (now.tv_sec - start->tv_sec) * 1000 + (now.tv_nsec - start->tv_nsec) / 1000000;
}

static int poll_test(void)
{
	struct timespec nap = {0, 50 * 1000 * 1000}, start;
	struct pollfd entries[4];
	char byte;
	int fds[2], file, status;
	pid_t child;

	signal(SIGPIPE, SIG_IGN);
	fflush(stdout);
	if (probe_poll(NULL, 0, 0) != 0 || pipe(fds) != 0)
		return poll_fail("empty");
	if (on_goos() && !fails(probe_poll(entries, 65, 0), EINVAL))
		return poll_fail("too many");
	entries[0] = (struct pollfd){fds[0], POLLIN, 0};
	entries[1] = (struct pollfd){fds[1], POLLOUT, 0};
	entries[2] = (struct pollfd){-1, POLLIN, 7};
	entries[3] = (struct pollfd){60, POLLIN, 0};
	if (probe_poll(entries, 4, 0) != 2 || entries[0].revents != 0 || entries[1].revents != POLLOUT ||
	    entries[2].revents != 0 || entries[3].revents != POLLNVAL)
		return poll_fail("idle pipe");
	if (write(fds[1], "x", 1) != 1 || probe_poll(entries, 1, 0) != 1 || entries[0].revents != POLLIN)
		return poll_fail("readable");
	read(fds[0], &byte, 1);
	clock_gettime(CLOCK_MONOTONIC, &start);
	if (probe_poll(entries, 1, 60) != 0 || elapsed_ms(&start) < 55)
		return poll_fail("timeout");
	child = fork();
	if (child == 0) {
		nanosleep(&nap, NULL);
		_exit(write(fds[1], "y", 1) == 1 ? 0 : 1);
	}
	if (child < 0 || probe_poll(entries, 1, -1) != 1 || entries[0].revents != POLLIN ||
	    wait4(child, &status, 0, NULL) != child || status != 0)
		return poll_fail("wake");
	read(fds[0], &byte, 1);
	close(fds[1]);
	if (probe_poll(entries, 1, -1) != 1 || entries[0].revents != POLLHUP)
		return poll_fail("hangup");
	close(fds[0]);
	if (pipe(fds) != 0 || close(fds[0]) != 0)
		return poll_fail("broken pipe");
	entries[1] = (struct pollfd){fds[1], POLLOUT, 0};
	if (probe_poll(&entries[1], 1, 0) != 1 || !(entries[1].revents & POLLERR))
		return poll_fail("error");
	close(fds[1]);
	file = open(IMAGE, O_RDONLY);
	entries[0] = (struct pollfd){file, POLLIN | POLLOUT, 0};
	if (file < 0 || probe_poll(entries, 1, -1) != 1 || entries[0].revents != (POLLIN | POLLOUT))
		return poll_fail("regular file");
	close(file);
	printf("poll ok\n");
	return 0;
}

static int probe_ppoll(struct pollfd *entries, unsigned long count, struct timespec *timeout, const sigset_t *mask, unsigned long size)
{
	return syscall(SYS_ppoll, entries, count, timeout, mask, size);
}

static volatile int ppoll_seen;

static void note_ppoll(int number)
{
	(void)number;
	ppoll_seen++;
}

static int ppoll_fail(const char *step)
{
	printf("ppoll failed: %s errno=%d seen=%d\n", step, errno, ppoll_seen);
	return 1;
}

static int user1_blocked(void)
{
	sigset_t mask;

	sigprocmask(SIG_SETMASK, NULL, &mask);
	return sigismember(&mask, SIGUSR1);
}

/* Sleeps in ppoll with SIGUSR1 let through, after saying ready. */
static int ppoll_sleeper(int ready)
{
	struct pollfd entry = {eintr_pipe[0], POLLIN, 0};
	sigset_t open;

	sigemptyset(&open);
	if (write(ready, "r", 1) != 1)
		return 2;
	if (probe_ppoll(&entry, 1, NULL, &open, 8) != -1 || errno != EINTR || ppoll_seen != 1)
		return 1;
	return user1_blocked() ? 0 : 3;
}

static int ppoll_test(void)
{
	struct timespec timeout = {0, 60 * 1000 * 1000}, bad = {0, 2000000000}, start;
	struct pollfd entry;
	sigset_t block, open;
	int fds[2], status;

	fflush(stdout);
	if (pipe(fds) != 0 || pipe(eintr_pipe) != 0)
		return ppoll_fail("pipe");
	entry = (struct pollfd){fds[0], POLLIN, 0};
	clock_gettime(CLOCK_MONOTONIC, &start);
	if (probe_ppoll(&entry, 1, &timeout, NULL, 0) != 0 || elapsed_ms(&start) < 55 ||
	    timeout.tv_sec != 0 || timeout.tv_nsec != 0)
		return ppoll_fail("timeout");
	if (!fails(probe_ppoll(&entry, 1, &bad, NULL, 0), EINVAL) || !fails(probe_ppoll(&entry, 1, NULL, &open, 4), EINVAL))
		return ppoll_fail("arguments");

	/* A signal the temporary mask lets through interrupts the call, and the
	 * handler's return gives the old mask back. */
	signal(SIGUSR1, note_ppoll);
	sigemptyset(&block);
	sigaddset(&block, SIGUSR1);
	sigemptyset(&open);
	sigprocmask(SIG_BLOCK, &block, NULL);
	raise(SIGUSR1);
	if (ppoll_seen != 0 || probe_ppoll(&entry, 1, NULL, &open, 8) != -1 || errno != EINTR ||
	    ppoll_seen != 1 || !user1_blocked())
		return ppoll_fail("pending signal");

	/* A ready descriptor wins: the old mask is back before any delivery. */
	raise(SIGUSR1);
	if (write(fds[1], "x", 1) != 1 || probe_ppoll(&entry, 1, NULL, &open, 8) != 1 || ppoll_seen != 1)
		return ppoll_fail("ready with a pending signal");
	sigprocmask(SIG_UNBLOCK, &block, NULL);
	if (ppoll_seen != 2)
		return ppoll_fail("unblock");
	sigprocmask(SIG_BLOCK, &block, NULL);

	/* rt_sigsuspend lets a pending signal through and restores the mask. */
	raise(SIGUSR1);
	if (syscall(SYS_rt_sigsuspend, &open, 8) != -1 || errno != EINTR || ppoll_seen != 3 || !user1_blocked())
		return ppoll_fail("sigsuspend");

	/* A signal sent while the call sleeps interrupts it. */
	ppoll_seen = 0;
	if ((status = interrupt_child(ppoll_sleeper, 0)) != 0) {
		printf("ppoll failed: sleeper %d\n", status);
		return 1;
	}
	sigprocmask(SIG_UNBLOCK, &block, NULL);
	signal(SIGUSR1, SIG_DFL);
	printf("ppoll ok\n");
	return 0;
}

/* The child sleeps on a pipe nobody writes; the parent says so and reads
 * standard input. Ctrl-C on the parent must end both. */
static int family_test(void)
{
	int idle[2];
	char byte;
	pid_t child;

	if (pipe(idle) != 0)
		return 1;
	child = fork();
	if (child == 0) {
		read(idle[0], &byte, 1);
		_exit(0);
	}
	printf("family blocking with child %d\n", child);
	fflush(stdout);
	read(0, &byte, 1);
	return 0;
}

static int hard_fail(const char *step)
{
	printf("hardening failed: %s errno=%d\n", step, errno);
	return 1;
}

/* Runs body in a child and returns its wait status. */
static int child_status(int (*body)(void))
{
	int status = -1;
	pid_t child;

	fflush(stdout);
	child = fork();
	if (child == 0)
		_exit(body());
	if (child < 0 || wait4(child, &status, 0, NULL) != child)
		return -1;
	return status;
}

static char *fork_area;
static char *fork_heap;
static long fork_page;

/* The child sees the parent's pages, touched and untouched, and its writes
 * stay its own. */
static int fork_memory_child(void)
{
	if (fork_area[0] != 'p' || fork_area[fork_page] != 0 || fork_area[2 * fork_page] != 'r' ||
	    fork_heap[0] != 'h')
		return 1;
	fork_area[0] = 'c';
	fork_area[fork_page] = 'c';
	fork_heap[0] = 'c';
	return 0;
}

static int sigsegv_status(int status)
{
	return WIFSIGNALED(status) && WTERMSIG(status) == SIGSEGV;
}

static void spin_handler(int number)
{
	char byte;
	int idle[2];

	(void)number;
	if (pipe(idle) == 0)
		read(idle[0], &byte, 1);
}

static unsigned long bad_handler, bad_restorer;

/* Installs a raw action whose handler or restorer the process cannot run,
 * and signals itself. */
static int raw_handler_child(void)
{
	struct kernel_action action = {(void (*)(int))bad_handler, ACTION_RESTORER, (void (*)(void))bad_restorer, 0};

	if (syscall(SYS_rt_sigaction, SIGUSR1, &action, NULL, 8) != 0)
		return 2;
	syscall(SYS_tkill, getpid(), SIGUSR1);
	return 3;
}

static void plain_handler(int number)
{
	(void)number;
}

static int native_gate_child(void)
{
	return isolation() == 0 ? 0 : 1;
}

static int hardening_test(void)
{
	char *arguments[] = {IMAGE, NULL};
	static char big[70000];
	char *large[] = {big, NULL};
	char byte;
	int fds[2], shared, status, index, count;
	pid_t child;

	signal(SIGPIPE, SIG_IGN);
	/* Every failed exec leaves this image running. */
	if (!fails(execve((char *)8, arguments, NULL), EFAULT) || !fails(execve(IMAGE, (char **)8, NULL), EFAULT) ||
	    !fails(execve("/tmp/no-such-image", arguments, NULL), ENOENT) ||
	    !fails(execve("/tmp", arguments, NULL), EACCES))
		return hard_fail("failed exec");
	if (on_goos()) {
		memset(big, 'e', sizeof big - 1);
		if (!fails(execve("/system/bin/echo", arguments, NULL), ENOEXEC) ||
		    !fails(execve(IMAGE, arguments, large), E2BIG))
			return hard_fail("refused exec");
		/* A forked child cannot reach the native gate either. */
		if (child_status(native_gate_child) != 0)
			return hard_fail("native gate in a child");
	}
	printf("exec failures ok\n");

	/* Fork copies touched, untouched, read-only, and brk pages, and maps
	 * a PROT_NONE region without touching it. */
	fork_page = sysconf(_SC_PAGESIZE);
	fork_area = mmap(NULL, 3 * fork_page, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	/* musl's sbrk refuses to grow the break, so the probe asks brk. */
	fork_heap = (char *)syscall(SYS_brk, 0);
	if (syscall(SYS_brk, fork_heap + fork_page) != (long)(fork_heap + fork_page))
		fork_heap = (void *)-1;
	if (fork_area == MAP_FAILED || fork_heap == (void *)-1 ||
	    mmap(NULL, fork_page, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0) == MAP_FAILED)
		return hard_fail("fork memory setup");
	fork_area[0] = 'p';
	fork_area[2 * fork_page] = 'r';
	fork_heap[0] = 'h';
	if (mprotect(fork_area + 2 * fork_page, fork_page, PROT_READ) != 0)
		return hard_fail("mprotect");
	status = child_status(fork_memory_child);
	if (status != 0 || fork_area[0] != 'p' || fork_area[fork_page] != 0 || fork_heap[0] != 'h')
		return hard_fail("fork memory");
	printf("fork memory ok\n");

	/* Descriptors: dup and fork share one description, status flags
	 * included, and close-on-exec bits copy but stay per descriptor. */
	if (pipe(fds) != 0 || fcntl(fds[1], F_SETFD, FD_CLOEXEC) != 0)
		return hard_fail("descriptor setup");
	shared = open(IMAGE, O_RDONLY);
	child = fork();
	if (child == 0) {
		int copy = dup(shared);

		_exit(fcntl(fds[1], F_GETFD) == FD_CLOEXEC && fcntl(fds[0], F_GETFD) == 0 && copy >= 0 &&
		      read(copy, &byte, 1) == 1 && close(fds[0]) == 0 && fcntl(shared, F_SETFL, O_APPEND) == 0 ? 0 : 1);
	}
	if (child < 0 || wait4(child, &status, 0, NULL) != child || status != 0 ||
	    lseek(shared, 0, SEEK_CUR) != 1 || fcntl(fds[0], F_GETFD) != 0 || !(fcntl(shared, F_GETFL) & O_APPEND))
		return hard_fail("shared descriptions");
	close(shared);
	printf("descriptors ok\n");

	/* A writer asleep on a full pipe dies of SIGKILL, and the read end then
	 * sees EOF. Pipes run out with EMFILE and come back. */
	child = fork();
	if (child == 0) {
		static char fill[65536];

		close(fds[0]);
		for (;;)
			write(fds[1], fill, sizeof fill);
	}
	close(fds[1]);
	{
		struct timespec settle = {0, 100 * 1000 * 1000};

		nanosleep(&settle, NULL);
	}
	if (child < 0 || kill(child, SIGKILL) != 0 || wait4(child, &status, 0, NULL) != child ||
	    !WIFSIGNALED(status) || WTERMSIG(status) != SIGKILL)
		return hard_fail("killed writer");
	while ((count = read(fds[0], big, sizeof big)) > 0)
		;
	if (count != 0)
		return hard_fail("EOF after the writer died");
	close(fds[0]);
	for (count = 0; count < 4096 && pipe(fds) == 0; count++)
		;
	if (errno != EMFILE && errno != ENFILE)
		return hard_fail("pipe exhaustion");
	for (index = 3; index < 4096 + 3; index++)
		close(index);
	if (pipe(fds) != 0)
		return hard_fail("pipes after exhaustion");
	close(fds[0]);
	close(fds[1]);
	printf("pipes ok\n");

	/* Signal frames that return to bad places end the process, and SIGKILL
	 * ends one that sleeps in its handler. */
	bad_handler = 0x1000;
	bad_restorer = (unsigned long)plain_handler;
	if (!sigsegv_status(child_status(raw_handler_child)))
		return hard_fail("unmapped handler");
	bad_handler = 0xffff800000001000UL;
	if (!sigsegv_status(child_status(raw_handler_child)))
		return hard_fail("kernel handler");
	bad_handler = (unsigned long)plain_handler;
	bad_restorer = 0x10;
	if (!sigsegv_status(child_status(raw_handler_child)))
		return hard_fail("bad restorer");
	if (pipe(fds) != 0)
		return hard_fail("handler pipe");
	child = fork();
	if (child == 0) {
		signal(SIGUSR1, spin_handler);
		write(fds[1], "r", 1);
		for (;;)
			pause();
	}
	if (child < 0 || read(fds[0], &byte, 1) != 1 || kill(child, SIGUSR1) != 0)
		return hard_fail("handler child");
	{
		struct timespec settle = {0, 100 * 1000 * 1000};

		nanosleep(&settle, NULL);
	}
	if (kill(child, SIGKILL) != 0 || wait4(child, &status, 0, NULL) != child || !WIFSIGNALED(status) ||
	    WTERMSIG(status) != SIGKILL)
		return hard_fail("kill in a handler");
	close(fds[0]);
	close(fds[1]);
	printf("signal frames ok\n");
	printf("hardening ok\n");
	return 0;
}

/* GoOS only. The family budget ends a fork bomb with EAGAIN, zombies count
 * against it, and reaping returns the slots. */
static int limits_test(void)
{
	pid_t children[64];
	int idle[2], count, index, status;
	char byte;

	if (!on_goos() || pipe(idle) != 0)
		return 1;
	fflush(stdout);
	for (count = 0; count < 64; count++) {
		children[count] = fork();
		if (children[count] == 0) {
			read(idle[0], &byte, 1);
			_exit(0);
		}
		if (children[count] < 0)
			break;
	}
	if (count == 64 || errno != EAGAIN || count != 15) {
		printf("limits failed: %d children, errno=%d\n", count, errno);
		return 1;
	}
	for (index = 0; index < count; index++)
		kill(children[index], SIGKILL);
	for (index = 0; index < count; index++)
		if (wait4(children[index], &status, 0, NULL) != children[index])
			return 1;
	printf("fork bomb bounded at %d\n", count);
	for (count = 0; count < 64; count++) {
		children[count] = fork();
		if (children[count] == 0)
			_exit(0);
		if (children[count] < 0)
			break;
	}
	if (count != 15 || errno != EAGAIN || wait4(children[0], &status, 0, NULL) != children[0]) {
		printf("limits failed: %d zombies, errno=%d\n", count, errno);
		return 1;
	}
	children[0] = fork();
	if (children[0] == 0)
		_exit(0);
	if (children[0] < 0)
		return 1;
	for (index = 0; index < count; index++)
		if (wait4(-1, &status, 0, NULL) < 0)
			return 1;
	if (!fails(wait4(-1, &status, WNOHANG, NULL), ECHILD))
		return 1;
	printf("limits ok\n");
	return 0;
}

/* GoOS validation builds only. Under this working directory the kernel
 * fails fork after a few pages of its copy and checks its rollback. */
#define FORK_FAULT_DIRECTORY "/tmp/.validation-fork-fault"

static int fork_fault_test(void)
{
	int fds[2], status;
	char byte;
	pid_t child;

	if (!on_goos() || pipe(fds) != 0 || (syscall(SYS_mkdir, FORK_FAULT_DIRECTORY, 0755) != 0 && errno != EEXIST) ||
	    chdir(FORK_FAULT_DIRECTORY) != 0)
		return 1;
	fflush(stdout);
	child = fork();
	if (child == 0)
		_exit(9);
	if (child != -1 || errno != ENOMEM) {
		printf("fork fault failed: fork returned %d errno=%d\n", child, errno);
		return 1;
	}
	/* No child exists to wait for, and none holds the write end: closing
	 * the only one ends the pipe. */
	if (chdir("/") != 0 || !fails(wait4(-1, &status, WNOHANG, NULL), ECHILD) || close(fds[1]) != 0 ||
	    read(fds[0], &byte, 1) != 0) {
		printf("fork fault failed: a child survived errno=%d\n", errno);
		return 1;
	}
	child = fork();
	if (child == 0)
		_exit(7);
	if (child < 0 || wait4(child, &status, 0, NULL) != child || !WIFEXITED(status) || WEXITSTATUS(status) != 7) {
		printf("fork fault failed: the next fork errno=%d\n", errno);
		return 1;
	}
	printf("fork fault ok\n");
	return 0;
}

/* GoOS validation builds only. The VFS dies when it is asked to prepare
 * this file as an executable, in the middle of execve. */
#define EXEC_CRASH_PATH "/tmp/.validation-vfs-exec-crash"

/* touch_image reads one byte of every page of every PT_LOAD segment. The
 * segments need not be contiguous: the linker leaves an unmapped page
 * between them when a section asks for page alignment. */
static void touch_image(void)
{
	const Elf64_Phdr *headers = (const Elf64_Phdr *)getauxval(AT_PHDR);
	unsigned long count = getauxval(AT_PHNUM), index;
	volatile char sink = 0;

	for (index = 0; headers && index < count; index++) {
		uintptr_t page, last;

		if (headers[index].p_type != PT_LOAD || headers[index].p_memsz == 0)
			continue;
		page = headers[index].p_vaddr & ~(uintptr_t)4095;
		last = headers[index].p_vaddr + headers[index].p_memsz - 1;
		for (; page <= last; page += 4096)
			sink += *(volatile char *)page;
	}
	(void)sink;
}
static int exec_loss_canary = 0x5a;

static int exec_vfs_loss_test(void)
{
	char *arguments[] = {EXEC_CRASH_PATH, NULL};
	char cwd[16], *heap = malloc(32);
	pid_t pid = getpid();
	int kept, file;

	if (!on_goos() || !heap)
		return 1;
	strcpy(heap, "heap");
	/* The image pages in from the VFS on first touch. Every page is read
	 * now, while the VFS still answers. */
	touch_image();
	kept = open(IMAGE, O_RDONLY);
	file = open(EXEC_CRASH_PATH, O_CREAT | O_TRUNC | O_WRONLY, 0755);
	if (kept < 0 || file < 0 || write(file, "\177ELF", 4) != 4 || close(file) != 0 || chdir("/tmp") != 0)
		return 1;
	fflush(stdout);
	if (execve(EXEC_CRASH_PATH, arguments, NULL) != -1 || errno != EIO) {
		printf("exec vfs loss failed: errno=%d\n", errno);
		return 1;
	}
	/* The old image still runs: same PID, memory, descriptors, and cwd. */
	if (getpid() != pid || exec_loss_canary != 0x5a || strcmp(heap, "heap") || fcntl(kept, F_GETFD) != 0 ||
	    fcntl(1, F_GETFD) != 0 || !getcwd(cwd, sizeof cwd) || strcmp(cwd, "/tmp")) {
		printf("exec vfs loss failed: state changed\n");
		return 1;
	}
	printf("exec vfs loss ok errno=%d\n", EIO);
	return 0;
}

/* XMM0, XMM1, XMM7, and XMM15 carry one 16-byte pattern each, MXCSR rounds
 * toward zero, and the x87 control word selects 53-bit precision. */
static unsigned char fp_in[64], fp_out[64];
static volatile int fp_nested_ok, fp_seen;

#define FP_MXCSR 0x7f80u
#define FP_FCW 0x027fu

/* Loads XMM0, XMM1, XMM7, XMM15 from fp_in, MXCSR, and the x87 control
 * word, makes one system call inside the same asm block, and stores the state
 * the return gives back in fp_out, *mxcsr, and *fcw. */
static long fp_syscall(long number, long first, long second, unsigned int mxcsr_in, unsigned short fcw_in,
		       unsigned int *mxcsr, unsigned short *fcw)
{
	unsigned int mx = mxcsr_in;
	unsigned short cw = fcw_in;
	long result;

	__asm__ volatile("ldmxcsr %[mx]\n\tfldcw %[cw]\n\t"
			 "movdqu 0(%[in]), %%xmm0\n\tmovdqu 16(%[in]), %%xmm1\n\t"
			 "movdqu 32(%[in]), %%xmm7\n\tmovdqu 48(%[in]), %%xmm15\n\t"
			 "syscall\n\t"
			 "movdqu %%xmm0, 0(%[out])\n\tmovdqu %%xmm1, 16(%[out])\n\t"
			 "movdqu %%xmm7, 32(%[out])\n\tmovdqu %%xmm15, 48(%[out])\n\t"
			 "stmxcsr %[mx]\n\tfnstcw %[cw]"
			 : "=a"(result), [mx] "+m"(mx), [cw] "+m"(cw)
			 : "a"(number), "D"(first), "S"(second), [in] "r"(fp_in), [out] "r"(fp_out)
			 : "rcx", "r11", "memory", "xmm0", "xmm1", "xmm7", "xmm15");
	*mxcsr = mx;
	*fcw = cw;
	return result;
}

/* Signals the calling thread with the state loaded. */
static long fp_signal(long signal, unsigned int *mxcsr, unsigned short *fcw)
{
	return fp_syscall(SYS_tkill, getpid(), signal, FP_MXCSR, FP_FCW, mxcsr, fcw);
}

static void fp_clobber(void)
{
	unsigned int mx = 0x1f80;
	unsigned short cw = 0x037f;

	__asm__ volatile("pcmpeqb %%xmm0, %%xmm0\n\tpxor %%xmm1, %%xmm1\n\t"
			 "pcmpeqb %%xmm7, %%xmm7\n\tpxor %%xmm15, %%xmm15\n\t"
			 "ldmxcsr %[mx]\n\tfldcw %[cw]"
			 :
			 : [mx] "m"(mx), [cw] "m"(cw)
			 : "xmm0", "xmm1", "xmm7", "xmm15");
}

static void fp_clobber_handler(int number)
{
	(void)number;
	fp_seen++;
	fp_clobber();
}

/* Runs a second signal inside the first handler and checks that its own
 * state comes back, then clobbers everything before it returns. */
static void fp_outer_handler(int number)
{
	unsigned char saved[64];
	unsigned int mxcsr;
	unsigned short fcw;

	(void)number;
	memcpy(saved, fp_in, sizeof saved);
	for (int index = 0; index < 64; index++)
		fp_in[index] = (unsigned char)(0xa0 + index);
	fp_signal(SIGUSR2, &mxcsr, &fcw);
	fp_nested_ok = !memcmp(fp_out, fp_in, sizeof fp_out) && mxcsr == FP_MXCSR && fcw == FP_FCW;
	memcpy(fp_in, saved, sizeof saved);
	fp_clobber();
}

/* Rewrites the saved XMM1 image: offset 160 holds XMM0, 176 XMM1. */
static void fp_edit_handler(int number, siginfo_t *info, void *context)
{
	unsigned char *image = (unsigned char *)((ucontext_t *)context)->uc_mcontext.fpregs;

	(void)number;
	(void)info;
	memset(image + 176, 0x3c, 16);
	fp_clobber();
}

static void fp_null_handler(int number, siginfo_t *info, void *context)
{
	(void)number;
	(void)info;
	((ucontext_t *)context)->uc_mcontext.fpregs = NULL;
}

static unsigned long fp_bad_address;

static void fp_bad_pointer_handler(int number, siginfo_t *info, void *context)
{
	(void)number;
	(void)info;
	((ucontext_t *)context)->uc_mcontext.fpregs = (void *)fp_bad_address;
}

static void fp_bad_mxcsr_handler(int number, siginfo_t *info, void *context)
{
	unsigned char *image = (unsigned char *)((ucontext_t *)context)->uc_mcontext.fpregs;

	(void)number;
	(void)info;
	memset(image + 24, 0xff, 4);
}

static int fp_bad_child(void)
{
	unsigned int mxcsr;
	unsigned short fcw;

	if (set_action(SIGUSR1, fp_bad_mxcsr_handler, 0) != 0 ||
	    (fp_bad_address != 0 && set_action(SIGUSR1, fp_bad_pointer_handler, 0) != 0))
		return 2;
	fp_signal(SIGUSR1, &mxcsr, &fcw);
	/* Reserved MXCSR bits are cleared, as Linux clears them. */
	return mxcsr >> 16 == 0 ? 3 : 4;
}

/* State unlike both the initial state and the signal tests' state: MXCSR
 * rounds down with flush-to-zero and every exception masked, and the x87
 * control word selects 24-bit precision. */
#define FP_PLAIN_MXCSR 0xbf80u
#define FP_PLAIN_FCW 0x007fu

/* Loads the plain state, makes one call, and checks that the state came back
 * as loaded. A forked child checks its own copy and exits with the verdict. */
static int fp_call_keeps_state(const char *name, long number, long first, long second)
{
	unsigned int mxcsr;
	unsigned short fcw;
	int status;
	long result;

	for (int index = 0; index < 64; index++)
		fp_in[index] = (unsigned char)(0xe1 ^ (index * 13));
	fflush(stdout);
	result = fp_syscall(number, first, second, FP_PLAIN_MXCSR, FP_PLAIN_FCW, &mxcsr, &fcw);
	int kept = !memcmp(fp_out, fp_in, sizeof fp_out) && mxcsr == FP_PLAIN_MXCSR && fcw == FP_PLAIN_FCW;
	if (number == SYS_fork && result == 0)
		_exit(kept ? 0 : 1);
	if (number == SYS_fork && (result < 0 || wait4(result, &status, 0, NULL) != result || status != 0))
		kept = 0;
	if (!kept)
		printf("fpstate failed: %s mxcsr=%#x fcw=%#x result=%ld\n", name, mxcsr, fcw, result);
	return kept;
}

static int fpstate_test(void)
{
	struct timespec nap = {0, 30 * 1000 * 1000};
	unsigned int mxcsr;
	unsigned short fcw;
	char *pages;
	int status;

	fflush(stdout);
	/* A call that returns at once, one that sleeps in the scheduler, and a
	 * fork child all see the caller's state. */
	if (!fp_call_keeps_state("getpid", SYS_getpid, 0, 0) ||
	    !fp_call_keeps_state("nanosleep", SYS_nanosleep, (long)&nap, 0) ||
	    !fp_call_keeps_state("fork", SYS_fork, 0, 0))
		return 1;
	for (int index = 0; index < 64; index++)
		fp_in[index] = (unsigned char)(index * 7 + 1);

	/* A handler that clobbers every register gives nothing back. */
	signal(SIGUSR1, fp_clobber_handler);
	if (fp_signal(SIGUSR1, &mxcsr, &fcw) != 0 || fp_seen != 1 || memcmp(fp_out, fp_in, sizeof fp_out) ||
	    mxcsr != FP_MXCSR || fcw != FP_FCW) {
		printf("fpstate failed: clobbered mxcsr=%#x fcw=%#x seen=%d\n", mxcsr, fcw, fp_seen);
		return 1;
	}

	/* A signal inside a handler restores the handler's own state. */
	signal(SIGUSR1, fp_outer_handler);
	signal(SIGUSR2, fp_clobber_handler);
	if (fp_signal(SIGUSR1, &mxcsr, &fcw) != 0 || !fp_nested_ok || memcmp(fp_out, fp_in, sizeof fp_out) ||
	    mxcsr != FP_MXCSR || fcw != FP_FCW) {
		printf("fpstate failed: nested\n");
		return 1;
	}

	/* An edited image is what comes back. */
	set_action(SIGUSR1, fp_edit_handler, 0);
	fp_signal(SIGUSR1, &mxcsr, &fcw);
	for (int index = 0; index < 16; index++)
		if (fp_out[16 + index] != 0x3c || fp_out[index] != fp_in[index]) {
			printf("fpstate failed: edited image\n");
			return 1;
		}

	/* A null fpstate returns to the initial state. */
	set_action(SIGUSR1, fp_null_handler, 0);
	fp_signal(SIGUSR1, &mxcsr, &fcw);
	if (mxcsr != 0x1f80 || fcw != 0x037f) {
		printf("fpstate failed: null image mxcsr=%#x fcw=%#x\n", mxcsr, fcw);
		return 1;
	}

	/* An image the process cannot read, one in the kernel half, and one
	 * that runs off a mapping end the process with SIGSEGV. */
	pages = mmap(NULL, 8192, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	if (pages == MAP_FAILED || munmap(pages + 4096, 4096) != 0)
		return 1;
	unsigned long bad[] = {0x1000, 0xffff800000001000UL, (unsigned long)pages + 4096 - 256};
	for (int index = 0; index < 3; index++) {
		fp_bad_address = bad[index];
		if (!sigsegv_status(status = child_status(fp_bad_child))) {
			printf("fpstate failed: bad image %d status=%#x\n", index, status);
			return 1;
		}
	}
	/* What a Linux kernel does with them depends on the CPU, which may fault
	 * on the load; an emulated one may not. GoOS clears them on any CPU. */
	fp_bad_address = 0;
	if (on_goos() && (status = child_status(fp_bad_child)) != 3 << 8) {
		printf("fpstate failed: reserved MXCSR status=%#x\n", status);
		return 1;
	}
	signal(SIGUSR1, SIG_DFL);
	signal(SIGUSR2, SIG_DFL);
	printf("fpstate ok\n");
	return 0;
}

/* Two pages of initialized data. The first is made read-only the way glibc
 * protects its relocated data (PT_GNU_RELRO) before main; the second stays
 * writable. */
static char libc_relro[2 * 4096] __attribute__((aligned(4096))) = {1};

struct probe_robust_list_head {
	void *list;
	long offset;
	void *pending;
};

#define PROBE_FUTEX_WAIT_PRIVATE 128
#define PROBE_FUTEX_WAKE_PRIVATE 129

static int libc_test(void)
{
	struct probe_robust_list_head head = {&head, 0, NULL};
	unsigned int words[2] = {1, 1};
	struct timeval tv;
	struct sysinfo info;
	long now, stored = 0;
	mode_t previous;
	char link[16];

	libc_relro[4096] = 2;
	if (mprotect(libc_relro, 4096, PROT_READ) != 0 || libc_relro[0] != 1) {
		printf("libc failed: mprotect of the image errno=%d\n", errno);
		return 1;
	}
	libc_relro[4096] = 3;
	if (libc_relro[4096] != 3) {
		printf("libc failed: the page after the read-only one\n");
		return 1;
	}
	errno = 0;
	if ((syscall(SYS_set_robust_list, &head, sizeof head) != 0 && errno != ENOSYS) ||
	    !fails(syscall(SYS_set_robust_list, &head, sizeof head - 1), EINVAL)) {
		printf("libc failed: set_robust_list\n");
		return 1;
	}
	previous = syscall(SYS_umask, 027);
	if (syscall(SYS_umask, previous) != 027 || syscall(SYS_umask, previous) != previous) {
		printf("libc failed: umask\n");
		return 1;
	}
	if (!fails(syscall(SYS_readlink, "/", link, sizeof link), EINVAL) ||
	    !fails(syscall(SYS_readlink, "/no-such-link", link, sizeof link), ENOENT) ||
	    !fails(syscall(SYS_readlink, "/", link, 0), EINVAL) ||
	    !fails(syscall(SYS_readlinkat, AT_FDCWD, "/no-such-link", link, sizeof link), ENOENT)) {
		printf("libc failed: readlink\n");
		return 1;
	}
	if (syscall(SYS_futex, &words[0], PROBE_FUTEX_WAKE_PRIVATE, 1, NULL, NULL, 0) != 0 ||
	    !fails(syscall(SYS_futex, &words[0], PROBE_FUTEX_WAIT_PRIVATE, 2, NULL, NULL, 0), EAGAIN) ||
	    !fails(syscall(SYS_futex, (char *)&words[0] + 1, PROBE_FUTEX_WAKE_PRIVATE, 1, NULL, NULL, 0), EINVAL)) {
		printf("libc failed: futex\n");
		return 1;
	}
	if (syscall(SYS_gettimeofday, &tv, NULL) != 0 || tv.tv_usec < 0 || tv.tv_usec >= 1000000 ||
	    syscall(SYS_gettimeofday, NULL, NULL) != 0) {
		printf("libc failed: gettimeofday\n");
		return 1;
	}
	now = syscall(SYS_time, &stored);
	if (now <= 0 || stored != now || now < tv.tv_sec || now > tv.tv_sec + 2) {
		printf("libc failed: time %ld stored %ld timeval %ld\n", now, stored, (long)tv.tv_sec);
		return 1;
	}
	if (syscall(SYS_sysinfo, &info) != 0 || info.mem_unit < 1 || info.totalram == 0 ||
	    info.freeram > info.totalram || info.procs < 1) {
		printf("libc failed: sysinfo\n");
		return 1;
	}
	printf("libc ok\n");
	return 0;
}

/* Three threads add to one counter under a mutex and report through a
 * condition; the main thread waits for all three, then joins them. Each
 * thread has its own TID and its own copy of a thread-local variable. */
#define PROBE_THREADS 3
#define PROBE_ROUNDS 2000

static pthread_mutex_t probe_lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t probe_done = PTHREAD_COND_INITIALIZER;
static long probe_counter;
static int probe_finished;
static pid_t probe_tids[PROBE_THREADS];
static __thread long probe_local = 7;

static void *probe_worker(void *argument)
{
	long index = (long)argument, round;

	probe_local = 100 + index;
	probe_tids[index] = syscall(SYS_gettid);
	for (round = 0; round < PROBE_ROUNDS; round++) {
		pthread_mutex_lock(&probe_lock);
		probe_counter++;
		pthread_mutex_unlock(&probe_lock);
		if (round % 256 == 0)
			sched_yield();
	}
	pthread_mutex_lock(&probe_lock);
	probe_finished++;
	pthread_cond_signal(&probe_done);
	pthread_mutex_unlock(&probe_lock);
	return (void *)(probe_local * 2);
}

static void *probe_exit_group(void *argument)
{
	(void)argument;
	syscall(SYS_exit_group, 42);
	return NULL;
}

/* Two threads checksum their own stack buffers for a few seconds, long
 * enough for the timer to switch between them many times, and compare
 * every round with the first. A register, SSE, or stack value lost across
 * a switch shows as a different sum. */
static void *probe_grind(void *argument)
{
	unsigned char buffer[64 * 1024];
	unsigned long seed = (unsigned long)argument, first = 0, sum, round, index;
	struct timespec start, now;

	clock_gettime(CLOCK_MONOTONIC, &start);
	for (round = 0;; round++) {
		for (index = 0; index < sizeof buffer; index++)
			buffer[index] = (unsigned char)(index * 31 + seed);
		memmove(buffer + 1, buffer, sizeof buffer - 1);
		sum = 0;
		for (index = 0; index < sizeof buffer; index++)
			sum = sum * 131 + buffer[index] + (double)index * 0.5;
		if (round == 0)
			first = sum;
		else if (sum != first)
			return (void *)1;
		clock_gettime(CLOCK_MONOTONIC, &now);
		if (now.tv_sec - start.tv_sec >= 3)
			return round > 1 ? NULL : (void *)2;
	}
}

/* A thread writes a file while another reads a different one, many times
 * over. Every byte read must be the reader's file and every byte written
 * the writer's: the two calls may not share a buffer in the kernel. */
static void *probe_writer(void *argument)
{
	char block[4096];
	int fd = open((const char *)argument, O_CREAT | O_TRUNC | O_WRONLY, 0644), round;

	if (fd < 0)
		return (void *)1;
	memset(block, 'w', sizeof block);
	for (round = 0; round < 64; round++)
		if (write(fd, block, sizeof block) != sizeof block)
			return (void *)2;
	return close(fd) == 0 ? NULL : (void *)3;
}

static int probe_file_io(void)
{
	char block[4096], *dir = getcwd(NULL, 0);
	static char reader_path[64], writer_path[64];
	pthread_t writer;
	void *result;
	int fd, round, index;

	snprintf(reader_path, sizeof reader_path, "%s/threads-read", dir ? dir : "/tmp");
	snprintf(writer_path, sizeof writer_path, "%s/threads-write", dir ? dir : "/tmp");
	free(dir);
	fd = open(reader_path, O_CREAT | O_TRUNC | O_RDWR, 0644);
	memset(block, 'r', sizeof block);
	for (round = 0; fd >= 0 && round < 64; round++)
		if (write(fd, block, sizeof block) != sizeof block)
			return 1;
	if (fd < 0 || pthread_create(&writer, NULL, probe_writer, writer_path) != 0)
		return 1;
	for (round = 0; round < 64; round++) {
		if (pread(fd, block, sizeof block, (off_t)round * sizeof block) != sizeof block)
			return 1;
		for (index = 0; index < (int)sizeof block; index++)
			if (block[index] != 'r')
				return 1;
	}
	if (pthread_join(writer, &result) != 0 || result != NULL || close(fd) != 0)
		return 1;
	fd = open(writer_path, O_RDONLY);
	for (round = 0; fd >= 0 && round < 64; round++) {
		if (read(fd, block, sizeof block) != sizeof block)
			return 1;
		for (index = 0; index < (int)sizeof block; index++)
			if (block[index] != 'w')
				return 1;
	}
	unlink(reader_path);
	unlink(writer_path);
	return fd < 0 || close(fd) != 0;
}

static volatile sig_atomic_t probe_raised;

static void probe_on_usr1(int signal)
{
	(void)signal;
	probe_raised = 1;
}

/* raise in a thread is tgkill(PID, TID): the signal reaches the process. */
static void *probe_raiser(void *argument)
{
	(void)argument;
	return raise(SIGUSR1) == 0 ? NULL : (void *)1;
}

static int threads_test(void)
{
	pthread_t workers[PROBE_THREADS];
	pid_t pid = getpid(), tid = syscall(SYS_gettid), child;
	void *result;
	long index;
	int status;

	if (tid != pid) {
		printf("threads failed: main TID %d is not the PID %d\n", tid, pid);
		return 1;
	}
	for (index = 0; index < PROBE_THREADS; index++)
		if (pthread_create(&workers[index], NULL, probe_worker, (void *)index) != 0) {
			printf("threads failed: create %ld\n", index);
			return 1;
		}
	pthread_mutex_lock(&probe_lock);
	while (probe_finished < PROBE_THREADS)
		pthread_cond_wait(&probe_done, &probe_lock);
	pthread_mutex_unlock(&probe_lock);
	for (index = 0; index < PROBE_THREADS; index++) {
		if (pthread_join(workers[index], &result) != 0 || (long)result != 2 * (100 + index)) {
			printf("threads failed: join %ld\n", index);
			return 1;
		}
		if (probe_tids[index] == pid || probe_tids[index] <= 0 ||
		    (index > 0 && probe_tids[index] == probe_tids[index - 1])) {
			printf("threads failed: TIDs %d %d %d\n", probe_tids[0], probe_tids[1], probe_tids[2]);
			return 1;
		}
	}
	if (probe_counter != PROBE_THREADS * PROBE_ROUNDS || probe_local != 7 || getpid() != pid) {
		printf("threads failed: counter %ld local %ld\n", probe_counter, probe_local);
		return 1;
	}
	printf("threads joined: %d threads %d rounds\n", PROBE_THREADS, PROBE_ROUNDS);
	{
		pthread_t grinders[2];

		for (index = 0; index < 2; index++)
			if ((status = pthread_create(&grinders[index], NULL, probe_grind, (void *)(index + 3))) != 0) {
				printf("threads failed: create grinder %ld: %d\n", index, status);
				return 1;
			}
		probe_grind((void *)9);
		for (index = 0; index < 2; index++)
			if (pthread_join(grinders[index], &result) != 0 || result != NULL) {
				printf("threads failed: grinder %ld result %ld\n", index, (long)result);
				return 1;
			}
		printf("threads preempted: state kept\n");
	}
	if (probe_file_io() != 0) {
		printf("threads failed: concurrent file I/O\n");
		return 1;
	}
	printf("threads file I/O: kept apart\n");
	{
		pthread_t raiser;
		struct sigaction action = {0};

		action.sa_handler = probe_on_usr1;
		if (sigaction(SIGUSR1, &action, NULL) != 0 || pthread_create(&raiser, NULL, probe_raiser, NULL) != 0 ||
		    pthread_join(raiser, &result) != 0 || result != NULL) {
			printf("threads failed: raise in a thread\n");
			return 1;
		}
		while (!probe_raised)
			sched_yield();
		printf("threads signal: delivered\n");
	}
	/* exit_group from a thread ends the whole process with its status. */
	fflush(stdout);
	child = fork();
	if (child == 0) {
		pthread_t ender;

		pthread_create(&ender, NULL, probe_exit_group, NULL);
		for (;;)
			pause();
	}
	if (child < 0 || wait4(child, &status, 0, NULL) != child || !WIFEXITED(status) || WEXITSTATUS(status) != 42) {
		printf("threads failed: exit_group from a thread, status %#x\n", status);
		return 1;
	}
	printf("threads ok\n");
	return 0;
}

/* smp runs on every online processor at once. Workers record the
 * processors getcpu reports and whether another worker computed on another
 * processor at the same moment, so the result proves parallel execution
 * rather than threads that merely completed. */
#define SMP_WORKERS 16
#define SMP_SECONDS 3
/* A Linux process has eight tasks on GoOS: the main thread and seven more. */
#define SMP_THREADS 7

static int smp_count;
static volatile int smp_go, smp_stop;
static volatile int smp_inside[SMP_WORKERS];
static volatile int smp_where[SMP_WORKERS];
static unsigned long smp_seen[SMP_WORKERS];
static int smp_overlap[SMP_WORKERS];
static int smp_fpu_bad;

static int smp_cpu(void)
{
	unsigned cpu = 0;

	if (syscall(SYS_getcpu, &cpu, NULL, NULL) != 0)
		return -1;
	return (int)cpu;
}

static long smp_elapsed_ms(const struct timespec *start)
{
	struct timespec now;

	clock_gettime(CLOCK_MONOTONIC, &now);
	return (now.tv_sec - start->tv_sec) * 1000 + (now.tv_nsec - start->tv_nsec) / 1000000;
}

/* One slice of SSE work with a known answer. A state lost across a
 * preemption or a move to another processor shows as a different sum. The
 * worker's progress counter advances while it computes. */
static volatile unsigned long smp_progress[SMP_WORKERS];

static double smp_slice(int seed)
{
	volatile double sum = 0;
	int index;

	for (index = 1; index < 20000; index++) {
		sum += (double)(index ^ seed) / (double)index;
		if (index % 512 == 0 && seed < SMP_WORKERS)
			smp_progress[seed]++;
	}
	return sum;
}

static long smp_now_us(void)
{
	struct timespec now;

	clock_gettime(CLOCK_MONOTONIC, &now);
	return now.tv_sec * 1000000 + now.tv_nsec / 1000;
}

/* smp_watch reports whether worker other computed on another processor
 * during a window shorter than half a scheduler tick in which this thread
 * stayed on cpu. A thread that lost its processor to another task would be
 * away for a whole tick, so both ran at the same moment. */
static int smp_watch(int other, int cpu)
{
	unsigned long before, after;
	long start = smp_now_us(), spin;

	if (!__atomic_load_n(&smp_inside[other], __ATOMIC_SEQ_CST) || smp_where[other] == cpu)
		return 0;
	before = smp_progress[other];
	for (spin = 0; spin < 2000; spin++)
		__asm__ volatile("pause");
	after = smp_progress[other];
	return after != before && smp_now_us() - start < 5000 && smp_where[other] != cpu && smp_cpu() == cpu;
}

static void *smp_worker(void *argument)
{
	long self = (long)argument;
	double expected = smp_slice((int)self + SMP_WORKERS);
	struct timespec start, nap = {0, 2000000};
	int cpu, other, round = 0;

	while (!smp_go)
		sched_yield();
	clock_gettime(CLOCK_MONOTONIC, &start);
	while (!smp_stop && smp_elapsed_ms(&start) < SMP_SECONDS * 1000) {
		cpu = smp_cpu();
		if (cpu < 0 || cpu >= 64)
			return (void *)1;
		smp_seen[self] |= 1ul << cpu;
		smp_where[self] = cpu;
		__atomic_store_n(&smp_inside[self], 1, __ATOMIC_SEQ_CST);
		if (smp_slice((int)self) != smp_slice((int)self) || smp_slice((int)self + SMP_WORKERS) != expected)
			__atomic_store_n(&smp_fpu_bad, 1, __ATOMIC_SEQ_CST);
		for (other = 0; other < smp_count; other++)
			if (other != self && smp_watch(other, cpu))
				smp_overlap[self]++;
		__atomic_store_n(&smp_inside[self], 0, __ATOMIC_SEQ_CST);
		/* A short sleep now and then lets the scheduler place the worker
		 * again, on whichever processor is free. */
		if (++round % 16 == 0)
			nanosleep(&nap, NULL);
	}
	return NULL;
}

/* Threads take one mutex and pass one condition back and forth. */
#define SMP_LOCK_ROUNDS 20000
static pthread_mutex_t smp_lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t smp_turn = PTHREAD_COND_INITIALIZER;
static long smp_counter;
static int smp_token;

static void *smp_contender(void *argument)
{
	long self = (long)argument, round;

	for (round = 0; round < SMP_LOCK_ROUNDS; round++) {
		pthread_mutex_lock(&smp_lock);
		smp_counter++;
		pthread_mutex_unlock(&smp_lock);
	}
	for (round = 0; round < 200; round++) {
		pthread_mutex_lock(&smp_lock);
		while (smp_token != self % 2)
			pthread_cond_wait(&smp_turn, &smp_lock);
		smp_token = !smp_token;
		pthread_cond_broadcast(&smp_turn);
		pthread_mutex_unlock(&smp_lock);
	}
	return NULL;
}

/* A page is discarded and written again at one address while another
 * thread reads it on another processor. Each discard frees the physical
 * page and the next write faults in a fresh one, so after a new value is
 * published the reader must see it, or zero: an old translation would show
 * an older value, or the bytes of whoever has the freed page now. */
#define SMP_REMAPS 400
static volatile unsigned long *smp_page;
static volatile int smp_generation, smp_reader_bad, smp_reader_done;

static void *smp_reader(void *argument)
{
	int seen;
	unsigned long value;

	(void)argument;
	while (!__atomic_load_n(&smp_reader_done, __ATOMIC_ACQUIRE)) {
		seen = __atomic_load_n(&smp_generation, __ATOMIC_ACQUIRE);
		if (seen <= 0)
			continue;
		value = smp_page[0];
		if (value < (unsigned long)seen && value != 0)
			smp_reader_bad = 1;
	}
	return NULL;
}

static int smp_remap(void)
{
	void *address = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	pthread_t reader;
	int round;

	if (address == MAP_FAILED)
		return 2;
	smp_page = address;
	smp_page[0] = 1;
	__atomic_store_n(&smp_generation, 1, __ATOMIC_RELEASE);
	if (pthread_create(&reader, NULL, smp_reader, NULL) != 0)
		return 2;
	for (round = 2; round < SMP_REMAPS; round++) {
		if (madvise(address, 4096, MADV_DONTNEED) != 0)
			return 2;
		smp_page[0] = (unsigned long)round;
		__atomic_store_n(&smp_generation, round, __ATOMIC_RELEASE);
		if (round % 4 == 0 && (mprotect(address, 4096, PROT_READ) != 0 || mprotect(address, 4096, PROT_READ | PROT_WRITE) != 0))
			return 2;
	}
	__atomic_store_n(&smp_reader_done, 1, __ATOMIC_RELEASE);
	pthread_join(reader, NULL);
	munmap(address, 4096);
	return smp_reader_bad;
}

/* Several threads touch the same untouched pages of one mapping at once:
 * every fault but the first finds the page already there. */
#define SMP_FAULT_PAGES 64
static volatile unsigned char *smp_lazy;
static volatile int smp_fault_go;

static void *smp_toucher(void *argument)
{
	long self = (long)argument;
	int page;

	while (!smp_fault_go)
		;
	for (page = 0; page < SMP_FAULT_PAGES; page++)
		smp_lazy[page * 4096 + self] = (unsigned char)(self + 1);
	return NULL;
}

/* A child creates spinning threads and calls execve: every thread must be
 * gone before the new image runs, and the new image must start. */
static void *smp_spinner(void *argument)
{
	(void)argument;
	for (;;)
		smp_slice(SMP_WORKERS);
	return NULL;
}

static int smp_exec_with_threads(void)
{
	char *arguments[] = {"/system/linux/hello", NULL};
	pid_t child;
	int status, index;

	fflush(stdout);
	child = fork();
	if (child == 0) {
		pthread_t spinner;

		for (index = 0; index < 3; index++)
			pthread_create(&spinner, NULL, smp_spinner, NULL);
		execv(arguments[0], arguments);
		_exit(9);
	}
	return child < 0 || wait4(child, &status, 0, NULL) != child || !WIFEXITED(status) || WEXITSTATUS(status) != 0;
}

/* smp_speedup times a fixed amount of SSE work done by one thread, then the
 * same work split over threads threads, and returns the ratio in tenths. It
 * is a coarse sanity figure, not a pass mark: host load moves it. */
#define SMP_UNITS 96

static void *smp_units(void *argument)
{
	long units = (long)argument, index;

	for (index = 0; index < units; index++)
		smp_slice(SMP_WORKERS);
	return NULL;
}

static long smp_speedup(int threads)
{
	pthread_t helpers[SMP_WORKERS];
	long start, single, parallel;
	int index;

	start = smp_now_us();
	smp_units((void *)(long)SMP_UNITS);
	single = smp_now_us() - start;
	start = smp_now_us();
	for (index = 0; index < threads; index++)
		if (pthread_create(&helpers[index], NULL, smp_units, (void *)(long)(SMP_UNITS / threads)) != 0)
			return 0;
	for (index = 0; index < threads; index++)
		pthread_join(helpers[index], NULL);
	parallel = smp_now_us() - start;
	return parallel > 0 ? single * 10 / parallel : 0;
}

/* smp_affinity limits the calling thread to one processor and checks that
 * it runs nowhere else, that a thread made before keeps its own mask, and
 * that an empty mask is refused. Widened to two processors while a thread
 * pinned to the first keeps it busy, the caller moves to the second. The
 * kernel need not move it at once, so the check runs until a deadline. */
static volatile int affinity_stop, affinity_tid;

/* The bystander sleeps between looks, so the processors it may use go idle
 * and the widened caller has somewhere to go. */
static void *affinity_bystander(void *argument)
{
	struct timespec nap = {0, 2000000};

	(void)argument;
	__atomic_store_n(&affinity_tid, (int)syscall(SYS_gettid), __ATOMIC_SEQ_CST);
	while (!__atomic_load_n(&affinity_stop, __ATOMIC_SEQ_CST))
		nanosleep(&nap, NULL);
	return NULL;
}

static void *affinity_hog(void *argument)
{
	unsigned long mask = (unsigned long)argument;

	if (syscall(SYS_sched_setaffinity, 0, sizeof mask, &mask) != 0)
		return (void *)1;
	while (!__atomic_load_n(&affinity_stop, __ATOMIC_SEQ_CST))
		smp_slice(SMP_WORKERS);
	return NULL;
}

static int smp_affinity_fail(const char *step)
{
	printf("smp failed: affinity %s\n", step);
	__atomic_store_n(&affinity_stop, 1, __ATOMIC_SEQ_CST);
	return 1;
}

static int smp_affinity(unsigned long online)
{
	unsigned long mask = 0, one, pair, empty = 0;
	struct timespec nap = {0, 1000000};
	pthread_t bystander, hog;
	int target, second, round, moved = 0;
	long start;

	if (syscall(SYS_sched_getaffinity, 0, sizeof mask, &mask) != sizeof mask || mask != online)
		return smp_affinity_fail("read");
	if ((online & (online - 1)) == 0) {
		printf("smp affinity: one processor\n");
		return 0;
	}
	target = 63 - __builtin_clzl(online);
	second = __builtin_ctzl(online);
	one = 1ul << target;
	pair = one | 1ul << second;
	if (pthread_create(&bystander, NULL, affinity_bystander, NULL) != 0)
		return smp_affinity_fail("bystander");
	while (!__atomic_load_n(&affinity_tid, __ATOMIC_SEQ_CST))
		sched_yield();
	if (syscall(SYS_sched_setaffinity, 0, sizeof one, &one) != 0)
		return smp_affinity_fail("restrict");
	for (round = 0; round < 2000; round++) {
		if (smp_cpu() != target)
			return smp_affinity_fail("ran outside its mask");
		if (round % 64 == 0)
			sched_yield();
		if (round % 400 == 0)
			nanosleep(&nap, NULL);
	}
	mask = 0;
	if (syscall(SYS_sched_getaffinity, 0, sizeof mask, &mask) != sizeof mask || mask != one)
		return smp_affinity_fail("read back");
	mask = 0;
	if (syscall(SYS_sched_getaffinity, affinity_tid, sizeof mask, &mask) != sizeof mask || mask != online)
		return smp_affinity_fail("bystander mask");
	if (syscall(SYS_sched_setaffinity, 0, sizeof empty, &empty) != -1 || errno != EINVAL)
		return smp_affinity_fail("empty mask");
	if (pthread_create(&hog, NULL, affinity_hog, (void *)one) != 0)
		return smp_affinity_fail("hog");
	if (syscall(SYS_sched_setaffinity, 0, sizeof pair, &pair) != 0)
		return smp_affinity_fail("widen");
	start = smp_now_us();
	while (!moved && smp_now_us() - start < 3000000) {
		smp_slice(SMP_WORKERS);
		sched_yield();
		moved = smp_cpu() == second;
	}
	__atomic_store_n(&affinity_stop, 1, __ATOMIC_SEQ_CST);
	pthread_join(hog, NULL);
	pthread_join(bystander, NULL);
	if (!moved)
		return smp_affinity_fail("never moved after widening");
	if (syscall(SYS_sched_setaffinity, 0, sizeof online, &online) != 0)
		return smp_affinity_fail("restore");
	printf("smp affinity: held to cpu %d for %d checks, moved to cpu %d after widening\n", target, round, second);
	return 0;
}

/* smp_pinned runs one CPU-bound worker pinned to each processor, up to
 * four: each must make progress and run only on its own processor. */
#define PINNED_WORKERS 4

static volatile int pinned_go;
static volatile unsigned long pinned_progress[PINNED_WORKERS];
static volatile int pinned_strayed[PINNED_WORKERS];

static void *pinned_worker(void *argument)
{
	long index = (long)argument & 0xff, cpu = (long)argument >> 8;
	unsigned long mask = 1ul << cpu;
	long start;

	if (syscall(SYS_sched_setaffinity, 0, sizeof mask, &mask) != 0)
		return (void *)1;
	while (!__atomic_load_n(&pinned_go, __ATOMIC_SEQ_CST))
		sched_yield();
	start = smp_now_us();
	while (smp_now_us() - start < 300000) {
		smp_slice(SMP_WORKERS);
		pinned_progress[index]++;
		if (smp_cpu() != cpu)
			pinned_strayed[index] = 1;
	}
	return NULL;
}

static int smp_pinned(unsigned long online)
{
	pthread_t workers[PINNED_WORKERS];
	unsigned long left = online;
	long index, count = 0;
	void *result;

	while (left != 0 && count < PINNED_WORKERS) {
		long cpu = __builtin_ctzl(left);

		left &= left - 1;
		if (pthread_create(&workers[count], NULL, pinned_worker, (void *)(cpu << 8 | count)) != 0) {
			printf("smp failed: pinned worker\n");
			return 1;
		}
		count++;
	}
	__atomic_store_n(&pinned_go, 1, __ATOMIC_SEQ_CST);
	for (index = 0; index < count; index++)
		if (pthread_join(workers[index], &result) != 0 || result != NULL || pinned_strayed[index] || pinned_progress[index] == 0) {
			printf("smp failed: pinned worker %ld strayed %d, progress %lu\n", index, pinned_strayed[index], pinned_progress[index]);
			return 1;
		}
	printf("smp pinned: %ld workers, one per processor, each stayed and ran\n", count);
	return 0;
}

/* smp_deadlines checks the timeouts of waits: a short one fires, readiness
 * beats a long one, a timeout beats readiness that comes later, a timeout
 * readiness ended never fires into a later sleep, and many timeouts on many
 * threads all expire. The scheduler tick is 10 ms, so a timeout may fire up
 * to one tick early by the monotonic clock. */
#define DEADLINE_THREADS 6
#define DEADLINE_SLACK_US 10000

static int deadline_ends[2];

static void *deadline_writer(void *argument)
{
	struct timespec pause_for = {0, 20000000};

	(void)argument;
	nanosleep(&pause_for, NULL);
	return write(deadline_ends[1], "x", 1) == 1 ? NULL : (void *)1;
}

static void *deadline_timer(void *argument)
{
	long index = (long)argument, timeout = 10 * (index + 1), start;
	int ends[2], ready;
	struct pollfd poller;

	if (pipe(ends) != 0)
		return (void *)1;
	poller.fd = ends[0];
	poller.events = POLLIN;
	start = smp_now_us();
	ready = probe_poll(&poller, 1, (int)timeout);
	close(ends[0]);
	close(ends[1]);
	return ready == 0 && smp_now_us() - start >= timeout * 1000 - DEADLINE_SLACK_US ? NULL : (void *)1;
}

static int smp_deadline_fail(const char *step)
{
	printf("smp failed: deadline %s\n", step);
	return 1;
}

static int smp_deadlines(void)
{
	struct pollfd poller;
	struct timespec sleep_for = {0, 300000000};
	pthread_t threads[DEADLINE_THREADS];
	char byte;
	long start, index;
	void *result;

	if (pipe(deadline_ends) != 0)
		return smp_deadline_fail("pipe");
	poller.fd = deadline_ends[0];
	poller.events = POLLIN;
	start = smp_now_us();
	if (probe_poll(&poller, 1, 30) != 0 || smp_now_us() - start < 30000 - DEADLINE_SLACK_US)
		return smp_deadline_fail("short timeout");
	if (write(deadline_ends[1], "x", 1) != 1)
		return smp_deadline_fail("write");
	start = smp_now_us();
	if (probe_poll(&poller, 1, 2000) != 1 || smp_now_us() - start > 1000000 || read(deadline_ends[0], &byte, 1) != 1)
		return smp_deadline_fail("readiness before a timeout");
	start = smp_now_us();
	if (pthread_create(&threads[0], NULL, deadline_writer, NULL) != 0 || probe_poll(&poller, 1, 200) != 1 ||
	    pthread_join(threads[0], &result) != 0 || result != NULL || read(deadline_ends[0], &byte, 1) != 1)
		return smp_deadline_fail("readiness during a timeout");
	/* The poll's timeout would have fired 200 ms after it began; a sleep
	 * that spans that moment must last its whole length. */
	start = smp_now_us();
	if (nanosleep(&sleep_for, NULL) != 0 || smp_now_us() - start < 300000 - DEADLINE_SLACK_US)
		return smp_deadline_fail("a finished wait's timeout fired later");
	if (probe_poll(&poller, 1, 30) != 0 || write(deadline_ends[1], "x", 1) != 1 || probe_poll(&poller, 1, 0) != 1 ||
	    read(deadline_ends[0], &byte, 1) != 1)
		return smp_deadline_fail("readiness after a timeout");
	for (index = 0; index < DEADLINE_THREADS; index++)
		if (pthread_create(&threads[index], NULL, deadline_timer, (void *)index) != 0)
			return smp_deadline_fail("timer thread");
	for (index = 0; index < DEADLINE_THREADS; index++)
		if (pthread_join(threads[index], &result) != 0 || result != NULL)
			return smp_deadline_fail("concurrent timeouts");
	close(deadline_ends[0]);
	close(deadline_ends[1]);
	printf("smp deadlines: timeouts, wakes, and %d concurrent timeouts\n", DEADLINE_THREADS);
	return 0;
}

/* kernel mode: threads and processes that stay inside the kernel, with
 * calls that take no lock in common, so several processors run kernel code
 * at once. The kernel reports the most processors it saw inside system calls
 * at the same time; the boot test reads that after this mode. */
#define KERNEL_THREADS 4
#define KERNEL_PROCESSES 2
#define KERNEL_ROUNDS 4000

static volatile int kernel_go;
static int kernel_words[KERNEL_THREADS + 1];

static void *kernel_grind(void *argument)
{
	long index = (long)argument;
	struct timespec now;
	unsigned char bytes[16];
	unsigned long mask;
	int round;

	while (!kernel_go)
		;
	for (round = 0; round < KERNEL_ROUNDS; round++)
		if (syscall(SYS_futex, &kernel_words[index], PROBE_FUTEX_WAKE_PRIVATE, 1, NULL, NULL, 0) != 0 ||
		    syscall(SYS_getpid) <= 0 || syscall(SYS_gettid) <= 0 ||
		    syscall(SYS_clock_gettime, CLOCK_MONOTONIC, &now) != 0 ||
		    syscall(SYS_getrandom, bytes, sizeof bytes, 0) != sizeof bytes ||
		    syscall(SYS_sched_getaffinity, 0, sizeof mask, &mask) != sizeof mask)
			return (void *)1;
	return NULL;
}

static int kernel_test(void)
{
	pthread_t workers[KERNEL_THREADS];
	pid_t children[KERNEL_PROCESSES];
	long index;
	int status, failed = 0;
	void *result;

	fflush(stdout);
	for (index = 0; index < KERNEL_PROCESSES; index++) {
		children[index] = fork();
		if (children[index] == 0) {
			kernel_go = 1;
			_exit(kernel_grind((void *)KERNEL_THREADS) == NULL ? 0 : 1);
		}
		if (children[index] < 0) {
			printf("kernel failed: fork\n");
			return 1;
		}
	}
	for (index = 0; index < KERNEL_THREADS; index++)
		if (pthread_create(&workers[index], NULL, kernel_grind, (void *)index) != 0) {
			printf("kernel failed: thread %ld\n", index);
			return 1;
		}
	kernel_go = 1;
	for (index = 0; index < KERNEL_THREADS; index++)
		if (pthread_join(workers[index], &result) != 0 || result != NULL)
			failed = 1;
	for (index = 0; index < KERNEL_PROCESSES; index++)
		if (wait4(children[index], &status, 0, NULL) != children[index] || !WIFEXITED(status) || WEXITSTATUS(status) != 0)
			failed = 1;
	if (failed) {
		printf("kernel failed: a worker's call failed\n");
		return 1;
	}
	printf("kernel workers: %d threads and %d processes, %d rounds of 6 calls each\n", KERNEL_THREADS, KERNEL_PROCESSES, KERNEL_ROUNDS);
	printf("kernel ok\n");
	return 0;
}

/* domains mode: the work of one program spread over the kernel's wide lock
 * domains. Two threads read and write files of their own through the VFS,
 * a third moves bytes through a pipe, and a fourth forks and reaps children.
 * Several copies with different tags, started at once, are the lock profile's
 * workload: unrelated programs doing unrelated work. */
#define DOMAINS_ROUNDS 200

static const char *domains_tag;
static volatile int domains_go;

static void *domains_files(void *argument)
{
	char path[64], block[512], back[512];
	long index = (long)argument;
	int round, fd;

	snprintf(path, sizeof path, "/tmp/domains-%s-%ld", domains_tag, index);
	memset(block, 'a' + (int)index, sizeof block);
	while (!domains_go)
		;
	for (round = 0; round < DOMAINS_ROUNDS; round++) {
		fd = open(path, O_RDWR | O_CREAT | O_TRUNC, 0644);
		if (fd < 0 || write(fd, block, sizeof block) != sizeof block || lseek(fd, 0, SEEK_SET) != 0 ||
		    read(fd, back, sizeof back) != sizeof back || memcmp(block, back, sizeof block) != 0 ||
		    lseek(fd, 0, SEEK_END) != sizeof block || close(fd) != 0)
			return (void *)1;
	}
	return unlink(path) == 0 ? NULL : (void *)1;
}

static void *domains_pipe(void *argument)
{
	int ends[2], round;
	char byte = 'p', back;

	(void)argument;
	if (pipe(ends) != 0)
		return (void *)1;
	while (!domains_go)
		;
	for (round = 0; round < DOMAINS_ROUNDS * 4; round++)
		if (write(ends[1], &byte, 1) != 1 || read(ends[0], &back, 1) != 1 || back != byte)
			return (void *)1;
	close(ends[0]);
	close(ends[1]);
	return NULL;
}

static void *domains_processes(void *argument)
{
	int round, status;

	(void)argument;
	while (!domains_go)
		;
	for (round = 0; round < DOMAINS_ROUNDS / 10; round++) {
		pid_t child = fork();

		if (child == 0)
			_exit(7);
		if (child < 0 || wait4(child, &status, 0, NULL) != child || !WIFEXITED(status) || WEXITSTATUS(status) != 7)
			return (void *)1;
	}
	return NULL;
}

static int domains_test(const char *tag)
{
	pthread_t workers[4];
	void *(*bodies[4])(void *) = {domains_files, domains_files, domains_pipe, domains_processes};
	long index;
	int failed = 0;
	void *result;

	domains_tag = tag;
	fflush(stdout);
	for (index = 0; index < 4; index++)
		if (pthread_create(&workers[index], NULL, bodies[index], (void *)index) != 0) {
			printf("domains %s failed: thread %ld\n", tag, index);
			return 1;
		}
	domains_go = 1;
	for (index = 0; index < 4; index++)
		if (pthread_join(workers[index], &result) != 0 || result != NULL)
			failed = 1;
	printf(failed ? "domains %s failed\n" : "domains %s ok\n", tag);
	return failed;
}

static int smp_test(void)
{
	pthread_t workers[SMP_WORKERS];
	unsigned long mask = 0, seen = 0;
	long index;
	int migrated = 0, overlaps = 0, status;
	void *result;

	if (syscall(SYS_sched_getaffinity, 0, sizeof mask, &mask) != sizeof mask || mask == 0) {
		printf("smp failed: sched_getaffinity\n");
		return 1;
	}
	smp_count = __builtin_popcountl(mask) + 1;
	if (smp_count > SMP_THREADS)
		smp_count = SMP_THREADS;
	printf("smp cpus: %d online\n", __builtin_popcountl(mask));
	for (index = 0; index < smp_count; index++)
		if (pthread_create(&workers[index], NULL, smp_worker, (void *)index) != 0) {
			printf("smp failed: create worker %ld\n", index);
			return 1;
		}
	smp_go = 1;
	for (index = 0; index < smp_count; index++) {
		if (pthread_join(workers[index], &result) != 0 || result != NULL) {
			printf("smp failed: worker %ld\n", index);
			return 1;
		}
		seen |= smp_seen[index];
		overlaps += smp_overlap[index];
		if (__builtin_popcountl(smp_seen[index]) > 1)
			migrated++;
	}
	printf("smp workers: %d on cpus %#lx, %d moved, %d parallel samples\n", smp_count, seen, migrated, overlaps);
	/* With fewer workers than processors, each worker must find one of its
	 * own; with more, every processor must have run one. */
	if (__builtin_popcountl(seen) < (smp_count < __builtin_popcountl(mask) ? smp_count : __builtin_popcountl(mask)) ||
	    (seen & ~mask) != 0 || (mask & (mask - 1)) != 0 && (overlaps == 0 || migrated == 0) || smp_fpu_bad) {
		printf("smp failed: cpus %#lx of %#lx, overlaps %d, moved %d, fpu %d\n", seen, mask, overlaps, migrated, smp_fpu_bad);
		return 1;
	}
	for (index = 0; index < 4; index++)
		if (pthread_create(&workers[index], NULL, smp_contender, (void *)index) != 0)
			return 1;
	for (index = 0; index < 4; index++)
		pthread_join(workers[index], NULL);
	if (smp_counter != 4 * SMP_LOCK_ROUNDS) {
		printf("smp failed: mutex counter %ld\n", smp_counter);
		return 1;
	}
	printf("smp futex: %ld locked increments\n", smp_counter);
	if ((status = smp_remap()) != 0) {
		printf("smp failed: %s\n", status == 1 ? "a reader saw a discarded page" : "discard and write");
		return 1;
	}
	smp_lazy = mmap(NULL, SMP_FAULT_PAGES * 4096, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	if (smp_lazy == MAP_FAILED)
		return 1;
	for (index = 0; index < 4; index++)
		pthread_create(&workers[index], NULL, smp_toucher, (void *)index);
	smp_fault_go = 1;
	for (index = 0; index < 4; index++)
		pthread_join(workers[index], NULL);
	for (index = 0; index < SMP_FAULT_PAGES * 4; index++)
		if (smp_lazy[(index / 4) * 4096 + index % 4] != index % 4 + 1) {
			printf("smp failed: concurrent fault lost a write\n");
			return 1;
		}
	printf("smp vm: discards and concurrent faults kept\n");
	if (smp_affinity(mask) != 0 || smp_pinned(mask) != 0 || smp_deadlines() != 0)
		return 1;
	/* Measured before the spinners below start: they never stop. */
	{
		int threads = __builtin_popcountl(mask) > SMP_THREADS ? SMP_THREADS : __builtin_popcountl(mask);
		long tenths = smp_speedup(threads);

		printf("smp speedup: %ld.%ld with %d threads\n", tenths / 10, tenths % 10, threads);
	}
	/* fork from a process whose other threads run */
	for (index = 0; index < 4; index++)
		pthread_create(&workers[index], NULL, smp_spinner, NULL);
	for (index = 0; index < 20; index++) {
		pid_t child;

		fflush(stdout);
		child = fork();
		if (child == 0)
			_exit(smp_lazy[0] == 1 ? 3 : 4);
		if (child < 0 || wait4(child, &status, 0, NULL) != child || !WIFEXITED(status) || WEXITSTATUS(status) != 3) {
			printf("smp failed: fork from a running process\n");
			return 1;
		}
	}
	printf("smp lifecycle: 20 forks beside running threads\n");
	if (smp_exec_with_threads() != 0) {
		printf("smp failed: exec with running threads\n");
		return 1;
	}
	printf("smp exec: threads stopped\n");
	printf("smp ok\n");
	fflush(stdout);
	/* The spinners end with the process. */
	_exit(0);
}

int main(int argc, char **argv)
{
	const char *mode = getenv("GOOS_LINUX_PROBE");
	int failed = 0;

	if (argc >= 2) {
		struct timespec minute = {60, 0};
		char byte;

		if (!strcmp(argv[1], "hostile"))
			return hostile();
		if (!strcmp(argv[1], "exhaust"))
			return exhaust();
		if (!strcmp(argv[1], "isolation"))
			return isolation();
		if (!strcmp(argv[1], "port-io"))
			return port_io();
		if (!strcmp(argv[1], "pair") && argc >= 3)
			return pair(argv[2], argc > 3);
		if (!strcmp(argv[1], "bench"))
			return bench();
		if (!strcmp(argv[1], "session-loss"))
			return session_loss();
		if (!strcmp(argv[1], "session-timeout"))
			return session_timeout();
		if (!strcmp(argv[1], "exec"))
			return exec_test();
		if (!strcmp(argv[1], "fork"))
			return fork_test();
		if (!strcmp(argv[1], "vfork"))
			return vfork_test();
		if (!strcmp(argv[1], "orphan"))
			return orphan_test();
		if (!strcmp(argv[1], "wait"))
			return wait_test();
		if (!strcmp(argv[1], "pipe"))
			return pipe_test();
		if (!strcmp(argv[1], "signal"))
			return signal_test();
		if (!strcmp(argv[1], "kill"))
			return kill_test();
		if (!strcmp(argv[1], "eintr"))
			return eintr_test();
		if (!strcmp(argv[1], "poll"))
			return poll_test();
		if (!strcmp(argv[1], "ppoll"))
			return ppoll_test();
		if (!strcmp(argv[1], "family"))
			return family_test();
		if (!strcmp(argv[1], "run") && argc >= 3) {
			execv(argv[2], argv + 2);
			printf("run failed: errno=%d\n", errno);
			return 1;
		}
		if (!strcmp(argv[1], "hardening"))
			return hardening_test();
		if (!strcmp(argv[1], "limits"))
			return limits_test();
		if (!strcmp(argv[1], "fork-fault"))
			return fork_fault_test();
		if (!strcmp(argv[1], "libc"))
			return libc_test();
		if (!strcmp(argv[1], "threads"))
			return threads_test();
		if (!strcmp(argv[1], "smp"))
			return smp_test();
		if (!strcmp(argv[1], "kernel"))
			return kernel_test();
		if (!strcmp(argv[1], "domains"))
			return domains_test(argc > 2 ? argv[2] : "a");
		if (!strcmp(argv[1], "fpstate"))
			return fpstate_test();
		if (!strcmp(argv[1], "exec-vfs-loss"))
			return exec_vfs_loss_test();
		if (!strcmp(argv[1], "path")) {
			printf("path %s\n", getenv("PATH") ? getenv("PATH") : "(none)");
			return 0;
		}
		if (!strcmp(argv[1], "after-exec"))
			return after_exec(argc, argv);
		if (!strcmp(argv[1], "stack-overflow"))
			return stack_overflow();
		if (!strcmp(argv[1], "random")) {
			int source = random_source();

			printf("random: %s\n", source == 1 ? "available" : source == 0 ? "none" : "inconsistent");
			return source != 1;
		}
		if (!strcmp(argv[1], "block")) {
			puts("blocking on standard input");
			fflush(stdout);
			while (read(0, &byte, 1) > 0)
				;
			puts("standard input ended");
			return 0;
		}
		if (!strcmp(argv[1], "sleep")) {
			puts("sleeping");
			fflush(stdout);
			nanosleep(&minute, NULL);
			puts("sleep ended");
			return 0;
		}
	}
	printf("argc=%d\n", argc);
	for (int index = 1; index < argc; index++)
		printf("argv[%d]=%s\n", index, argv[index]);
	if (argv[argc] != NULL || !mode || strcmp(mode, "musl")) {
		puts("env failed");
		failed = 1;
	}

	tls_data += 2;
	tls_zero += 7;
	errno = ENOENT;
	if (tls_data != 42 || tls_zero != 7 || errno != ENOENT) {
		puts("tls failed");
		failed = 1;
	}
	if (!heap_ok()) {
		puts("heap failed");
		failed = 1;
	}
	if (session_ok(argv[0])) {
		puts("vfs session ok");
	} else {
		puts("vfs session failed");
		failed = 1;
	}
	if (paths_ok()) {
		puts("paths ok");
	} else {
		puts("paths failed");
		failed = 1;
	}
	failed += scratch_checks(argv[0]);
	failed += report("devices", devices_ok(on_goos(), strict_kernel()));
	failed += report("system", system_ok());
	failed += report("signals", signals_ok(strict_kernel()));
	failed += report("random", random_ok(on_goos()));
	if (!failed)
		puts("abi-probe ok");
	return failed;
}
