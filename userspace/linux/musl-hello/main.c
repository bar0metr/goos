/*
 * Static musl startup probe. It reaches main through crt1 and
 * __libc_start_main, then checks the pieces of the Linux process ABI that
 * libc startup depends on: the argument and environment vectors, the TLS
 * image behind FS, the brk and mmap heaps, and buffered stdio flushed at exit.
 * It then opens and closes its own image, which is one request and one reply
 * on the VFS session PID 1 granted.
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
	if (!failed)
		puts("hello from musl");
	return failed;
}
