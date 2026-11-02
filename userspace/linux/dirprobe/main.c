/*
 * Directory proof: dirprobe DIR [SIZE] lists DIR with getdents64 and prints
 * each name, a directory with a trailing slash, then the most entries one call
 * returned and the count. SIZE is the caller's buffer, at most 4096 bytes, and
 * giving it leaves out the names. The bundled musl has no readdir, so the
 * program reads the records itself.
 */
#define _GNU_SOURCE
#include <dirent.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
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
	int fd, count = 0, most = 0;
	long got, size = sizeof buffer;

	if (argc == 3)
		size = atol(argv[2]);
	if (argc < 2 || argc > 3 || size <= 0 || size > (long)sizeof buffer) {
		puts("usage: dirprobe DIRECTORY [SIZE]");
		return 2;
	}
	fd = open(argv[1], O_RDONLY | O_DIRECTORY);
	if (fd < 0) {
		puts("dirprobe: cannot open directory");
		return 1;
	}
	while ((got = syscall(SYS_getdents64, fd, buffer, size)) > 0) {
		int returned = 0;

		for (long at = 0; at < got;) {
			struct listed *entry = (void *)(buffer + at);

			at += entry->length;
			if (!strcmp(entry->name, ".") || !strcmp(entry->name, ".."))
				continue;
			if (argc == 2)
				printf("%s%s\n", entry->name, entry->type == DT_DIR ? "/" : "");
			returned++;
		}
		count += returned;
		if (returned > most)
			most = returned;
	}
	if (got < 0 || close(fd) != 0) {
		puts("dirprobe: listing failed");
		return 1;
	}
	printf("dirprobe largest call: %d entries\n", most);
	printf("dirprobe ok: %d entries\n", count);
	return 0;
}
