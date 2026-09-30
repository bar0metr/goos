/*
 * Filesystem proof: fsprobe PATH creates PATH, writes a line, reads it back,
 * and compares what stat says of the name with what fstat says of the open
 * file. It prints one line per step and leaves the file in place.
 */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

static const char text[] = "written by a Linux program\n";

static int step(const char *name, int ok)
{
	if (ok)
		printf("%s ok\n", name);
	else
		printf("%s failed: errno %d\n", name, errno);
	return ok;
}

int main(int argc, char **argv)
{
	struct stat by_name, by_descriptor;
	char back[sizeof text];
	size_t size = sizeof text - 1;
	int fd;

	if (argc != 2) {
		puts("usage: fsprobe PATH");
		return 2;
	}
	fd = open(argv[1], O_RDWR | O_CREAT | O_TRUNC, 0644);
	if (!step("write", fd >= 0 && write(fd, text, size) == (ssize_t)size))
		return 1;
	if (!step("read", lseek(fd, 0, SEEK_SET) == 0 &&
			  read(fd, back, sizeof back) == (ssize_t)size &&
			  !memcmp(back, text, size) && read(fd, back, 1) == 0))
		return 1;
	/* The bundled musl has no stat wrappers; these are the calls they make. */
	if (!step("stat", syscall(SYS_stat, argv[1], &by_name) == 0 &&
			  syscall(SYS_fstat, fd, &by_descriptor) == 0 &&
			  S_ISREG(by_name.st_mode) && by_name.st_size == (off_t)size &&
			  by_name.st_ino == by_descriptor.st_ino &&
			  by_name.st_dev == by_descriptor.st_dev))
		return 1;
	return close(fd) == 0 ? 0 : 1;
}
