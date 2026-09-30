/*
 * Static musl startup probe. It reaches main through crt1 and
 * __libc_start_main, then checks the pieces of the Linux process ABI that
 * libc startup depends on: the argument and environment vectors, the TLS
 * image behind FS, the brk and mmap heaps, and buffered stdio flushed at exit.
 * It then opens and closes its own image, which is one request and one reply
 * on the VFS session PID 1 granted, and names that image through the working
 * directory in several spellings to check path resolution.
 *
 * It works in a scratch directory under /tmp: it creates, writes, seeks,
 * reads, appends to, truncates, stats, lists, renames, and removes files and
 * directories, reads and writes them through stdio and through vectors, and
 * checks the errno of each refusal. Last it reads its identity, the system
 * name, the clocks, and a limit, and sleeps.
 *
 * GoOS starts it as /system/linux/musl-hello with /system/linux as the
 * working directory. GOOS_LINUX_LONG, when set, names an existing file whose
 * path is too long for one request message; GoOS alone sets it, so it also
 * marks the run whose program image sits on a read-only mount.
 *
 * Two more modes serve the GoOS boot test. "musl-hello session-loss" waits
 * for end of input, then reports whether its VFS session still opens files.
 * "musl-hello session-timeout" opens a name the validation VFS answers late
 * and expects the wait to end with ETIMEDOUT and the session to keep working.
 */
#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/uio.h>
#include <sys/utsname.h>
#include <time.h>
#include <unistd.h>

#define IMAGE "/system/linux/musl-hello"

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
	int fd = open("musl-hello", O_RDONLY);

	if (fd != 3 || close(fd) != 0)
		return 0;
	if (!opens(AT_FDCWD, "./musl-hello") ||
	    !opens(AT_FDCWD, "..//linux/./musl-hello") ||
	    !opens(AT_FDCWD, "../../../system/linux/musl-hello") ||
	    !opens(1, IMAGE))
		return 0;
	if (!refuses(AT_FDCWD, "", ENOENT) ||
	    !refuses(1, "musl-hello", ENOTDIR) ||
	    !refuses(42, "musl-hello", EBADF))
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
	errno = 0;
	return fails(syscall(SYS_stat, "missing", &other), ENOENT) &&
	       fails(syscall(SYS_stat, "data/", &other), ENOTDIR) &&
	       fails(syscall(SYS_fstat, 42, &other), EBADF) &&
	       fails(syscall(SYS_fstat, AT_FDCWD, &other), EBADF) &&
	       fails(syscall(SYS_newfstatat, AT_FDCWD, "data", &other, 0x10000000), EINVAL) &&
	       fails(syscall(SYS_stat, "data", (void *)8), EFAULT);
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
	    getuid() != geteuid() || getgid() != getegid())
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
	    !fails(rmdir("../linux-files"), ENOTEMPTY) ||
	    unlink("moved") != 0 || close(dirfd) != 0)
		return 0;
	return !read_only_image || fails(make_directory(AT_FDCWD, IMAGE "-dir"), EROFS);
}

static int report(const char *what, int ok)
{
	printf("%s %s\n", what, ok ? "ok" : "failed");
	return !ok;
}

/* Returns how many of its checks failed. */
static int scratch_checks(const char *self)
{
	const char *scratch = "/tmp/linux-files";
	int read_only_image = getenv("GOOS_LINUX_LONG") != NULL;
	char cwd[64];
	int failed;

	errno = 0;
	if (make_directory(AT_FDCWD, scratch) != 0 ||
	    !fails(make_directory(AT_FDCWD, scratch), EEXIST) ||
	    !fails(chdir(self), ENOTDIR) || chdir(scratch) != 0 ||
	    !getcwd(cwd, sizeof cwd) || strcmp(cwd, scratch) ||
	    getcwd(cwd, 4) || !fails(-1, ERANGE))
		return report("scratch directory", 0);
	failed = report("files", data_ok());
	failed += report("status", status_ok(self, read_only_image));
	failed += report("stdio", stdio_ok());
	failed += report("listing", listing_ok());
	failed += report("vectors", vectors_ok(read_only_image));
	failed += report("names", names_ok(read_only_image));
	if (chdir("/system/linux") != 0 || rmdir(scratch) != 0)
		failed += report("scratch cleanup", 0);
	return failed;
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

int main(int argc, char **argv)
{
	const char *mode = getenv("GOOS_LINUX_PROBE");
	int failed = 0;

	if (argc == 2 && !strcmp(argv[1], "session-loss"))
		return session_loss();
	if (argc == 2 && !strcmp(argv[1], "session-timeout"))
		return session_timeout();
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
	failed += report("system", system_ok());
	if (!failed)
		puts("hello from musl");
	return failed;
}
