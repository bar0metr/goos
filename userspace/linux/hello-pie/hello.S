/*
 * The smallest position-independent Linux program: no libc, no relocation
 * to apply, and every address taken relative to the instruction pointer. The
 * linker makes it an ET_DYN image that starts at address zero; the kernel
 * loads it somewhere else and has to say where.
 *
 * Before it prints, the program checks what the kernel told it against
 * where it finds itself running. AT_PHDR must point at the loaded program
 * headers, whose first entry is PT_PHDR and records the table's link-time
 * address: the difference is the load base. AT_ENTRY and the base plus the
 * ELF header's entry field must both be the address of _start. It exits 2
 * when the auxiliary vector is wrong and 1 when the write came up short.
 */
	.globl _start
	.text
_start:
	mov	(%rsp), %rcx		/* argc */
	lea	16(%rsp, %rcx, 8), %rsi	/* envp: past argv and its NULL */
skip_environment:
	lodsq
	test	%rax, %rax
	jnz	skip_environment
	xor	%r8, %r8		/* AT_PHDR */
	xor	%r9, %r9		/* AT_ENTRY */
next_auxiliary:
	lodsq
	mov	%rax, %rdx
	lodsq
	cmp	$3, %rdx
	cmove	%rax, %r8
	cmp	$9, %rdx
	cmove	%rax, %r9
	test	%rdx, %rdx
	jnz	next_auxiliary

	lea	_start(%rip), %r10
	cmp	%r10, %r9
	jne	wrong
	test	%r8, %r8
	jz	wrong
	cmpl	$6, (%r8)		/* p_type of the first header: PT_PHDR */
	jne	wrong
	mov	%r8, %r11
	sub	16(%r8), %r11		/* AT_PHDR - p_vaddr = load base */
	jz	wrong			/* a PIE left at zero was not moved */
	cmpl	$0x464c457f, (%r11)	/* the ELF header sits at the base */
	jne	wrong
	mov	24(%r11), %rax		/* e_entry, as linked */
	add	%r11, %rax
	cmp	%r10, %rax
	jne	wrong

	mov	$1, %eax		/* write */
	mov	$1, %edi
	lea	message(%rip), %rsi
	mov	$length, %edx
	syscall
	xor	%edi, %edi
	cmp	$length, %rax
	je	leave
	mov	$1, %edi
	jmp	leave
wrong:
	mov	$2, %edi
leave:
	mov	$231, %eax		/* exit_group */
	syscall
	ud2

	.section .rodata
message:
	.ascii	"hello from a static PIE\n"
	.set	length, . - message

	.section .note.GNU-stack, "", @progbits
