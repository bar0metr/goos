/*
 * Directory proof: dirprobe DIR lists DIR with getdents64 and prints each
 * name, a directory with a trailing slash, then the count. The bundled musl
 * has no readdir, so the program reads the records itself.
 */
#define _GNU_SOURCE
#include <dirent.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/syscall.h>
#include <unistd.h>

struct listed {
	unsigned long long inode;
	long long next;
	unsigned short length;
	unsigned char type;
	char name[];
};

int main(int argc, char **argv)
{
	static char buffer[4096];
	int fd, count = 0;
	long got;

	if (argc != 2) {
		puts("usage: dirprobe DIRECTORY");
		return 2;
	}
	fd = open(argv[1], O_RDONLY | O_DIRECTORY);
	if (fd < 0) {
		puts("dirprobe: cannot open directory");
		return 1;
	}
	while ((got = syscall(SYS_getdents64, fd, buffer, sizeof buffer)) > 0) {
		for (long at = 0; at < got;) {
			struct listed *entry = (void *)(buffer + at);

			at += entry->length;
			if (!strcmp(entry->name, ".") || !strcmp(entry->name, ".."))
				continue;
			printf("%s%s\n", entry->name, entry->type == DT_DIR ? "/" : "");
			count++;
		}
	}
	if (got < 0 || close(fd) != 0) {
		puts("dirprobe: listing failed");
		return 1;
	}
	printf("dirprobe ok: %d entries\n", count);
	return 0;
}
