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
 *
 * exhaust, isolation, and port-io make sense on GoOS only.
 */
#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
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
#include <sys/sysmacros.h>
#include <sys/uio.h>
#include <sys/utsname.h>
#include <sys/wait.h>
#include <time.h>
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
 * back. No check here raises a signal: GoOS delivers none. An emulator that
 * keeps the signal table itself leaves SIGKILL and SIGSTOP in a handler's
 * mask, so only a strict run requires them gone. */
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

/* 1 when the kernel hands out random bytes, 0 when it has no source and says
 * so in both places a program looks, -1 when the two disagree. */
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

/* Linux always has a source. GoOS has one when the CPU has a generator; a
 * machine without it must refuse the call and leave AT_RANDOM out. */
static int random_ok(int goos)
{
	static unsigned char large[300];
	unsigned char first[32], second[32], *start;
	int source = random_source(), zero = 0;
	long got;

	errno = 0;
	if (source != 1)
		return goos && source == 0;
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
			 (!goos || fails(open("data", O_RDONLY | O_NONBLOCK), EOPNOTSUPP)) &&
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
 * watches the position as fork_test does. GoOS refuses the thread form of
 * clone; Linux would create a thread, so that check runs on GoOS only. */
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
		if (!strcmp(argv[1], "after-exec"))
			return after_exec(argc, argv);
		if (!strcmp(argv[1], "stack-overflow"))
			return stack_overflow();
		if (!strcmp(argv[1], "random")) {
			int source = random_source();

			printf("random: %s\n", source == 1 ? "hardware" : source == 0 ? "none" : "inconsistent");
			return source < 0;
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
