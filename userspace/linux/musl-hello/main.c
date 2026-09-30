/*
 * Static musl startup probe. It reaches main through crt1 and
 * __libc_start_main, then checks the pieces of the Linux process ABI that
 * libc startup depends on: the argument and environment vectors, the TLS
 * image behind FS, the brk and mmap heaps, and buffered stdio flushed at exit.
 * It then opens and closes its own image, which is one request and one reply
 * on the VFS session PID 1 granted, and names that image through the working
 * directory in several spellings to check path resolution.
 *
 * Last it works in a scratch directory under /tmp: it creates, writes,
 * seeks, reads, appends to, truncates, stats, renames, and removes files and
 * directories, reads and writes one through stdio, and checks the errno of
 * each refusal.
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
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/uio.h>
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
	if (!failed)
		puts("hello from musl");
	return failed;
}
