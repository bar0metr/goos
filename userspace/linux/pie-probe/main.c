/*
 * Static PIE load-randomization probe (ADR 0219). A static musl program
 * linked position-independent with no dynamic linker: the start code below
 * applies the image's own R_X86_64_RELATIVE relocations before musl's
 * __libc_start_main runs, as a static-PIE crt does on Linux.
 *
 * Every run checks what the kernel reported against where the image runs:
 * AT_PHDR, AT_PHENT, AT_PHNUM, AT_ENTRY, AT_RANDOM, the initial stack, the
 * relocated pointers in data and RELRO, TLS, getrandom, and that brk starts
 * at the page after the image and grows. It prints
 *
 *	pie base=0x... end=0x... brk=0x...
 *
 * and then one mode line:
 *
 *	pie-probe            pie ok
 *	pie-probe fork       pie fork ok, once the child saw the parent's base
 *	pie-probe exec N     pie exec N, then execs itself with N-1; at zero
 *	                     it prints pie exec done
 *
 * A failed check prints "pie check failed: <name>" and exits 1. The start
 * code exits 3 when it meets a relocation it does not handle.
 */
#define _GNU_SOURCE
#include <elf.h>
#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/auxv.h>
#include <sys/random.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

#define hidden __attribute__((__visibility__("hidden")))

/* The linker defines both: the ELF header sits at the image's base, and
 * _DYNAMIC at its dynamic section. Hidden, so the code reaches them with
 * instruction-pointer-relative addresses that need no relocation. */
extern const Elf64_Ehdr __ehdr_start hidden;
extern const Elf64_Dyn _DYNAMIC[] hidden;

int main(int, char **, char **);
int __libc_start_main(int (*)(int, char **, char **), int, char **, void (*)(void), void (*)(void), void (*)(void));
void _start(void);

/* The stack pointer the kernel handed _start. */
static long *initial_stack;

__asm__(
	".text\n"
	".global _start\n"
	"_start:\n"
	"	xor %rbp, %rbp\n"
	"	mov %rsp, %rdi\n"
	"	andq $-16, %rsp\n"
	"	call _start_c\n"
	"	hlt\n");

static __attribute__((__noreturn__)) void raw_exit(long status)
{
	__asm__ volatile("syscall" ::"a"(SYS_exit_group), "D"(status) : "rcx", "r11", "memory");
	for (;;)
		;
}

/* relocate runs before anything relocated is read. It touches only the
 * dynamic section and the words the relocations name, and calls nothing. */
static __attribute__((__noinline__)) void relocate(void)
{
	uintptr_t base = (uintptr_t)&__ehdr_start;
	const Elf64_Rela *rela = 0;
	size_t size = 0, entry = sizeof(Elf64_Rela);

	for (const Elf64_Dyn *dynamic = _DYNAMIC; dynamic->d_tag != DT_NULL; dynamic++) {
		if (dynamic->d_tag == DT_RELA)
			rela = (const Elf64_Rela *)(base + dynamic->d_un.d_ptr);
		else if (dynamic->d_tag == DT_RELASZ)
			size = dynamic->d_un.d_val;
		else if (dynamic->d_tag == DT_RELAENT)
			entry = dynamic->d_un.d_val;
		/* 36 is DT_RELR, newer than this musl's elf.h. */
		else if (dynamic->d_tag == DT_REL || dynamic->d_tag == 36 || dynamic->d_tag == DT_JMPREL)
			raw_exit(3);
	}
	if (entry != sizeof(Elf64_Rela))
		raw_exit(3);
	for (size_t index = 0; rela && index < size / entry; index++) {
		if (ELF64_R_TYPE(rela[index].r_info) != R_X86_64_RELATIVE)
			raw_exit(3);
		*(uint64_t *)(base + rela[index].r_offset) = base + rela[index].r_addend;
	}
}

hidden void _start_c(long *stack)
{
	relocate();
	initial_stack = stack;
	__libc_start_main(main, (int)stack[0], (char **)(stack + 1), 0, 0, 0);
}

static int marker(void) { return 0x5049; }

/* Pointers the linker leaves to relocation: one in writable data, a table
 * in RELRO. */
static int (*marker_pointer)(void) = marker;
static const char *const relro_names[] = {"alpha", "beta", "gamma"};
static _Thread_local int tls_data = 40;
static _Thread_local int tls_zero;

static int failed;

static void check(int ok, const char *name)
{
	if (!ok) {
		printf("pie check failed: %s\n", name);
		failed = 1;
	}
}

static uintptr_t image_end(uintptr_t base)
{
	const Elf64_Phdr *headers = (const Elf64_Phdr *)(base + __ehdr_start.e_phoff);
	uintptr_t end = 0;

	for (int index = 0; index < __ehdr_start.e_phnum; index++) {
		if (headers[index].p_type == PT_LOAD && headers[index].p_vaddr + headers[index].p_memsz > end)
			end = headers[index].p_vaddr + headers[index].p_memsz;
	}
	return (base + end + 4095) & ~(uintptr_t)4095;
}

static uintptr_t current_brk(void) { return (uintptr_t)syscall(SYS_brk, 0); }

static void check_image(int argc, char **argv, char **envp, uintptr_t base)
{
	const Elf64_Phdr *headers = (const Elf64_Phdr *)getauxval(AT_PHDR);
	unsigned char random[16] = {0};

	check(base != 0 && __ehdr_start.e_ident[EI_MAG0] == ELFMAG0 && __ehdr_start.e_type == ET_DYN, "elf header at base");
	check((uintptr_t)headers == base + __ehdr_start.e_phoff, "AT_PHDR");
	check(headers != 0 && headers[0].p_type == PT_PHDR && (uintptr_t)headers - headers[0].p_vaddr == base, "PT_PHDR");
	check(getauxval(AT_PHENT) == sizeof(Elf64_Phdr), "AT_PHENT");
	check(getauxval(AT_PHNUM) == __ehdr_start.e_phnum, "AT_PHNUM");
	check(getauxval(AT_ENTRY) == base + __ehdr_start.e_entry && getauxval(AT_ENTRY) == (uintptr_t)_start, "AT_ENTRY");
	check(getauxval(AT_RANDOM) != 0, "AT_RANDOM");
	check(initial_stack != 0 && ((uintptr_t)initial_stack & 15) == 0, "stack alignment");
	check(initial_stack != 0 && initial_stack[0] == argc && (char **)(initial_stack + 1) == argv && argv[argc] == 0, "argv layout");
	check(envp == argv + argc + 1, "envp layout");
	check(marker_pointer == marker && marker_pointer() == 0x5049, "data relocation");
	check(strcmp(relro_names[1], "beta") == 0 && (uintptr_t)relro_names[2] > base, "RELRO relocation");
	tls_data += 2;
	tls_zero += 7;
	check(tls_data == 42 && tls_zero == 7, "TLS");
	check(getrandom(random, sizeof random, 0) == sizeof random, "getrandom");
}

static void check_brk(uintptr_t base)
{
	uintptr_t start = current_brk();
	uintptr_t grown;

	check(start == image_end(base), "brk starts after the image");
	grown = (uintptr_t)syscall(SYS_brk, start + (1 << 20));
	check(grown == start + (1 << 20), "brk grows");
	if (grown == start + (1 << 20)) {
		memset((void *)start, 0x5a, 1 << 20);
		check(((unsigned char *)start)[(1 << 20) - 1] == 0x5a, "brk memory");
	}
	check((uintptr_t)syscall(SYS_brk, start) == start, "brk shrinks");
	char *heap = malloc(256 << 10);
	check(heap != 0, "malloc");
	if (heap)
		memset(heap, 1, 256 << 10);
	free(heap);
}

static int fork_mode(uintptr_t base)
{
	uintptr_t phdr = getauxval(AT_PHDR), entry = getauxval(AT_ENTRY), brk = current_brk();
	int status = 0;
	pid_t child;

	fflush(stdout);
	child = fork();

	if (child == 0) {
		int same = (uintptr_t)&__ehdr_start == base && getauxval(AT_PHDR) == phdr && getauxval(AT_ENTRY) == entry &&
			   current_brk() == brk && marker_pointer == marker;
		printf("pie child base=0x%lx\n", (unsigned long)(uintptr_t)&__ehdr_start);
		fflush(stdout);
		_exit(same ? 0 : 1);
	}
	check(child > 0 && waitpid(child, &status, 0) == child && WIFEXITED(status) && WEXITSTATUS(status) == 0, "fork child layout");
	if (!failed)
		printf("pie fork ok\n");
	return failed;
}

static int exec_mode(char **argv, int remaining)
{
	char count[16];
	char *arguments[] = {argv[0], "exec", count, 0};

	printf("pie exec %d\n", remaining);
	if (failed)
		return 1;
	if (remaining <= 0) {
		printf("pie exec done\n");
		return 0;
	}
	snprintf(count, sizeof count, "%d", remaining - 1);
	fflush(stdout);
	execv(argv[0], arguments);
	printf("pie check failed: execv errno=%d\n", errno);
	return 1;
}

int main(int argc, char **argv, char **envp)
{
	uintptr_t base = (uintptr_t)&__ehdr_start;

	check_image(argc, argv, envp, base);
	check_brk(base);
	printf("pie base=0x%lx end=0x%lx brk=0x%lx\n", (unsigned long)base, (unsigned long)image_end(base),
	       (unsigned long)current_brk());
	if (argc >= 2 && strcmp(argv[1], "fork") == 0)
		return fork_mode(base);
	if (argc >= 3 && strcmp(argv[1], "exec") == 0)
		return exec_mode(argv, atoi(argv[2]));
	if (failed)
		return 1;
	printf("pie ok\n");
	return 0;
}
