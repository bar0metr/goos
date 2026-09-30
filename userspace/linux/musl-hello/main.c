/*
 * Static musl startup proof. It reaches main through crt1 and
 * __libc_start_main and uses what libc startup depends on: the argument and
 * environment vectors, the TLS image behind FS, the brk and mmap heaps, and
 * buffered stdio flushed at exit.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

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

int main(int argc, char **argv, char **envp)
{
	int variables = 0;

	printf("argc=%d\n", argc);
	while (envp[variables])
		variables++;
	tls_data += 2;
	tls_zero += 7;
	if (argv[argc] != NULL || argc < 1 || !argv[0][0]) {
		puts("arguments failed");
		return 1;
	}
	if (getenv("PATH") && !strchr(getenv("PATH"), '/')) {
		puts("environment failed");
		return 1;
	}
	if (tls_data != 42 || tls_zero != 7) {
		puts("tls failed");
		return 1;
	}
	if (!heap_ok()) {
		puts("heap failed");
		return 1;
	}
	(void)variables;
	puts("hello from musl");
	return 0;
}
