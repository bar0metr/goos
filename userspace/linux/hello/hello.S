/*
 * The smallest Linux program: no libc, two system calls. It writes one line
 * with write and leaves with exit_group, and exits 1 when the write came up
 * short.
 */
	.globl _start
	.text
_start:
	mov	$1, %eax		/* write */
	mov	$1, %edi
	lea	message(%rip), %rsi
	mov	$length, %edx
	syscall
	xor	%edi, %edi
	cmp	$length, %rax
	je	leave
	mov	$1, %edi
leave:
	mov	$231, %eax		/* exit_group */
	syscall
	ud2

	.section .rodata
message:
	.ascii	"hello from Linux ABI\n"
	.set	length, . - message

	.section .note.GNU-stack, "", @progbits
