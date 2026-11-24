/* Restart errors follow the Linux VFS mapping: EIO for a lost backend,
 * EBADF for a revoked descriptor, ENOENT for a retired VFS handle. Recovery
 * opens may also return EAGAIN or EINTR. Timeout errors fail this workload.
 * Each child checks four 4 KiB pages after a fresh open, including zero padding. */
#define EXT4_RESTART_READERS 4

static int ext4_restart_failure(const char *stage, int index, long count)
{
	int error = errno;
	printf("ext4 restart failed stage=%s reader=%d errno=%d n=%ld\n", stage, index, error, count);
	fflush(stdout);
	return 1;
}

static int ext4_restart_metadata(const char *stage, int index, const struct stat *metadata)
{
	printf("ext4 restart metadata size=%lld mode=%o nlink=%lu\n", (long long)metadata->st_size,
	       (unsigned)metadata->st_mode, (unsigned long)metadata->st_nlink);
	return ext4_restart_failure(stage, index, 0);
}

static int ext4_restart_transient(int error)
{
	return error == EIO || error == EBADF || error == ENOENT || error == EAGAIN || error == EINTR;
}

static int ext4_restart_reader(const char *path, const char *expected, int ready, int go, int index)
{
	char data[4096], wanted[4096] = {0}, byte;
	struct timespec pause = {0, 100000000};
	size_t length = strlen(expected);
	int fd = open(path, O_RDONLY), fresh;
	ssize_t count;
	struct stat metadata;

	if (fd < 0)
		return ext4_restart_failure("reader-open", index, fd);
	if (syscall(SYS_fstat, fd, &metadata))
		return ext4_restart_failure("reader-fstat", index, -1);
	if (metadata.st_size != 1024 * 1024 + 4096 || !S_ISREG(metadata.st_mode) || metadata.st_nlink != 1)
		return ext4_restart_metadata("reader-metadata", index, &metadata);
	if (length == 0 || length >= sizeof data)
		return ext4_restart_failure("reader-expected-length", index, length);
	memcpy(wanted, expected, length);
	wanted[length] = '\n';
	wanted[4095] = index + 1;
	for (int turn = 0; turn < 4; turn++) {
		count = pread(fd, data, sizeof data, turn * 4096);
		if (count != sizeof data || memcmp(data, wanted, sizeof data))
			return ext4_restart_failure("reader-warm-data", index, count);
	}
	count = write(ready, "r", 1);
	if (count != 1)
		return ext4_restart_failure("reader-ready-write", index, count);
	count = read(go, &byte, 1);
	if (count != 1)
		return ext4_restart_failure("reader-go-read", index, count);
	/* The service retains this reserved read until the backing endpoint dies. */
	for (;;) {
		count = pread(fd, data, sizeof data, 1024 * 1024);
		if (count < 0)
			break;
		if (count != sizeof data || memcmp(data, wanted, sizeof data))
			return ext4_restart_failure("reader-sentinel-data", index, count);
	}
	if (errno != EIO && errno != EBADF && errno != ENOENT)
		return ext4_restart_failure("reader-revocation-errno", index, count);
	printf("ext4 restart reader %d revoked errno=%d\n", index, errno);
	fflush(stdout);
	count = pread(fd, data, 256, 0);
	if (count >= 0 || (errno != EIO && errno != EBADF && errno != ENOENT))
		return ext4_restart_failure("reader-stale-descriptor", index, count);
	close(fd);
	for (int attempt = 0; attempt < 350; attempt++) {
		fresh = open(path, O_RDONLY);
		if (fresh >= 0) {
			count = pread(fresh, data, 256, 0);
			int error = errno;
			if (count == 256 && !memcmp(data, wanted, 256)) {
				if (syscall(SYS_fstat, fresh, &metadata))
					return ext4_restart_failure("reader-fresh-fstat", index, -1);
				if (metadata.st_size != 1024 * 1024 + 4096 || !S_ISREG(metadata.st_mode) || metadata.st_nlink != 1)
					return ext4_restart_metadata("reader-fresh-metadata", index, &metadata);
				for (int page = 0; page < 4; page++) {
					count = pread(fresh, data, sizeof data, page * 4096);
					if (count != sizeof data || memcmp(data, wanted, sizeof data))
						return ext4_restart_failure("reader-fresh-data", index, count);
				}
				if (close(fresh))
					return ext4_restart_failure("reader-fresh-close", index, -1);
				printf("ext4 restart reader %d fresh data ok\n", index);
				fflush(stdout);
				return 0;
			}
			close(fresh);
			if (count >= 0 || !ext4_restart_transient(error)) {
				errno = error;
				return ext4_restart_failure("reader-fresh-prefix", index, count);
			}
		} else if (!ext4_restart_transient(errno)) {
			return ext4_restart_failure("reader-fresh-open", index, fresh);
		}
		nanosleep(&pause, NULL);
	}
	return ext4_restart_failure("reader-recovery-timeout", index, 350);
}

/* Four processes own independent opens. The parent bounds startup and recovery. */
static int ext4_restart_test(const char *path, const char *expected)
{
	int ready[2], go[2], status = 0, failed = 0, done = 0;
	pid_t children[EXT4_RESTART_READERS] = {0};
	struct timespec start, now, pause = {0, 25000000};
	char bytes[EXT4_RESTART_READERS];
	int warmed = 0;
	char reader_path[EXT4_RESTART_READERS][512], seed[4096] = {0};
	if (strlen(expected) >= sizeof seed - 1)
		return ext4_restart_failure("seed-expected-length", -1, strlen(expected));
	memcpy(seed, expected, strlen(expected));
	seed[strlen(expected)] = '\n';
	for (int index = 0; index < EXT4_RESTART_READERS; index++) {
		int path_length = snprintf(reader_path[index], sizeof reader_path[index], "%s.active-%d", path, index);
		if (path_length < 0 || path_length >= sizeof reader_path[index])
			return ext4_restart_failure("seed-path", index, path_length);
		seed[4095] = index + 1;
		int fd = open(reader_path[index], O_CREAT | O_TRUNC | O_WRONLY, 0644);
		if (fd < 0)
			return ext4_restart_failure("seed-open", index, fd);
		for (int page = 0; page < 4; page++) {
			ssize_t count = pwrite(fd, seed, sizeof seed, page * 4096);
			if (count != sizeof seed)
				return ext4_restart_failure("seed-warm-pwrite", index, count);
		}
		ssize_t count = pwrite(fd, seed, sizeof seed, 1024 * 1024);
		if (count != sizeof seed)
			return ext4_restart_failure("seed-cold-pwrite", index, count);
		if (fsync(fd))
			return ext4_restart_failure("seed-fsync", index, -1);
		if (close(fd))
			return ext4_restart_failure("seed-close", index, -1);
	}

	if (pipe(ready))
		return ext4_restart_failure("parent-ready-pipe", -1, -1);
	if (pipe(go))
		return ext4_restart_failure("parent-go-pipe", -1, -1);
	clock_gettime(CLOCK_MONOTONIC, &start);
	for (int index = 0; index < EXT4_RESTART_READERS; index++) {
		children[index] = fork();
		if (children[index] == 0) {
			close(ready[0]);
			close(go[1]);
			_exit(ext4_restart_reader(reader_path[index], expected, ready[1], go[0], index));
		}
		if (children[index] < 0) {
			ext4_restart_failure("parent-fork", index, children[index]);
			failed = 1;
			goto finish;
		}
	}
	close(ready[1]);
	close(go[0]);
	while (warmed < EXT4_RESTART_READERS) {
		struct pollfd poller = {ready[0], POLLIN, 0};
		int readable = syscall(SYS_poll, &poller, 1, 25);
		if (readable < 0 && errno != EINTR) {
			ext4_restart_failure("parent-ready-poll", -1, readable);
			failed = 1;
			goto finish;
		}
		if (readable > 0) {
			ssize_t count = read(ready[0], bytes, sizeof bytes);
			if (count > 0)
				warmed += count;
			else if (count == 0 || errno != EINTR) {
				ext4_restart_failure("parent-ready-read", -1, count);
				failed = 1;
				goto finish;
			}
		}
		clock_gettime(CLOCK_MONOTONIC, &now);
		if (now.tv_sec - start.tv_sec >= 15) {
			ext4_restart_failure("parent-warmup-timeout", -1, warmed);
			failed = 1;
			goto finish;
		}
		nanosleep(&pause, NULL);
	}
	puts("ext4 restart four processes reading");
	fflush(stdout);
	ssize_t released = write(go[1], "gggg", EXT4_RESTART_READERS);
	if (released != EXT4_RESTART_READERS) {
		ext4_restart_failure("parent-release-write", -1, released);
		failed = 1;
		goto finish;
	}
	while (done < EXT4_RESTART_READERS) {
		for (int index = 0; index < EXT4_RESTART_READERS; index++) {
			if (children[index] <= 0)
				continue;
			pid_t result = waitpid(children[index], &status, WNOHANG);
			if (result < 0 || (result > 0 && (!WIFEXITED(status) || WEXITSTATUS(status)))) {
				printf("ext4 restart child %d failed status=%d errno=%d\n", index, status, errno);
				failed = 1;
				goto finish;
			}
			if (result > 0) {
				children[index] = 0;
				done++;
			}
		}
		clock_gettime(CLOCK_MONOTONIC, &now);
		if (now.tv_sec - start.tv_sec >= 55) {
			failed = 1;
			goto finish;
		}
		nanosleep(&pause, NULL);
	}
finish:
	for (int index = 0; index < EXT4_RESTART_READERS; index++)
		if (children[index] > 0) {
			kill(children[index], SIGKILL);
			waitpid(children[index], &status, 0);
		}
	close(ready[0]);
	close(go[1]);
	puts(failed ? "ext4 restart failed" : "ext4 restart ok readers=4 revoked=4 fresh=4");
	return failed;
}
