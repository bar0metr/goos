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

.global goos_load_gdt
goos_load_gdt:
    subq $16, %rsp
    movw %dx, (%rsp)
    movq %rcx, 2(%rsp)
    lgdt (%rsp)
    addq $16, %rsp
    movw $0x10, %ax
    movw %ax, %ds
    movw %ax, %es
    movw %ax, %ss
    pushq $0x08
    leaq .gdt_loaded(%rip), %rax
    pushq %rax
    lretq
.gdt_loaded:
    retq

.global goos_load_idt
goos_load_idt:
    subq $16, %rsp
    movw %dx, (%rsp)
    movq %rcx, 2(%rsp)
    lidt (%rsp)
    addq $16, %rsp
    retq

.global goos_trigger_breakpoint
goos_trigger_breakpoint:
    int3
    retq

.macro GOOS_FATAL_NOERR vector
.global goos_exception_\vector
goos_exception_\vector:
    cli
    pushq $0
    pushq $\vector
    jmp goos_exception_fatal
.endm

.macro GOOS_FATAL_ERR vector
.global goos_exception_\vector
goos_exception_\vector:
    cli
    pushq $\vector
    jmp goos_exception_fatal
.endm

GOOS_FATAL_NOERR 0
GOOS_FATAL_NOERR 1
GOOS_FATAL_NOERR 2

.global goos_exception_3
goos_exception_3:
    pushq %rax
    pushq %rdx
    pushq %rsi
    leaq breakpoint_message(%rip), %rsi
.breakpoint_next:
    movb (%rsi), %al
    testb %al, %al
    jz .breakpoint_done
    movb %al, %ah
.breakpoint_wait:
    movw $0x3fd, %dx
    inb %dx, %al
    testb $0x20, %al
    jz .breakpoint_wait
    movw $0x3f8, %dx
    movb %ah, %al
    outb %al, %dx
    incq %rsi
    jmp .breakpoint_next
.breakpoint_done:
    popq %rsi
    popq %rdx
    popq %rax
    iretq

GOOS_FATAL_NOERR 4
GOOS_FATAL_NOERR 5
GOOS_FATAL_NOERR 6
GOOS_FATAL_NOERR 7
GOOS_FATAL_ERR   8
GOOS_FATAL_NOERR 9
GOOS_FATAL_ERR   10
GOOS_FATAL_ERR   11
GOOS_FATAL_ERR   12
GOOS_FATAL_ERR   13
GOOS_FATAL_ERR   14
GOOS_FATAL_NOERR 15
GOOS_FATAL_NOERR 16
GOOS_FATAL_ERR   17
GOOS_FATAL_NOERR 18
GOOS_FATAL_NOERR 19
GOOS_FATAL_NOERR 20
GOOS_FATAL_ERR   21
GOOS_FATAL_NOERR 22
GOOS_FATAL_NOERR 23
GOOS_FATAL_NOERR 24
GOOS_FATAL_NOERR 25
GOOS_FATAL_NOERR 26
GOOS_FATAL_NOERR 27
GOOS_FATAL_NOERR 28
GOOS_FATAL_ERR   29
GOOS_FATAL_ERR   30
GOOS_FATAL_NOERR 31

goos_exception_fatal:
    leaq fatal_exception_message(%rip), %rsi
.fatal_next:
    movb (%rsi), %al
    testb %al, %al
    jz .fatal_halt
    movb %al, %ah
.fatal_wait:
    movw $0x3fd, %dx
    inb %dx, %al
    testb $0x20, %al
    jz .fatal_wait
    movw $0x3f8, %dx
    movb %ah, %al
    outb %al, %dx
    incq %rsi
    jmp .fatal_next
.fatal_halt:
    hlt
    jmp .fatal_halt

.section .rdata
breakpoint_message:
    .asciz "EXCEPTION_OK\r\n"
fatal_exception_message:
    .asciz "PANIC: CPU exception\r\n"

.align 8
.global goos_interrupt_table
goos_interrupt_table:
    .quad goos_exception_0, goos_exception_1, goos_exception_2, goos_exception_3
    .quad goos_exception_4, goos_exception_5, goos_exception_6, goos_exception_7
    .quad goos_exception_8, goos_exception_9, goos_exception_10, goos_exception_11
    .quad goos_exception_12, goos_exception_13, goos_exception_14, goos_exception_15
    .quad goos_exception_16, goos_exception_17, goos_exception_18, goos_exception_19
    .quad goos_exception_20, goos_exception_21, goos_exception_22, goos_exception_23
    .quad goos_exception_24, goos_exception_25, goos_exception_26, goos_exception_27
    .quad goos_exception_28, goos_exception_29, goos_exception_30, goos_exception_31
    .quad goos_irq_timer

.section .text
.global goos_irq_timer
goos_irq_timer:
    pushq %rax
    incq goos_timer_ticks(%rip)
    movb $0x20, %al
    outb %al, $0x20
    popq %rax
    iretq

.global goos_start_bootstrap_timer
goos_start_bootstrap_timer:
    cli
    movb $0x11, %al
    outb %al, $0x20
    outb %al, $0xa0
    movb $0x20, %al
    outb %al, $0x21
    movb $0x28, %al
    outb %al, $0xa1
    movb $0x04, %al
    outb %al, $0x21
    movb $0x02, %al
    outb %al, $0xa1
    movb $0x01, %al
    outb %al, $0x21
    outb %al, $0xa1
    movb $0xfe, %al
    outb %al, $0x21
    movb $0xff, %al
    outb %al, $0xa1
    movb $0x36, %al
    outb %al, $0x43
    movb $0x9c, %al
    outb %al, $0x40
    movb $0x2e, %al
    outb %al, $0x40
    sti
    retq

.global goos_timer_ticks_read
goos_timer_ticks_read:
    movq goos_timer_ticks(%rip), %rax
    retq

.section .data
.align 8
goos_timer_ticks:
    .quad 0
