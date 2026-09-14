//go:build uefi && amd64

.section .text

.global goos_outb
goos_outb:
    movb %dl, %al
    movw %cx, %dx
    outb %al, %dx
    retq

.global goos_inb
goos_inb:
    movw %cx, %dx
    inb %dx, %al
    retq

.global goos_pause
goos_pause:
    pause
    retq

.global goos_halt
goos_halt:
    hlt
    retq

.global goos_has_nx
goos_has_nx:
    pushq %rbx
    movl $0x80000000, %eax
    cpuid
    cmpl $0x80000001, %eax
    jb .no_nx
    movl $0x80000001, %eax
    cpuid
    btl $20, %edx
    setc %al
    popq %rbx
    retq
.no_nx:
    xorl %eax, %eax
    popq %rbx
    retq

.global goos_enable_nx_wp
goos_enable_nx_wp:
    movl $0xc0000080, %ecx
    rdmsr
    orl $0x800, %eax
    wrmsr
    movq %cr0, %rax
    orq $0x10000, %rax
    movq %rax, %cr0
    retq

.global goos_load_cr3
goos_load_cr3:
    movq %rcx, %cr3
    retq

.global goos_read_rsp
goos_read_rsp:
    movq %rsp, %rax
    retq
