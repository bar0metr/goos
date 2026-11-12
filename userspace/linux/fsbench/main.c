/*
 * fsbench DIR [SCALE] times file system calls under DIR and prints one
 * "bench NAME key=value ..." line per workload, then "bench ok". The same
 * binary runs on GoOS and on Linux, so the two can be compared on the same
 * machine. SCALE, 1 to 16, multiplies the file counts and the file size.
 *
 *   create      2000*SCALE empty files in one directory, then fsync of it
 *   stat        stat of each of those files
 *   readdir     getdents64 over the directory to its end
 *   unlink      unlink of each file, then fsync of the directory
 *   seq-write   64*SCALE MiB in 1 MiB writes, then fsync
 *   seq-read    the same file in 1 MiB reads
 *   rand-write  2000 aligned 4 KiB pwrites at seeded offsets, then fsync
 *   rand-read   2000 aligned 4 KiB preads at seeded offsets
 *   fsync-4k    200 rounds of a 4 KiB pwrite at offset 0 and fsync
 *   replace     200 rounds of write a temporary file, fsync, rename it
 *               over a fixed name
 *   sparse      one byte at 5 GiB in a new file, read back
 *   parallel    1, 2, 4 and 8 processes, each in its own directory doing
 *               200*SCALE rounds of create, write 4 KiB, close, unlink
 *
 * fsbench works in DIR/fsbench.PID and removes it at the end. A failed call
 * prints "fsbench: STEP: errno N" on stderr and exits with status 1. Times
 * come from CLOCK_MONOTONIC and include the fsync calls named above.
 *
 * The bundled musl has no stat, mkdir, or directory sources, so the program
 * makes those calls through syscall().
 */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define MIB (1L << 20)
#define BLOCK 4096
#define RANDOM_OPS 2000
#define SYNC_OPS 200
#define REPLACE_OPS 200
#define SPARSE_OFFSET (5LL << 30)

struct listed {
	unsigned long long inode;
	long long next;
	unsigned short length;
	unsigned char type;
	char name[];
};

static unsigned char buffer[MIB];
static char listing[32768];
static char base[512];
static int failed;

static void die(const char *step)
{
	fprintf(stderr, "fsbench: %s: errno %d\n", step, errno);
	exit(1);
}

static void join(char *out, const char *dir, const char *name)
{
	if (snprintf(out, 512, "%s/%s", dir, name) >= 512) {
		errno = ENAMETOOLONG;
		die("path");
	}
}

static void numbered(char *out, const char *dir, int index)
{
	char name[16];

	snprintf(name, sizeof name, "f%05d", index);
	join(out, dir, name);
}

static long long now_us(void)
{
	struct timespec time;

	if (clock_gettime(CLOCK_MONOTONIC, &time) != 0)
		die("clock_gettime");
	return time.tv_sec * 1000000LL + time.tv_nsec / 1000LL;
}

/* tenths prints num/den with one decimal, rounded. */
static void tenths(char *out, long long num, long long den)
{
	long long value = den ? (num * 10 + den / 2) / den : 0;

	snprintf(out, 32, "%lld.%lld", value / 10, value % 10);
}

static void report_ops(const char *name, long long ops, long long elapsed)
{
	char per[32];

	tenths(per, elapsed, ops);
	printf("bench %s ops=%lld us=%lld us_per_op=%s\n", name, ops, elapsed, per);
}

static void report_rate(const char *name, long long bytes, long long elapsed)
{
	char rate[32];

	tenths(rate, bytes * 1000000LL / MIB, elapsed);
	printf("bench %s bytes=%lld us=%lld mib_s=%s\n", name, bytes, elapsed, rate);
}

static void make_dir(const char *path)
{
	if (syscall(SYS_mkdir, path, 0755) != 0)
		die("mkdir");
}

static void sync_dir(const char *path, const char *step)
{
	int fd = open(path, O_RDONLY | O_DIRECTORY);

	if (fd < 0 || fsync(fd) != 0 || close(fd) != 0)
		die(step);
}

static void write_full(int fd, const unsigned char *data, long size, const char *step)
{
	while (size > 0) {
		ssize_t n = write(fd, data, (size_t)size);

		if (n <= 0) {
			if (n == 0)
				errno = EIO;
			die(step);
		}
		data += n;
		size -= n;
	}
}

static unsigned long long seed;

static long long next_block(long long blocks)
{
	seed = seed * 6364136223846793005ULL + 1442695040888963407ULL;
	return (long long)((seed >> 33) % (unsigned long long)blocks);
}

static void bench_files(int files)
{
	char dir[512], path[512];
	struct stat st;
	long long start;
	long got;
	int fd, entries = 0;

	join(dir, base, "files");
	make_dir(dir);

	start = now_us();
	for (int i = 0; i < files; i++) {
		numbered(path, dir, i);
		fd = open(path, O_WRONLY | O_CREAT | O_EXCL, 0644);
		if (fd < 0 || close(fd) != 0)
			die("create");
	}
	sync_dir(dir, "create fsync");
	report_ops("create", files, now_us() - start);

	start = now_us();
	for (int i = 0; i < files; i++) {
		numbered(path, dir, i);
		if (syscall(SYS_stat, path, &st) != 0)
			die("stat");
		if (!S_ISREG(st.st_mode) || st.st_size != 0) {
			errno = EIO;
			die("stat result");
		}
	}
	report_ops("stat", files, now_us() - start);

	start = now_us();
	fd = open(dir, O_RDONLY | O_DIRECTORY);
	if (fd < 0)
		die("readdir open");
	while ((got = syscall(SYS_getdents64, fd, listing, sizeof listing)) > 0) {
		for (long at = 0; at < got;) {
			struct listed *entry = (void *)(listing + at);

			at += entry->length;
			if (strcmp(entry->name, ".") && strcmp(entry->name, ".."))
				entries++;
		}
	}
	if (got < 0 || close(fd) != 0)
		die("readdir");
	printf("bench readdir entries=%d us=%lld\n", entries, now_us() - start);
	if (entries != files) {
		errno = EIO;
		die("readdir count");
	}

	start = now_us();
	for (int i = 0; i < files; i++) {
		numbered(path, dir, i);
		if (unlink(path) != 0)
			die("unlink");
	}
	sync_dir(dir, "unlink fsync");
	report_ops("unlink", files, now_us() - start);
	if (rmdir(dir) != 0)
		die("rmdir files");
}

static void bench_data(long size)
{
	char path[512];
	long long start, blocks = size / BLOCK, total = 0;
	ssize_t n;
	int fd;

	for (long i = 0; i < MIB; i++)
		buffer[i] = (unsigned char)(i * 131 + 7);
	join(path, base, "data");

	fd = open(path, O_RDWR | O_CREAT | O_TRUNC, 0644);
	if (fd < 0)
		die("seq-write open");
	start = now_us();
	for (long done = 0; done < size; done += MIB)
		write_full(fd, buffer, MIB, "seq-write");
	if (fsync(fd) != 0)
		die("seq-write fsync");
	report_rate("seq-write", size, now_us() - start);
	if (close(fd) != 0)
		die("seq-write close");

	fd = open(path, O_RDONLY);
	if (fd < 0)
		die("seq-read open");
	start = now_us();
	while ((n = read(fd, buffer, MIB)) > 0)
		total += n;
	if (n < 0)
		die("seq-read");
	report_rate("seq-read", total, now_us() - start);
	if (close(fd) != 0)
		die("seq-read close");
	if (total != size) {
		errno = EIO;
		die("seq-read size");
	}

	fd = open(path, O_RDWR);
	if (fd < 0)
		die("rand-write open");
	seed = 1;
	start = now_us();
	for (int i = 0; i < RANDOM_OPS; i++)
		if (pwrite(fd, buffer, BLOCK, next_block(blocks) * BLOCK) != BLOCK)
			die("rand-write");
	if (fsync(fd) != 0)
		die("rand-write fsync");
	report_ops("rand-write", RANDOM_OPS, now_us() - start);

	seed = 2;
	start = now_us();
	for (int i = 0; i < RANDOM_OPS; i++)
		if (pread(fd, buffer, BLOCK, next_block(blocks) * BLOCK) != BLOCK)
			die("rand-read");
	report_ops("rand-read", RANDOM_OPS, now_us() - start);
	if (close(fd) != 0 || unlink(path) != 0)
		die("data remove");
}

static void bench_sync(void)
{
	char path[512];
	long long start;
	int fd;

	join(path, base, "sync");
	fd = open(path, O_RDWR | O_CREAT | O_TRUNC, 0644);
	if (fd < 0)
		die("fsync-4k open");
	start = now_us();
	for (int i = 0; i < SYNC_OPS; i++) {
		if (pwrite(fd, buffer, BLOCK, 0) != BLOCK)
			die("fsync-4k write");
		if (fsync(fd) != 0)
			die("fsync-4k fsync");
	}
	report_ops("fsync-4k", SYNC_OPS, now_us() - start);
	if (close(fd) != 0 || unlink(path) != 0)
		die("fsync-4k remove");
}

static void bench_replace(void)
{
	char temporary[512], target[512];
	long long start;
	int fd;

	join(temporary, base, "replace.tmp");
	join(target, base, "replace");
	start = now_us();
	for (int i = 0; i < REPLACE_OPS; i++) {
		fd = open(temporary, O_WRONLY | O_CREAT | O_TRUNC, 0644);
		if (fd < 0)
			die("replace open");
		write_full(fd, buffer, BLOCK, "replace write");
		if (fsync(fd) != 0)
			die("replace fsync");
		if (close(fd) != 0)
			die("replace close");
		if (syscall(SYS_rename, temporary, target) != 0)
			die("replace rename");
	}
	report_ops("replace", REPLACE_OPS, now_us() - start);
	if (unlink(target) != 0)
		die("replace remove");
}

static const char *skip_name(int code)
{
	return code == EFBIG ? "EFBIG" : code == ENOSPC ? "ENOSPC" : NULL;
}

/* sparse_fail reports a sparse result Linux and GoOS may differ on; the run
 * goes on and ends without "bench ok". */
static int sparse_fail(int code)
{
	printf("bench sparse ok=0 errno=%d\n", code);
	failed = 1;
	return 0;
}

static int bench_sparse(void)
{
	char path[512];
	unsigned char byte = 0x5a, back = 0;
	struct stat st;
	long long start;
	int fd;

	join(path, base, "sparse");
	fd = open(path, O_RDWR | O_CREAT | O_TRUNC, 0644);
	if (fd < 0)
		die("sparse open");
	start = now_us();
	if (pwrite(fd, &byte, 1, SPARSE_OFFSET) != 1) {
		int code = errno;

		if (close(fd) != 0 || unlink(path) != 0)
			die("sparse remove");
		if (skip_name(code)) {
			printf("bench sparse skipped=%s\n", skip_name(code));
			return 1;
		}
		return sparse_fail(code);
	}
	if (syscall(SYS_fstat, fd, &st) != 0)
		die("sparse fstat");
	if (pread(fd, &back, 1, SPARSE_OFFSET) != 1)
		die("sparse pread");
	if (close(fd) != 0 || unlink(path) != 0)
		die("sparse remove");
	if (st.st_size != SPARSE_OFFSET + 1 || back != byte)
		return sparse_fail(0);
	printf("bench sparse ok=1 size=%lld blocks=%lld us=%lld\n", (long long)st.st_size,
	       (long long)st.st_blocks, now_us() - start);
	return 1;
}

static void worker(const char *dir, int rounds)
{
	char path[512];
	int fd;

	join(path, dir, "f");
	for (int i = 0; i < rounds; i++) {
		fd = open(path, O_WRONLY | O_CREAT | O_TRUNC, 0644);
		if (fd < 0)
			die("parallel open");
		write_full(fd, buffer, BLOCK, "parallel write");
		if (close(fd) != 0)
			die("parallel close");
		if (unlink(path) != 0)
			die("parallel unlink");
	}
}

static void bench_parallel(int procs, int rounds)
{
	char group[512], dirs[8][512], name[16];
	pid_t children[8];
	long long start, elapsed, ops = (long long)procs * rounds;
	int started = 0, status, refused = 0;
	char per[32], rate[32];

	snprintf(name, sizeof name, "p%d", procs);
	join(group, base, name);
	make_dir(group);
	for (int i = 0; i < procs; i++) {
		snprintf(name, sizeof name, "w%d", i);
		join(dirs[i], group, name);
		make_dir(dirs[i]);
	}
	fflush(stdout);
	start = now_us();
	for (; started < procs; started++) {
		children[started] = fork();
		if (children[started] < 0) {
			refused = errno;
			break;
		}
		if (children[started] == 0) {
			worker(dirs[started], rounds);
			_exit(0);
		}
	}
	for (int i = 0; i < started; i++) {
		if (waitpid(children[i], &status, 0) != children[i])
			die("parallel wait");
		if (!WIFEXITED(status) || WEXITSTATUS(status) != 0) {
			errno = ECHILD;
			die("parallel worker");
		}
	}
	elapsed = now_us() - start;
	for (int i = 0; i < procs; i++)
		if (rmdir(dirs[i]) != 0)
			die("parallel rmdir");
	if (rmdir(group) != 0)
		die("parallel rmdir");
	if (refused == ENOSYS || refused == EAGAIN) {
		printf("bench parallel procs=%d skipped=%s\n", procs, refused == ENOSYS ? "ENOSYS" : "EAGAIN");
		return;
	}
	if (refused) {
		errno = refused;
		die("parallel fork");
	}
	tenths(per, elapsed, ops);
	tenths(rate, ops * 1000000LL, elapsed);
	printf("bench parallel procs=%d ops=%lld us=%lld us_per_op=%s ops_s=%s\n", procs, ops, elapsed, per,
	       rate);
}

int main(int argc, char **argv)
{
	struct stat st;
	char *end;
	long scale = 1;

	if (argc == 3) {
		scale = strtol(argv[2], &end, 10);
		if (*end != 0)
			scale = 0;
	}
	if (argc < 2 || argc > 3 || scale < 1 || scale > 16) {
		fputs("usage: fsbench DIR [SCALE]  (SCALE 1 to 16)\n", stderr);
		return 2;
	}
	if (syscall(SYS_stat, argv[1], &st) != 0)
		die("stat DIR");
	if (!S_ISDIR(st.st_mode)) {
		errno = ENOTDIR;
		die("stat DIR");
	}
	if (snprintf(base, sizeof base, "%s/fsbench.%d", argv[1], (int)getpid()) >= (int)sizeof base) {
		errno = ENAMETOOLONG;
		die("path");
	}
	make_dir(base);
	printf("bench start dir=%s scale=%ld\n", base, scale);

	bench_files(2000 * (int)scale);
	bench_data(64 * MIB * scale);
	bench_sync();
	bench_replace();
	bench_sparse();
	for (int procs = 1; procs <= 8; procs *= 2)
		bench_parallel(procs, 200 * (int)scale);

	if (rmdir(base) != 0)
		die("rmdir");
	if (failed) {
		puts("bench failed");
		return 1;
	}
	puts("bench ok");
	return 0;
}
