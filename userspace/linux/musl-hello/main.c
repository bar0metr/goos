/*
 * Static musl startup probe. It reaches main through crt1 and
 * __libc_start_main, then checks the pieces of the Linux process ABI that
 * libc startup depends on: the argument and environment vectors, the TLS
 * image behind FS, the brk and mmap heaps, and buffered stdio flushed at exit.
 * It then opens and closes its own image, which is one request and one reply
 * on the VFS session PID 1 granted, and names that image through the working
 * directory in several spellings to check path resolution.
 *
 * GoOS starts it as /system/linux/musl-hello with /system/linux as the
 * working directory. GOOS_LINUX_LONG, when set, names an existing file whose
 * path is too long for one request message.
 */
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

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
	    !opens(1, "/system/linux/musl-hello"))
		return 0;
	if (!refuses(AT_FDCWD, "", ENOENT) ||
	    !refuses(1, "musl-hello", ENOTDIR) ||
	    !refuses(42, "musl-hello", EBADF))
		return 0;
	if (existing && !opens(AT_FDCWD, existing))
		return 0;
	/* 300 bytes fit a file name and no request message; 1100 bytes fit
	 * neither. */
	memset(name, 'n', sizeof name - 1);
	name[300] = 0;
	if (!refuses(AT_FDCWD, name, ENOENT))
		return 0;
	name[300] = 'n';
	name[1100] = 0;
	return refuses(AT_FDCWD, name, ENAMETOOLONG);
}

int main(int argc, char **argv)
{
	const char *mode = getenv("GOOS_LINUX_PROBE");
	int failed = 0;

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
	if (!failed)
		puts("hello from musl");
	return failed;
}
