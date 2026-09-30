//go:build uefi && amd64

.set GOOS_KERNEL_SLOT, 0
.set GOOS_IDLE_SLOT, 3
.set GOOS_YIELD_VECTOR, 0x81
.set GOOS_FPU_AREA_SHIFT, 9
.set GOOS_CR4_SSE, 0x600
.set GOOS_KERNEL_LOW_BASE, 0xffffc00000000000
.set GOOS_LINUX_ENTRY_PROCESS_ROOT, 0
.set GOOS_LINUX_ENTRY_STACK, 8
.set GOOS_LINUX_ENTRY_USER_RSP, 16
.set GOOS_LINUX_FRAME_BASE, 32
.set GOOS_LINUX_FRAME_RETURN, 88
.set GOOS_LINUX_FRAME_ACTION, 96
.set GOOS_LINUX_FRAME_RESERVE, 136
.set GOOS_LINUX_SAVED_RAX, GOOS_LINUX_FRAME_RESERVE + 0
.set GOOS_LINUX_SAVED_RCX, GOOS_LINUX_FRAME_RESERVE + 8
.set GOOS_LINUX_SAVED_RDX, GOOS_LINUX_FRAME_RESERVE + 16
.set GOOS_LINUX_SAVED_RSI, GOOS_LINUX_FRAME_RESERVE + 40
.set GOOS_LINUX_SAVED_RDI, GOOS_LINUX_FRAME_RESERVE + 48
.set GOOS_LINUX_SAVED_R8, GOOS_LINUX_FRAME_RESERVE + 56
.set GOOS_LINUX_SAVED_R9, GOOS_LINUX_FRAME_RESERVE + 64
.set GOOS_LINUX_SAVED_R10, GOOS_LINUX_FRAME_RESERVE + 72
.set GOOS_LINUX_SAVED_R11, GOOS_LINUX_FRAME_RESERVE + 80
.set GOOS_LINUX_IRET_BASE, GOOS_LINUX_FRAME_RESERVE + 120
.set GOOS_LINUX_ACTION_EXIT, 1
.set GOOS_LINUX_ACTION_BLOCK, 2

// The console ring is a power of two so the wrap is a mask. It holds two of
// the longest lines the terminal accepts (4096 bytes), so a pasted line
// arrives whole even when the terminal service reads it a tick later.
// Two input rings: 0 is COM1's, 1 the PS/2 keyboard's. Each console reads
// its own (ADR 0125). A ring is a header of four quads and its data.
.set GOOS_INPUT_RING, 8192
.set GOOS_INPUT_RING_MASK, 8191
.set GOOS_INPUT_HEAD, 0
.set GOOS_INPUT_TAIL, 8
.set GOOS_INPUT_DROPPED, 16
.set GOOS_INPUT_INTERRUPT, 24
.set GOOS_INPUT_DATA, 32
.set GOOS_INPUT_STRIDE, 8224
.set GOOS_INPUT_COUNT, 2
.set GOOS_KEYBOARD_BYTES, 8

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

.global goos_outw
goos_outw:
    movw %dx, %ax
    movw %cx, %dx
    outw %ax, %dx
    retq

.global goos_inw
goos_inw:
    movw %cx, %dx
    inw %dx, %ax
    retq

.global goos_outl
goos_outl:
    movl %edx, %eax
    movw %cx, %dx
    outl %eax, %dx
    retq

// goos_mmio_write32 and goos_mmio_read32 keep memory-mapped device access out
// of the compiler's reach. These are ordinary memory operations to the
// optimiser, which may reorder, merge or discard them; a device register select
// followed by a data write must survive exactly as written.
.global goos_mmio_write32
goos_mmio_write32:
    movl %edx, (%rcx)
    retq

.global goos_mmio_read32
goos_mmio_read32:
    movl (%rcx), %eax
    retq

.global goos_inl
goos_inl:
    movw %cx, %dx
    inl %dx, %eax
    retq

.global goos_pause
goos_pause:
    pause
    retq

.global goos_halt
goos_halt:
    hlt
    retq

.global goos_system_reset
goos_system_reset:
    cli
    movw $0xcf9, %dx
    movb $0x02, %al
    outb %al, %dx
    movb $0x06, %al
    outb %al, %dx
    movb $0xfe, %al
    outb %al, $0x64
    lidt goos_zero_idtr(%rip)
    int3
    retq

.global goos_copy_bytes
goos_copy_bytes:
    pushq %rdi
    pushq %rsi
    movq %rcx, %rdi
    movq %rdx, %rsi
    movq %r8, %rcx
    shrq $3, %rcx
    cld
    rep movsq
    movq %r8, %rcx
    andq $7, %rcx
    rep movsb
    popq %rsi
    popq %rdi
    retq

.global goos_zero_bytes
goos_zero_bytes:
    pushq %rdi
    movq %rcx, %rdi
    movq %rdx, %rcx
    shrq $3, %rcx
    xorl %eax, %eax
    cld
    rep stosq
    movq %rdx, %rcx
    andq $7, %rcx
    rep stosb
    popq %rdi
    retq

.global goos_scheduler_state_read
goos_scheduler_state_read:
    movq goos_runnable_mask(%rip), %rax
    movq %rax, 0(%rcx)
    movq goos_task_dead_mask(%rip), %rax
    movq %rax, 8(%rcx)
    movq goos_task_count(%rip), %rax
    movq %rax, 16(%rcx)
    movq goos_current_task(%rip), %rax
    movq %rax, 24(%rcx)
    retq

.global goos_task_rsp0_read
goos_task_rsp0_read:
    leaq goos_task_rsp0(%rip), %rax
    movq (%rax,%rcx,8), %rax
    retq

.global goos_interrupts_disable
goos_interrupts_disable:
    pushfq
    popq %rax
    cli
    retq

.global goos_interrupts_restore
goos_interrupts_restore:
    testq $0x200, %rcx
    jz .interrupts_restore_done
    sti
.interrupts_restore_done:
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

.global goos_has_pat
goos_has_pat:
    pushq %rbx
    movl $1, %eax
    cpuid
    btl $16, %edx
    setc %al
    popq %rbx
    retq

.global goos_configure_pat
goos_configure_pat:
    pushfq
    cli
    movq %cr0, %r8
    movq %r8, %rax
    orq $0x40000000, %rax
    andq $~0x20000000, %rax
    movq %rax, %cr0
    wbinvd
    movq %cr3, %rax
    movq %rax, %cr3
    movl $0x277, %ecx
    movl $0x00070406, %eax
    movl $0x00070406, %edx
    wrmsr
    wbinvd
    movq %r8, %cr0
    movq %cr3, %rax
    movq %rax, %cr3
    movl $0x277, %ecx
    rdmsr
    xorl $0x00070406, %eax
    xorl $0x00070406, %edx
    orl %edx, %eax
    sete %al
    movzbl %al, %eax
    popfq
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
    movq %cr4, %rax
    orq $GOOS_CR4_SSE, %rax
    movq %rax, %cr4
    retq

// goos_install_linux_syscall enables SYSCALL and verifies every MSR used by
// the entry contract. GS_BASE remains user-owned zero; SWAPGS selects the
// supervisor entry-data mapping named by KERNEL_GS_BASE.
.global goos_install_linux_syscall
goos_install_linux_syscall:
    pushfq
    cli
    movq %rcx, %r10
    movq %rdx, %r11

    movl $0xc0000080, %ecx
    rdmsr
    orl $1, %eax
    wrmsr

    movq %r8, %rax
    movq %r8, %rdx
    shrq $32, %rdx
    movl $0xc0000081, %ecx
    wrmsr

    movq %r10, %rax
    movq %r10, %rdx
    shrq $32, %rdx
    movl $0xc0000082, %ecx
    wrmsr

    movq %r9, %rax
    movq %r9, %rdx
    shrq $32, %rdx
    movl $0xc0000084, %ecx
    wrmsr

    xorl %eax, %eax
    xorl %edx, %edx
    movl $0xc0000101, %ecx
    wrmsr

    movq %r11, %rax
    movq %r11, %rdx
    shrq $32, %rdx
    movl $0xc0000102, %ecx
    wrmsr

    movl $0xc0000080, %ecx
    rdmsr
    testl $1, %eax
    jz .linux_syscall_install_failed
    movl $0xc0000082, %ecx
    rdmsr
    shlq $32, %rdx
    orq %rdx, %rax
    cmpq %r10, %rax
    jne .linux_syscall_install_failed
    movl $0xc0000084, %ecx
    rdmsr
    shlq $32, %rdx
    orq %rdx, %rax
    cmpq %r9, %rax
    jne .linux_syscall_install_failed
    movl $0xc0000102, %ecx
    rdmsr
    shlq $32, %rdx
    orq %rdx, %rax
    cmpq %r11, %rax
    jne .linux_syscall_install_failed
    movl $1, %eax
    jmp .linux_syscall_install_done
.linux_syscall_install_failed:
    xorl %eax, %eax
.linux_syscall_install_done:
    popfq
    retq

.global goos_load_cr3
goos_load_cr3:
    movq %rcx, %cr3
    retq

.global goos_set_kernel_root
goos_set_kernel_root:
    movq %rcx, goos_kernel_root(%rip)
    retq

.global goos_read_rsp
goos_read_rsp:
    movq %rsp, %rax
    retq

// goos_switch_to_boot_stack(top) moves the boot task onto the guarded boot
// stack and calls goos_boot_continue there (ADR 0091). Win64 wants the stack
// 16-byte aligned at the call and 32 bytes of home space above the return
// address. Nothing returns here; a return would halt.
.global goos_switch_to_boot_stack
goos_switch_to_boot_stack:
    movq %rcx, %rsp
    andq $-16, %rsp
    subq $32, %rsp
    xorl %ebp, %ebp
    callq goos_boot_continue
.boot_stack_returned:
    cli
    hlt
    jmp .boot_stack_returned

.global goos_read_tsc
goos_read_tsc:
    rdtsc
    shlq $32, %rdx
    orq %rdx, %rax
    retq

// CPUID.01H:ECX bit 30 reports RDRAND.
.global goos_has_rdrand
goos_has_rdrand:
    pushq %rbx
    movl $1, %eax
    cpuid
    btl $30, %ecx
    setc %al
    popq %rbx
    retq

// goos_rdrand64 stores one hardware random value at the address in rcx and
// returns whether the generator produced it. The generator may run dry for a
// moment, which it signals by clearing CF; ten tries is what its vendors
// recommend before a caller treats it as failed.
.global goos_rdrand64
goos_rdrand64:
    movl $10, %edx
.rdrand_retry:
    rdrand %rax
    jc .rdrand_ready
    decl %edx
    jnz .rdrand_retry
    xorl %eax, %eax
    retq
.rdrand_ready:
    movq %rax, (%rcx)
    movl $1, %eax
    retq

.global goos_probe_address_spaces
goos_probe_address_spaces:
    pushfq
    cli
    movabsq $GOOS_KERNEL_LOW_BASE, %rax
    addq %rax, %rsp
    leaq .probe_address_spaces_high(%rip), %r10
    addq %rax, %r10
    jmp *%r10
.probe_address_spaces_high:
    movq %rcx, %cr3
    movzbq (%r9), %rcx
    movq %rdx, %cr3
    movzbq (%r9), %r11
    movq %r8, %cr3
    subq %rax, %rsp
    leaq .probe_address_spaces_low(%rip), %r10
    subq %rax, %r10
    jmp *%r10
.probe_address_spaces_low:
    movq %r11, %rax
    shlq $8, %rax
    orq %rcx, %rax
    popfq
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
    movw $0x28, %ax
    ltr %ax
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

// IDT gates use the higher-half alias of each entry label. This prefix uses
// assembly only, installs the kernel root, and returns to the handler's low
// link-time address with every interrupted register intact.
.macro GOOS_ENTER_LOW target
    pushq %rax
    movq goos_kernel_root(%rip), %rax
    movq %rax, %cr3
    popq %rax
    pushq %r11
    movabsq $\target, %r11
    xchgq %r11, (%rsp)
    retq
.endm

// User returns enter this prefix while the kernel root is active. The prefix
// changes CR3 only after control reaches its shared higher-half address.
.macro GOOS_RETURN_USER
    cmpq $0x23, 8(%rsp)
    jne 991f
    pushq %rax
    pushq %rcx
    movq goos_current_task(%rip), %rax
    leaq goos_task_roots(%rip), %rcx
    movq (%rcx,%rax,8), %rax
    pushq %rax
    pushq %r11
    movabsq $(GOOS_KERNEL_LOW_BASE + goos_return_high), %r11
    xchgq %r11, (%rsp)
    retq
991:
.endm

// Linux SYSCALL entry owns kernel GS until this prefix has installed the
// process CR3. Interrupts stay masked throughout the compatibility call.
.macro GOOS_RETURN_LINUX
    cmpq $0x23, 8(%rsp)
    jne 991f
    pushq %rax
    pushq %rcx
    movq goos_current_task(%rip), %rax
    leaq goos_task_roots(%rip), %rcx
    movq (%rcx,%rax,8), %rax
    pushq %rax
    pushq %r11
    movabsq $(GOOS_KERNEL_LOW_BASE + goos_linux_return_high), %r11
    xchgq %r11, (%rsp)
    retq
991:
.endm

goos_return_high:
    popq %rax
    movq %rax, %cr3
    popq %rcx
    popq %rax
    iretq

goos_linux_return_high:
    popq %rax
    movq %rax, %cr3
    popq %rcx
    popq %rax
    swapgs
    iretq

.macro GOOS_FATAL_NOERR vector
.global goos_exception_\vector
goos_exception_\vector:
    GOOS_ENTER_LOW .L_exception_\vector
.L_exception_\vector:
    cli
    pushq $0
    pushq $\vector
    jmp goos_exception_fatal
.endm

.macro GOOS_FATAL_ERR vector
.global goos_exception_\vector
goos_exception_\vector:
    GOOS_ENTER_LOW .L_exception_\vector
.L_exception_\vector:
    cli
    pushq $\vector
    jmp goos_exception_fatal
.endm

GOOS_FATAL_NOERR 0
GOOS_FATAL_NOERR 1
GOOS_FATAL_NOERR 2

.global goos_exception_3
goos_exception_3:
    GOOS_ENTER_LOW .L_exception_3
.L_exception_3:
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
    GOOS_RETURN_USER
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
    cmpq $0x23, 24(%rsp)
    je .user_fault
    cmpq $8, (%rsp)
    je .fatal_double
    leaq fatal_exception_message(%rip), %rsi
    cmpq $6, (%rsp)
    jne .fatal_check_gp
    leaq fatal_ud_message(%rip), %rsi
    jmp .fatal_next
.fatal_check_gp:
    cmpq $13, (%rsp)
    jne .fatal_check_pf
    leaq fatal_gp_message(%rip), %rsi
    jmp .fatal_next
.fatal_check_pf:
    cmpq $14, (%rsp)
    jne .fatal_next
    leaq fatal_pf_message(%rip), %rsi
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

// A double fault arrives on its own stack (IST 1). Its usual cause here is a
// kernel stack overflow: the page fault on a guard page cannot push its frame
// onto the exhausted stack. CR2 still holds that fault's address, which names
// the guard page, so it is printed after the message.
.fatal_double:
    leaq fatal_df_message(%rip), %rsi
.fatal_double_next:
    movb (%rsi), %al
    testb %al, %al
    jz .fatal_double_address
    movb %al, %ah
    callq .fatal_serial_byte
    incq %rsi
    jmp .fatal_double_next
.fatal_double_address:
    movq %cr2, %rsi
    movl $16, %ecx
.fatal_double_digit:
    rolq $4, %rsi
    movl %esi, %eax
    andl $0xf, %eax
    leaq hex_digits(%rip), %rdx
    movb (%rdx,%rax), %ah
    callq .fatal_serial_byte
    decl %ecx
    jnz .fatal_double_digit
    movb $0x0d, %ah
    callq .fatal_serial_byte
    movb $0x0a, %ah
    callq .fatal_serial_byte
    jmp .fatal_halt

// .fatal_serial_byte writes %ah to COM1 once the transmitter is empty. It
// clobbers %al and %dx only.
.fatal_serial_byte:
    movw $0x3fd, %dx
.fatal_serial_wait:
    inb %dx, %al
    testb $0x20, %al
    jz .fatal_serial_wait
    movw $0x3f8, %dx
    movb %ah, %al
    outb %al, %dx
    retq

// A page fault in user mode may be a demand-zero page (ADR 0104). The
// registers and the SSE state are saved around the resolver, which runs on the
// task's kernel stack with interrupts disabled; a supplied page retries the
// instruction, anything else retires the task as before.
.user_fault:
    cmpq $14, (%rsp)
    jne .terminate_user_fault
    pushq %r15
    pushq %r14
    pushq %r13
    pushq %r12
    pushq %r11
    pushq %r10
    pushq %r9
    pushq %r8
    pushq %rdi
    pushq %rsi
    pushq %rbp
    pushq %rbx
    pushq %rdx
    pushq %rcx
    pushq %rax
    movq goos_current_task(%rip), %rax
    movq %rax, %r9
    shlq $GOOS_FPU_AREA_SHIFT, %r9
    leaq goos_task_fpu(%rip), %r10
    fxsave64 (%r10,%r9)
    subq $32, %rsp
    leaq goos_task_roots(%rip), %rcx
    movq (%rcx,%rax,8), %rcx
    movq %cr2, %rdx
    movq 160(%rsp), %r8
    callq goos_resolve_user_fault
    addq $32, %rsp
    movq %rax, %r11
    movq goos_current_task(%rip), %r9
    shlq $GOOS_FPU_AREA_SHIFT, %r9
    leaq goos_task_fpu(%rip), %r10
    fxrstor64 (%r10,%r9)
    testq %r11, %r11
    popq %rax
    popq %rcx
    popq %rdx
    popq %rbx
    popq %rbp
    popq %rsi
    popq %rdi
    popq %r8
    popq %r9
    popq %r10
    popq %r11
    popq %r12
    popq %r13
    popq %r14
    popq %r15
    jz .terminate_user_fault
    addq $16, %rsp
    GOOS_RETURN_USER
    iretq

.terminate_user_fault:
    incq goos_user_faults(%rip)
    movq goos_current_task(%rip), %rcx
    // A page fault records its address, any other fault zero, so the kernel
    // can tell which page a faulting task touched.
    xorl %eax, %eax
    cmpq $14, (%rsp)
    jne .record_user_fault_address
    movq %cr2, %rax
.record_user_fault_address:
    leaq goos_task_fault_addresses(%rip), %rdx
    movq %rax, (%rdx,%rcx,8)
    movq (%rsp), %rax
    btsq $63, %rax
    leaq goos_task_exit_codes(%rip), %rdx
    movq %rax, (%rdx,%rcx,8)
    btsq %rcx, goos_task_exit_pending(%rip)
    btrq %rcx, goos_runnable_mask(%rip)
    btsq %rcx, goos_task_dead_mask(%rip)
    btsq $GOOS_KERNEL_SLOT, goos_runnable_mask(%rip)
    movq %rcx, %rax
    callq goos_select_next
    jmp .switch_to

.section .rdata
goos_zero_idtr:
    .quad 0, 0
breakpoint_message:
    .asciz "EXCEPTION_OK\r\n"
fatal_exception_message:
    .asciz "PANIC: CPU exception\r\n"
fatal_ud_message:
    .asciz "PANIC: invalid opcode\r\n"
fatal_gp_message:
    .asciz "PANIC: general protection fault\r\n"
fatal_pf_message:
    .asciz "PANIC: page fault\r\n"
fatal_df_message:
    .asciz "PANIC: double fault, a kernel stack overflow if this address is a guard page: 0x"
hex_digits:
    .ascii "0123456789abcdef"

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
    .quad goos_user_call
    .quad goos_irq_11
    .quad goos_irq_yield
    .quad goos_irq_serial
    .quad goos_irq_keyboard

.global goos_entry_mapping_table
goos_entry_mapping_table:
    .quad goos_return_high
    .quad goos_kernel_root
    .quad goos_probe_address_spaces

.global goos_linux_entry_mapping_table
goos_linux_entry_mapping_table:
    .quad goos_linux_syscall_entry
    .quad goos_linux_return_high
    .quad goos_linux_entry_data

.section .text
.global goos_linux_syscall_entry
goos_linux_syscall_entry:
    swapgs
    movq %rsp, %gs:GOOS_LINUX_ENTRY_USER_RSP
    movq %gs:GOOS_LINUX_ENTRY_STACK, %rsp
    pushq %r15
    pushq %r14
    pushq %r13
    pushq %r12
    pushq %r11
    pushq %r10
    pushq %r9
    pushq %r8
    pushq %rdi
    pushq %rsi
    pushq %rbp
    pushq %rbx
    pushq %rdx
    pushq %rcx
    pushq %rax
    movq goos_kernel_root(%rip), %r10
    movq %r10, %cr3
    cld
    movabsq $.L_linux_syscall_low, %r10
    jmp *%r10
.L_linux_syscall_low:
    subq $GOOS_LINUX_FRAME_RESERVE, %rsp
    movq GOOS_LINUX_SAVED_RAX(%rsp), %rax
    movq %rax, GOOS_LINUX_FRAME_BASE+0(%rsp)
    movq GOOS_LINUX_SAVED_RDI(%rsp), %rax
    movq %rax, GOOS_LINUX_FRAME_BASE+8(%rsp)
    movq GOOS_LINUX_SAVED_RSI(%rsp), %rax
    movq %rax, GOOS_LINUX_FRAME_BASE+16(%rsp)
    movq GOOS_LINUX_SAVED_RDX(%rsp), %rax
    movq %rax, GOOS_LINUX_FRAME_BASE+24(%rsp)
    movq GOOS_LINUX_SAVED_R10(%rsp), %rax
    movq %rax, GOOS_LINUX_FRAME_BASE+32(%rsp)
    movq GOOS_LINUX_SAVED_R8(%rsp), %rax
    movq %rax, GOOS_LINUX_FRAME_BASE+40(%rsp)
    movq GOOS_LINUX_SAVED_R9(%rsp), %rax
    movq %rax, GOOS_LINUX_FRAME_BASE+48(%rsp)
    movq GOOS_LINUX_SAVED_RCX(%rsp), %rax
    movq %rax, GOOS_LINUX_FRAME_BASE+56(%rsp)
    movq GOOS_LINUX_SAVED_R11(%rsp), %rax
    movq %rax, GOOS_LINUX_FRAME_BASE+64(%rsp)
    movq %gs:GOOS_LINUX_ENTRY_USER_RSP, %rax
    movq %rax, GOOS_LINUX_FRAME_BASE+72(%rsp)
    movq %gs:GOOS_LINUX_ENTRY_PROCESS_ROOT, %rax
    movq %rax, GOOS_LINUX_FRAME_BASE+80(%rsp)
    leaq GOOS_LINUX_FRAME_BASE(%rsp), %rcx
    callq goos_dispatch_linux_syscall
    cmpq $GOOS_LINUX_ACTION_EXIT, GOOS_LINUX_FRAME_BASE+GOOS_LINUX_FRAME_ACTION(%rsp)
    je .linux_user_exit
    cmpq $GOOS_LINUX_ACTION_BLOCK, GOOS_LINUX_FRAME_BASE+GOOS_LINUX_FRAME_ACTION(%rsp)
    je .linux_user_block
    movq GOOS_LINUX_FRAME_BASE+GOOS_LINUX_FRAME_RETURN(%rsp), %rax
    movq %rax, GOOS_LINUX_SAVED_RAX(%rsp)
    movq GOOS_LINUX_SAVED_RCX(%rsp), %rax
    movq %rax, GOOS_LINUX_IRET_BASE+0(%rsp)
    movq $0x23, GOOS_LINUX_IRET_BASE+8(%rsp)
    movq GOOS_LINUX_SAVED_R11(%rsp), %rax
    andq $~0x77700, %rax
    orq $0x202, %rax
    movq %rax, GOOS_LINUX_IRET_BASE+16(%rsp)
    movq GOOS_LINUX_FRAME_BASE+72(%rsp), %rax
    movq %rax, GOOS_LINUX_IRET_BASE+24(%rsp)
    movq $0x1b, GOOS_LINUX_IRET_BASE+32(%rsp)
    addq $GOOS_LINUX_FRAME_RESERVE, %rsp
    popq %rax
    popq %rcx
    popq %rdx
    popq %rbx
    popq %rbp
    popq %rsi
    popq %rdi
    popq %r8
    popq %r9
    popq %r10
    popq %r11
    popq %r12
    popq %r13
    popq %r14
    popq %r15
    GOOS_RETURN_LINUX
    iretq
.linux_user_block:
    // Save an ordinary scheduler context that retries the two-byte SYSCALL
    // instruction after its continuation is woken. The continuation's Issued
    // bit prevents the retried dispatcher from duplicating side effects.
    movq %rsp, %r10
    subq $160, %rsp
    movq 248(%r10), %rax
    movq %rax, 0(%rsp)
    movq 240(%r10), %rax
    movq %rax, 8(%rsp)
    movq 232(%r10), %rax
    movq %rax, 16(%rsp)
    movq 224(%r10), %rax
    movq %rax, 24(%rsp)
    movq 216(%r10), %rax
    movq %rax, 32(%rsp)
    movq 208(%r10), %rax
    movq %rax, 40(%rsp)
    movq 200(%r10), %rax
    movq %rax, 48(%rsp)
    movq 192(%r10), %rax
    movq %rax, 56(%rsp)
    movq 184(%r10), %rax
    movq %rax, 64(%rsp)
    movq 176(%r10), %rax
    movq %rax, 72(%rsp)
    movq 168(%r10), %rax
    movq %rax, 80(%rsp)
    movq 152(%r10), %rax
    movq %rax, 88(%rsp)
    movq 144(%r10), %rax
    movq %rax, 96(%rsp)
    movq 160(%r10), %rax
    movq %rax, 104(%rsp)
    movq 136(%r10), %rax
    movq %rax, 112(%rsp)
    movq 88(%r10), %rax
    subq $2, %rax
    movq %rax, 120(%rsp)
    movq $0x23, 128(%rsp)
    movq 96(%r10), %rax
    andq $~0x77700, %rax
    orq $0x202, %rax
    movq %rax, 136(%rsp)
    movq 104(%r10), %rax
    movq %rax, 144(%rsp)
    movq $0x1b, 152(%rsp)
    movq goos_current_task(%rip), %rcx
    leaq goos_task_contexts(%rip), %rdx
    movq %rsp, (%rdx,%rcx,8)
    movq %rcx, %r9
    shlq $GOOS_FPU_AREA_SHIFT, %r9
    leaq goos_task_fpu(%rip), %r10
    fxsave64 (%r10,%r9)
    btrq %rcx, goos_runnable_mask(%rip)
    swapgs
    movq %rcx, %rax
    callq goos_select_next
    jmp .switch_to
.linux_user_exit:
    movq goos_current_task(%rip), %rcx
    btsq %rcx, goos_task_exit_pending(%rip)
    btrq %rcx, goos_runnable_mask(%rip)
    btsq %rcx, goos_task_dead_mask(%rip)
    btsq $GOOS_KERNEL_SLOT, goos_runnable_mask(%rip)
    swapgs
    movq %rcx, %rax
    callq goos_select_next
    jmp .switch_to

.global goos_user_call
goos_user_call:
    GOOS_ENTER_LOW .L_user_call
.L_user_call:
    incq goos_user_call_entries(%rip)
    pushq %rax
    pushq %rdx
    pushq %r8
    movq goos_current_task(%rip), %rax
    leaq 24(%rsp), %rdx
    leaq goos_user_call_rsp_baselines(%rip), %r8
    cmpq $0, (%r8,%rax,8)
    jne .L_user_call_compare_rsp
    movq %rdx, (%r8,%rax,8)
    jmp .L_user_call_rsp_recorded
.L_user_call_compare_rsp:
    cmpq (%r8,%rax,8), %rdx
    je .L_user_call_rsp_recorded
    incq goos_user_call_rsp_drift(%rip)
.L_user_call_rsp_recorded:
    popq %r8
    popq %rdx
    popq %rax
    cmpq $0x23, 8(%rsp)
    jne goos_exception_fatal
	 pushq %r15
	 pushq %r14
	 pushq %r13
	 pushq %r12
	 pushq %r11
	 pushq %r10
	 pushq %r9
	 pushq %r8
	 pushq %rdi
	 pushq %rsi
	 pushq %rbp
	 pushq %rbx
	 pushq %rdx
	 pushq %rcx
	 pushq %rax
	 subq $48, %rsp
	 movq 48(%rsp), %rax
	 movq %rax, 40(%rsp)
	 movq goos_current_task(%rip), %rax
	 leaq goos_task_roots(%rip), %rcx
	 movq (%rcx,%rax,8), %rcx
	 callq goos_authorize_native_call
	 testq %rax, %rax
	 jnz .user_call_result
	 cmpq $5, 48(%rsp)
	 je .user_call_wait
	 cmpq $12, 48(%rsp)
	 je .user_call_process_wait
	 cmpq $17, 48(%rsp)
	 je .user_call_irq_wait
	 cmpq $21, 48(%rsp)
	 je .user_call_stream_wait
	 cmpq $22, 48(%rsp)
	 je .user_call_wait_any
	 movq 48(%rsp), %rcx
	 movq 96(%rsp), %rdx
	 movq 88(%rsp), %r8
	 movq 64(%rsp), %r10
	 movq %r10, goos_kernel_call_arg2(%rip)
	 movq goos_current_task(%rip), %rax
	 leaq goos_task_roots(%rip), %r9
	 movq (%r9,%rax,8), %r9
	 callq goos_dispatch_kernel_call
	 jmp .user_call_result
.user_call_wait:
	 movq 96(%rsp), %rcx
	 movq 88(%rsp), %rdx
	 movq 64(%rsp), %r8
	 movq goos_current_task(%rip), %rax
	 leaq goos_task_roots(%rip), %r9
	 movq (%r9,%rax,8), %r9
	 orq %rax, %r9
	 callq goos_dispatch_ipc_wait
	 jmp .user_call_result
.user_call_process_wait:
	 movq 96(%rsp), %rcx
	 movq 88(%rsp), %rdx
	 movq 64(%rsp), %r8
	 movq goos_current_task(%rip), %rax
	 leaq goos_task_roots(%rip), %r9
	 movq (%r9,%rax,8), %r9
	 orq %rax, %r9
	 callq goos_dispatch_process_wait
	 jmp .user_call_result
.user_call_irq_wait:
	 movq 96(%rsp), %rcx
	 movq goos_current_task(%rip), %rax
	 leaq goos_task_roots(%rip), %rdx
	 movq (%rdx,%rax,8), %rdx
	 orq %rax, %rdx
	 callq goos_dispatch_irq_wait
	 jmp .user_call_result
.user_call_stream_wait:
	 movq 96(%rsp), %rcx
	 movq 88(%rsp), %rdx
	 movq goos_current_task(%rip), %rax
	 leaq goos_task_roots(%rip), %r8
	 movq (%r8,%rax,8), %r8
	 orq %rax, %r8
	 callq goos_dispatch_stream_wait
	 jmp .user_call_result
.user_call_wait_any:
	 movq 96(%rsp), %rcx
	 movq 88(%rsp), %rdx
	 movq 64(%rsp), %r8
	 movq goos_current_task(%rip), %rax
	 leaq goos_task_roots(%rip), %r9
	 movq (%r9,%rax,8), %r9
	 orq %rax, %r9
	 callq goos_dispatch_wait_any
.user_call_result:
	 movq $0, 48(%rsp)
	 movl %eax, 48(%rsp)
	 shrq $32, %rax
	 testq %rax, %rax
	 jz .user_return
	 cmpq $1, %rax
	 je .user_exit
	 jmp .user_block
.user_return:
	 movq $0, 64(%rsp)
	 cmpq $0, goos_need_reschedule(%rip)
	 jne .user_yield
	 addq $48, %rsp
	 popq %rax
	 popq %rcx
	 popq %rdx
	 popq %rbx
	 popq %rbp
	 popq %rsi
	 popq %rdi
	 popq %r8
	 popq %r9
	 popq %r10
	 popq %r11
	 popq %r12
	 popq %r13
	 popq %r14
	 popq %r15
	 GOOS_RETURN_USER
	 iretq
.user_yield:
	 movq 48(%rsp), %rax
	 movq %rax, 40(%rsp)
	 xorq %r11, %r11
	 jmp .user_park
.user_block:
	 movq $2, %r11
.user_park:
	 movq %rsp, %r10
	 subq $160, %rsp
	 movq 160(%r10), %rax
	 movq %rax, 0(%rsp)
	 movq 152(%r10), %rax
	 movq %rax, 8(%rsp)
	 movq 144(%r10), %rax
	 movq %rax, 16(%rsp)
	 movq 136(%r10), %rax
	 movq %rax, 24(%rsp)
	 movq 128(%r10), %rax
	 movq %rax, 32(%rsp)
	 movq 120(%r10), %rax
	 movq %rax, 40(%rsp)
	 movq 112(%r10), %rax
	 movq %rax, 48(%rsp)
	 movq 104(%r10), %rax
	 movq %rax, 56(%rsp)
	 movq 96(%r10), %rax
	 movq %rax, 64(%rsp)
	 movq 88(%r10), %rax
	 movq %rax, 72(%rsp)
	 movq 80(%r10), %rax
	 movq %rax, 80(%rsp)
	 movq 64(%r10), %rax
	 movq %rax, 88(%rsp)
	 movq 56(%r10), %rax
	 movq %rax, 96(%rsp)
	 movq 72(%r10), %rax
	 movq %rax, 104(%rsp)
	 movq 40(%r10), %rax
	 movq %rax, 112(%rsp)
	 // A blocked call is rewound over its two-byte INT so it retries on wake;
	 // a yielding call has completed and resumes after it.
	 movq 168(%r10), %rax
	 subq %r11, %rax
	 movq %rax, 120(%rsp)
	 movq 176(%r10), %rax
	 movq %rax, 128(%rsp)
	 movq 184(%r10), %rax
	 movq %rax, 136(%rsp)
	 movq 192(%r10), %rax
	 movq %rax, 144(%rsp)
	 movq 200(%r10), %rax
	 movq %rax, 152(%rsp)
	 movq goos_current_task(%rip), %rcx
	 leaq goos_task_contexts(%rip), %rdx
	 movq %rsp, (%rdx,%rcx,8)
	 movq %rcx, %r9
	 shlq $GOOS_FPU_AREA_SHIFT, %r9
	 leaq goos_task_fpu(%rip), %r10
	 fxsave64 (%r10,%r9)
	 testq %r11, %r11
	 jz .user_park_select
	 btrq %rcx, goos_runnable_mask(%rip)
.user_park_select:
	 movq %rcx, %rax
	 callq goos_select_next
	 jmp .switch_to
.user_exit:
    addq $48, %rsp
    movq goos_current_task(%rip), %rcx
    btsq %rcx, goos_task_exit_pending(%rip)
    btrq %rcx, goos_runnable_mask(%rip)
    btsq %rcx, goos_task_dead_mask(%rip)
    btsq $GOOS_KERNEL_SLOT, goos_runnable_mask(%rip)
    movq %rcx, %rax
    callq goos_select_next
    jmp .switch_to

.section .text
.global goos_irq_timer
goos_irq_timer:
    GOOS_ENTER_LOW .L_irq_timer
.L_irq_timer:
    pushq %rax
    pushq %rbx
    pushq %rcx
    pushq %rdx
    pushq %rbp
    pushq %rsi
    pushq %rdi
    pushq %r8
    pushq %r9
    pushq %r10
    pushq %r11
    pushq %r12
    pushq %r13
    pushq %r14
    pushq %r15
    incq goos_timer_ticks(%rip)
    movq goos_current_task(%rip), %rax
    leaq goos_task_ticks(%rip), %rdx
    incq (%rdx,%rax,8)
    cmpq $0x23, 128(%rsp)
    jne .timer_not_user
    incq goos_user_preemptions(%rip)
.timer_not_user:
    movb $0x20, %al
    outb %al, $0x20
    cmpb $0, goos_scheduler_enabled(%rip)
    je .restore_context
    // The kernel task sleeps between housekeeping steps; each tick wakes it
    // to expire deadlines and retry retained lifecycle messages.
    btsq $GOOS_KERNEL_SLOT, goos_runnable_mask(%rip)
.preempt_current:
    movq goos_current_task(%rip), %rax
    leaq goos_task_contexts(%rip), %rdx
    movq %rsp, (%rdx,%rax,8)
    movq %rax, %r9
    shlq $GOOS_FPU_AREA_SHIFT, %r9
    leaq goos_task_fpu(%rip), %r10
    fxsave64 (%r10,%r9)
    callq goos_select_next
.switch_to:
	// FS base is schedulable user state. Save the outgoing value before RAX
	// changes from the incoming slot to the MSR halves.
	movq %rax, %r8
	movq goos_current_task(%rip), %r9
	movl $0xc0000100, %ecx
	rdmsr
	shlq $32, %rdx
	orq %rdx, %rax
	leaq goos_task_fs_bases(%rip), %r10
	movq %rax, (%r10,%r9,8)
	movq %r8, %rax
    movq %rax, %r9
    shlq $GOOS_FPU_AREA_SHIFT, %r9
    leaq goos_task_fpu(%rip), %r10
    fxrstor64 (%r10,%r9)
    movq %rax, goos_current_task(%rip)
	movq %rax, %r8
	leaq goos_task_fs_bases(%rip), %r10
	movq (%r10,%r8,8), %rax
	movq %rax, %rdx
	shrq $32, %rdx
	movl $0xc0000100, %ecx
	wrmsr
	movq %r8, %rax
    leaq goos_task_rsp0(%rip), %r10
    movq (%r10,%rax,8), %r11
    leaq goos_task_roots(%rip), %r9
    movq (%r9,%rax,8), %r9
    movq %r9, goos_linux_entry_data+GOOS_LINUX_ENTRY_PROCESS_ROOT(%rip)
    movq %r11, goos_linux_entry_data+GOOS_LINUX_ENTRY_STACK(%rip)
    testq %r11, %r11
    jz .switch_load
    movq goos_tss_rsp0_address(%rip), %r10
    movq %r11, (%r10)
.switch_load:
    leaq goos_task_contexts(%rip), %rdx
    movq (%rdx,%rax,8), %rsp
.restore_context:
    popq %r15
    popq %r14
    popq %r13
    popq %r12
    popq %r11
    popq %r10
    popq %r9
    popq %r8
    popq %rdi
    popq %rsi
    popq %rbp
    popq %rdx
    popq %rcx
    popq %rbx
    popq %rax
    GOOS_RETURN_USER
    iretq

// goos_irq_yield switches away from a kernel task without advancing time.
// Its vector is not callable from ring 3.
.global goos_irq_yield
goos_irq_yield:
    GOOS_ENTER_LOW .L_irq_yield
.L_irq_yield:
    pushq %rax
    pushq %rbx
    pushq %rcx
    pushq %rdx
    pushq %rbp
    pushq %rsi
    pushq %rdi
    pushq %r8
    pushq %r9
    pushq %r10
    pushq %r11
    pushq %r12
    pushq %r13
    pushq %r14
    pushq %r15
    cmpb $0, goos_scheduler_enabled(%rip)
    je .restore_context
    jmp .preempt_current

// goos_select_next returns in %rax the runnable slot after the one in %rax,
// in round-robin order. The idle slot is chosen only when no other task is
// runnable. Clobbers %r8.
goos_select_next:
    movq %rax, %r8
.select_loop:
    incq %rax
    cmpq goos_task_count(%rip), %rax
    jb .select_check
    xorq %rax, %rax
.select_check:
    cmpq $GOOS_IDLE_SLOT, %rax
    je .select_skip
    btq %rax, goos_runnable_mask(%rip)
    jc .select_done
.select_skip:
    cmpq %r8, %rax
    jne .select_loop
    movq $GOOS_IDLE_SLOT, %rax
.select_done:
    movq $0, goos_need_reschedule(%rip)
    retq

// goos_irq_serial drains every byte the UART has and returns. It takes no
// scheduling decision: the kernel loop sees the ring and wakes the waiters
// through the ordinary wait table, so one path publishes readiness.
.global goos_irq_serial
goos_irq_serial:
    GOOS_ENTER_LOW .L_irq_serial
.L_irq_serial:
    pushq %rax
    pushq %rcx
    pushq %rdx
    pushq %rsi
    pushq %rdi
    leaq goos_input_rings(%rip), %rdi
.serial_drain:
    movw $0x3fd, %dx
    inb %dx, %al
    testb $0x01, %al
    jz .serial_eoi
    movw $0x3f8, %dx
    inb %dx, %al
    cmpb $3, %al
    jne .serial_store
    incq GOOS_INPUT_INTERRUPT(%rdi)
    jmp .serial_drain
.serial_store:
    callq .input_enqueue
    jmp .serial_drain
.serial_eoi:
    movl $0, 0x3fffD0b0
    movb $0x20, %al
    outb %al, $0x20
    popq %rdi
    popq %rsi
    popq %rdx
    popq %rcx
    popq %rax
    GOOS_RETURN_USER
    iretq

// IRQ1 reads exactly one i8042 data byte. The bounded decoder returns at most
// one terminal key sequence (up to eight bytes, an F key is five), which is
// appended to the same ring as UART input. Arrival order is therefore the
// order in which IRQ handlers run. A sequence goes in whole or not at all: a
// ring without room for all of it drops the key rather than enqueue a prefix
// such as ESC [ 1 that would read as other keys.
.global goos_irq_keyboard
goos_irq_keyboard:
    GOOS_ENTER_LOW .L_irq_keyboard
.L_irq_keyboard:
    pushq %rax
    pushq %rbx
    pushq %rcx
    pushq %rdx
    pushq %rbp
    pushq %rsi
    pushq %rdi
    pushq %r8
    pushq %r9
    pushq %r10
    pushq %r11
    pushq %r12
    pushq %r13
    pushq %r14
    pushq %r15
    movw $0x64, %dx
    inb %dx, %al
    testb $0x01, %al
    jz .keyboard_eoi
    movw $0x60, %dx
    inb %dx, %al
    movzbq %al, %rcx
    leaq goos_keyboard_bytes(%rip), %rdx
    subq $32, %rsp
    callq goos_keyboard_feed
    addq $32, %rsp
    movq %rax, %rbx
    leaq goos_input_rings(%rip), %rdi
    addq $GOOS_INPUT_STRIDE, %rdi
    btq $32, %rbx
    jnc .keyboard_bytes
    incq GOOS_INPUT_INTERRUPT(%rdi)
    movq $1, goos_ps2_keyboard_event_pending(%rip)
.keyboard_bytes:
    movq %rbx, %r8
    andq $0xff, %r8
    jz .keyboard_eoi
    cmpq $GOOS_KEYBOARD_BYTES, %r8
    ja .keyboard_eoi
    // Free slots are (tail - head - 1) mod size; one slot stays empty so a
    // full ring is distinguishable from an empty one.
    movq GOOS_INPUT_TAIL(%rdi), %rax
    subq GOOS_INPUT_HEAD(%rdi), %rax
    decq %rax
    andq $GOOS_INPUT_RING_MASK, %rax
    cmpq %r8, %rax
    jae .keyboard_fits
    addq %r8, GOOS_INPUT_DROPPED(%rdi)
    jmp .keyboard_eoi
.keyboard_fits:
    leaq goos_keyboard_bytes(%rip), %r9
.keyboard_byte_loop:
    movzbq (%r9), %rax
    callq .input_enqueue
    testq %rax, %rax
    jz .keyboard_byte_next
    movq $1, goos_ps2_keyboard_event_pending(%rip)
.keyboard_byte_next:
    incq %r9
    decq %r8
    jnz .keyboard_byte_loop
.keyboard_eoi:
    movl $0, 0x3fffD0b0
    movb $0x20, %al
    outb %al, $0x20
    popq %r15
    popq %r14
    popq %r13
    popq %r12
    popq %r11
    popq %r10
    popq %r9
    popq %r8
    popq %rdi
    popq %rsi
    popq %rbp
    popq %rdx
    popq %rcx
    popq %rbx
    popq %rax
    GOOS_RETURN_USER
    iretq

// .input_enqueue appends AL to the input ring at RDI and returns one on
// success or zero when full. It clobbers RAX, RCX, RSI and RDX; interrupt
// entries preserve those registers.
.input_enqueue:
    movq GOOS_INPUT_HEAD(%rdi), %rcx
    movq %rcx, %rsi
    incq %rsi
    andq $GOOS_INPUT_RING_MASK, %rsi
    cmpq GOOS_INPUT_TAIL(%rdi), %rsi
    je .input_enqueue_drop
    movb %al, GOOS_INPUT_DATA(%rdi,%rcx,1)
    movq %rsi, GOOS_INPUT_HEAD(%rdi)
    movq $1, %rax
    retq
.input_enqueue_drop:
    incq GOOS_INPUT_DROPPED(%rdi)
    xorq %rax, %rax
    retq

.global goos_irq_11
goos_irq_11:
    GOOS_ENTER_LOW .L_irq_11
.L_irq_11:
    pushq %rax
    pushq %rbx
    pushq %rcx
    pushq %rdx
    pushq %rbp
    pushq %rsi
    pushq %rdi
    pushq %r8
    pushq %r9
    pushq %r10
    pushq %r11
    pushq %r12
    pushq %r13
    pushq %r14
    pushq %r15
    movl $0x26, 0x3fffe000
    movl 0x3fffe010, %eax
    orl $0x10000, %eax
    movl %eax, 0x3fffe010
    movq $1, goos_irq_11_pending(%rip)
    movq goos_irq_11_waiter(%rip), %rcx
    testq %rcx, %rcx
    jz .irq_11_eoi
    decq %rcx
    btsq %rcx, goos_runnable_mask(%rip)
    movq $1, goos_need_reschedule(%rip)
    movq $0, goos_irq_11_waiter(%rip)
.irq_11_eoi:
    movl $0, 0x3fffD0b0
    movb $0x20, %al
    outb %al, $0xa0
    outb %al, $0x20
    cmpq $0, goos_need_reschedule(%rip)
    je .irq_11_return
    cmpb $0, goos_scheduler_enabled(%rip)
    jne .preempt_current
.irq_11_return:
    popq %r15
    popq %r14
    popq %r13
    popq %r12
    popq %r11
    popq %r10
    popq %r9
    popq %r8
    popq %rdi
    popq %rsi
    popq %rbp
    popq %rdx
    popq %rcx
    popq %rbx
    popq %rax
    GOOS_RETURN_USER
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
    movl %ecx, %eax
    outb %al, $0x40
    movb %ch, %al
    outb %al, $0x40
    sti
    retq

.global goos_timer_ticks_read
goos_timer_ticks_read:
    movq goos_timer_ticks(%rip), %rax
    retq

// goos_task_ticks_read returns the ticks charged to one task slot, or zero for
// a slot outside the table.
.global goos_task_ticks_read
goos_task_ticks_read:
    xorq %rax, %rax
    cmpq $64, %rcx
    jae .task_ticks_done
    leaq goos_task_ticks(%rip), %rdx
    movq (%rdx,%rcx,8), %rax
.task_ticks_done:
    retq

// goos_task_fault_address_take returns and clears the address recorded for one
// task slot's last user fault, or zero for a slot outside the table.
.global goos_task_fault_address_take
goos_task_fault_address_take:
    xorq %rax, %rax
    cmpq $64, %rcx
    jae .task_fault_address_done
    leaq goos_task_fault_addresses(%rip), %rdx
    movq (%rdx,%rcx,8), %rax
    movq $0, (%rdx,%rcx,8)
.task_fault_address_done:
    retq

// goos_user_call_entry_count reports how many native calls reached the low
// handler after the high trampoline switched to the kernel page table.
.global goos_user_call_entry_count
goos_user_call_entry_count:
    movq goos_user_call_entries(%rip), %rax
    retq

// goos_user_call_entry_drift reports how often a task reached the low handler
// with an RSP different from that task's first observed entry RSP.
.global goos_user_call_entry_drift
goos_user_call_entry_drift:
    movq goos_user_call_rsp_drift(%rip), %rax
    retq

// goos_task_ticks_clear resets one slot's counter, so a slot reused by a new
// task does not inherit the previous one's time.
.global goos_task_ticks_clear
goos_task_ticks_clear:
    cmpq $64, %rcx
    jae .task_ticks_clear_done
    leaq goos_task_ticks(%rip), %rdx
    movq $0, (%rdx,%rcx,8)
.task_ticks_clear_done:
    retq

.global goos_scheduler_install
goos_scheduler_install:
    cli
    leaq goos_task_contexts(%rip), %rax
    movq %rcx, 8(%rax)
    movq %rdx, 16(%rax)
    movq %r8, 24(%rax)
    leaq goos_task_roots(%rip), %rax
    movq %r9, 0(%rax)
    movq %r9, 8(%rax)
    movq %r9, 16(%rax)
    movq %r9, 24(%rax)
    movq $4, goos_task_count(%rip)
    movq $0, goos_current_task(%rip)
    movq $0x0f, goos_runnable_mask(%rip)
    movb $1, goos_scheduler_enabled(%rip)
    sti
    retq

.global goos_scheduler_install_user
goos_scheduler_install_user:
    xorq %rax, %rax
    pushfq
    cli
    movq goos_task_dead_mask(%rip), %r10
    testq %r10, %r10
    jz .install_user_append
    leaq goos_task_rsp0(%rip), %r11
.install_user_dead_scan:
    // Only a dead slot whose exit record was already delivered may be reused:
    // reclaim zeroes rsp0 last, so a non-zero rsp0 means the exit is still
    // pending or being reclaimed. Reusing it would bump the generation and
    // orphan the exit record, leaving waiters blocked forever.
    bsfq %r10, %rax
    cmpq $0, (%r11,%rax,8)
    je .install_user_dead_found
    btrq %rax, %r10
    testq %r10, %r10
    jnz .install_user_dead_scan
    jmp .install_user_append
.install_user_dead_found:
    movq %rax, %r10
    btrq %r10, goos_task_dead_mask(%rip)
    jmp .install_user_slot
.install_user_append:
    xorq %rax, %rax
    movq goos_task_count(%rip), %r10
    cmpq $64, %r10
    jae .install_user_done
    incq goos_task_count(%rip)
.install_user_slot:
	 leaq goos_task_generations(%rip), %r11
	 incq (%r11,%r10,8)
	 jnz .install_user_generation_ready
	 incq (%r11,%r10,8)
.install_user_generation_ready:
    leaq goos_user_call_rsp_baselines(%rip), %r11
    movq $0, (%r11,%r10,8)
	leaq goos_task_fs_bases(%rip), %r11
	movq $0, (%r11,%r10,8)
    leaq goos_task_contexts(%rip), %r11
    movq %rcx, (%r11,%r10,8)
    leaq goos_task_roots(%rip), %r11
    movq %rdx, (%r11,%r10,8)
    leaq goos_task_rsp0(%rip), %r11
    movq %r8, (%r11,%r10,8)
    btsq %r10, goos_runnable_mask(%rip)
	 leaq goos_task_generations(%rip), %r11
	 movq (%r11,%r10,8), %rax
	 shlq $32, %rax
	 leaq 1(%r10,%rax), %rax
.install_user_done:
    popfq
    retq

.global goos_set_tss_stack_address
goos_set_tss_stack_address:
    movq %rcx, goos_tss_rsp0_address(%rip)
    retq

.global goos_task_fs_base_set
goos_task_fs_base_set:
	xorq %rax, %rax
	movl %ecx, %r8d
	testl %r8d, %r8d
	jz .task_fs_base_set_done
	cmpl $64, %r8d
	ja .task_fs_base_set_done
	decl %r8d
	movq %rcx, %r9
	shrq $32, %r9
	leaq goos_task_generations(%rip), %r10
	cmpq (%r10,%r8,8), %r9
	jne .task_fs_base_set_done
	btq %r8, goos_task_dead_mask(%rip)
	jc .task_fs_base_set_done
	pushfq
	cli
	leaq goos_task_fs_bases(%rip), %r10
	movq %rdx, (%r10,%r8,8)
	cmpq goos_current_task(%rip), %r8
	jne .task_fs_base_set_success
	movq %rdx, %r11
	movq %r11, %rax
	shrq $32, %rdx
	movl $0xc0000100, %ecx
	wrmsr
.task_fs_base_set_success:
	popfq
	movq $1, %rax
.task_fs_base_set_done:
	retq

.global goos_task_fs_base_read
goos_task_fs_base_read:
	xorq %rax, %rax
	movl %ecx, %r8d
	testl %r8d, %r8d
	jz .task_fs_base_read_done
	cmpl $64, %r8d
	ja .task_fs_base_read_done
	decl %r8d
	movq %rcx, %r9
	shrq $32, %r9
	leaq goos_task_generations(%rip), %r10
	cmpq (%r10,%r8,8), %r9
	jne .task_fs_base_read_done
	btq %r8, goos_task_dead_mask(%rip)
	jc .task_fs_base_read_done
	leaq goos_task_fs_bases(%rip), %r10
	movq (%r10,%r8,8), %r11
	movq %r11, (%rdx)
	movq $1, %rax
.task_fs_base_read_done:
	retq

.global goos_block_current_task
goos_block_current_task:
    pushfq
    cli
    movq goos_current_task(%rip), %rcx
    movq $1, %rax
    shlq %cl, %rax
    notq %rax
    andq %rax, goos_runnable_mask(%rip)
    popfq
    retq

.global goos_kernel_sleep
goos_kernel_sleep:
    pushfq
    cli
    movq goos_current_task(%rip), %rax
    btrq %rax, goos_runnable_mask(%rip)
    int $GOOS_YIELD_VECTOR
    popfq
    retq

.global goos_wake_task
goos_wake_task:
    xorq %rax, %rax
    cmpq goos_task_count(%rip), %rcx
    jae .wake_done
    pushfq
    cli
    btsq %rcx, goos_runnable_mask(%rip)
    movq $1, goos_need_reschedule(%rip)
    popfq
    movq $1, %rax
.wake_done:
    retq

.global goos_wake_task_id
goos_wake_task_id:
	 xorq %rax, %rax
	 movl %ecx, %r8d
	 testl %r8d, %r8d
	 jz .wake_id_done
	 cmpl $64, %r8d
	 ja .wake_id_done
	 decl %r8d
	 movq %rcx, %r9
	 shrq $32, %r9
	 leaq goos_task_generations(%rip), %rdx
	 cmpq (%rdx,%r8,8), %r9
	 jne .wake_id_done
	 btq %r8, goos_task_dead_mask(%rip)
	 jc .wake_id_done
	 pushfq
	 cli
	 btsq %r8, goos_runnable_mask(%rip)
	 movq $1, goos_need_reschedule(%rip)
	 popfq
	 movq $1, %rax
.wake_id_done:
	 retq

.global goos_task_id_for_slot
goos_task_id_for_slot:
	 xorq %rax, %rax
	 cmpq goos_task_count(%rip), %rcx
	 jae .task_id_for_slot_done
	 leaq goos_task_generations(%rip), %rdx
	 movq (%rdx,%rcx,8), %rax
	 testq %rax, %rax
	 jz .task_id_for_slot_done
	 shlq $32, %rax
	 leaq 1(%rcx,%rax), %rax
.task_id_for_slot_done:
	 retq

.global goos_set_current_task_exit_code
goos_set_current_task_exit_code:
    movq goos_current_task(%rip), %rax
    leaq goos_task_exit_codes(%rip), %rdx
    movq %rcx, (%rdx,%rax,8)
    retq

// goos_terminate_task(task, code) marks another task dead, the way a faulting
// task is retired. The caller has already cancelled its wait registrations.
.global goos_terminate_task
goos_terminate_task:
    xorq %rax, %rax
    movl %ecx, %r8d
    testl %r8d, %r8d
    jz .terminate_task_done
    cmpl $64, %r8d
    ja .terminate_task_done
    decl %r8d
    movq %rcx, %r9
    shrq $32, %r9
    leaq goos_task_generations(%rip), %r10
    cmpq (%r10,%r8,8), %r9
    jne .terminate_task_done
    cmpq $GOOS_KERNEL_SLOT, %r8
    je .terminate_task_done
    cmpq $GOOS_IDLE_SLOT, %r8
    je .terminate_task_done
    cmpq goos_current_task(%rip), %r8
    je .terminate_task_done
    pushfq
    cli
    btq %r8, goos_task_dead_mask(%rip)
    jc .terminate_task_locked
    leaq goos_task_exit_codes(%rip), %r10
    movq %rdx, (%r10,%r8,8)
    btsq %r8, goos_task_exit_pending(%rip)
    btrq %r8, goos_runnable_mask(%rip)
    btsq %r8, goos_task_dead_mask(%rip)
    movq $1, %rax
.terminate_task_locked:
    popfq
.terminate_task_done:
    retq

.global goos_take_exited_task
goos_take_exited_task:
    pushfq
    cli
    xorq %rax, %rax
    movq goos_task_exit_pending(%rip), %rdx
    testq %rdx, %rdx
    jz .take_exited_done
    bsfq %rdx, %rcx
    btrq %rcx, goos_task_exit_pending(%rip)
	 leaq goos_task_generations(%rip), %rax
	 movq (%rax,%rcx,8), %rax
	 shlq $32, %rax
	 leaq 1(%rcx,%rax), %rax
.take_exited_done:
    popfq
    retq

.global goos_task_exit_code
goos_task_exit_code:
    xorq %rax, %rax
	 movl %ecx, %r8d
	 testl %r8d, %r8d
    jz .task_exit_code_done
	 cmpl $64, %r8d
    ja .task_exit_code_done
	 decl %r8d
	 movq %rcx, %r9
	 shrq $32, %r9
	 leaq goos_task_generations(%rip), %rdx
	 cmpq (%rdx,%r8,8), %r9
	 jne .task_exit_code_done
    leaq goos_task_exit_codes(%rip), %rdx
	 movq (%rdx,%r8,8), %rax
	 movq $0, (%rdx,%r8,8)
.task_exit_code_done:
    retq

.global goos_reclaim_exited_task
goos_reclaim_exited_task:
    xorq %rax, %rax
	 movl %ecx, %r8d
	 testl %r8d, %r8d
    jz .reclaim_exited_done
	 cmpl $64, %r8d
    ja .reclaim_exited_done
	 decl %r8d
	 movq %rcx, %r9
	 shrq $32, %r9
	 leaq goos_task_generations(%rip), %rdx
	 cmpq (%rdx,%r8,8), %r9
	 jne .reclaim_exited_done
	 btq %r8, goos_task_dead_mask(%rip)
    jnc .reclaim_exited_done
	 btq %r8, goos_task_exit_pending(%rip)
    jc .reclaim_exited_done
    leaq goos_task_rsp0(%rip), %rdx
	 movq (%rdx,%r8,8), %rax
    testq %rax, %rax
    jz .reclaim_exited_done
	 movq $0, (%rdx,%r8,8)
    leaq goos_task_contexts(%rip), %rdx
	 movq $0, (%rdx,%r8,8)
	leaq goos_task_roots(%rip), %rdx
	movq $0, (%rdx,%r8,8)
	leaq goos_task_fs_bases(%rip), %rdx
	movq $0, (%rdx,%r8,8)
	movq $1, %rax
.reclaim_exited_done:
    retq

.global goos_invalidate_page
goos_invalidate_page:
    invlpg (%rcx)
    retq

.section .data
.align 8
goos_timer_ticks:
    .quad 0
goos_user_preemptions:
    .quad 0
goos_user_faults:
    .quad 0
goos_user_call_entries:
    .quad 0
goos_user_call_rsp_drift:
    .quad 0
goos_user_call_rsp_baselines:
    .zero 512
goos_current_task:
    .quad 0
goos_kernel_root:
    .quad 0
goos_task_count:
    .quad 1
goos_task_contexts:
    .zero 512
goos_task_roots:
    .zero 512
goos_task_fs_bases:
	.zero 512
goos_task_rsp0:
    .zero 512
goos_task_generations:
	.zero 512
goos_task_exit_codes:
    .zero 512
// The faulting address of each slot's last user page fault, zero after any
// other fault.
goos_task_fault_addresses:
    .zero 512
// One tick counter per task slot. The timer charges the task it interrupted,
// so the count is a sample of who was running rather than an exact duration,
// which is what a tick-driven scheduler can honestly report.
goos_task_ticks:
    .zero 512
goos_runnable_mask:
    .quad 1
goos_task_exit_pending:
    .quad 0
goos_task_dead_mask:
    .quad 0
goos_need_reschedule:
    .quad 0
.align 16
.global goos_task_fpu
goos_task_fpu:
    .zero 32768
goos_scheduler_enabled:
    .byte 0
goos_tss_rsp0_address:
    .quad 0
goos_kernel_call_arg2:
    .quad 0
.align 32
goos_linux_entry_data:
    .quad 0
    .quad 0
    .quad 0
    .quad 0
goos_irq_11_pending:
    .quad 0
goos_irq_11_waiter:
    .quad 0
goos_ps2_keyboard_event_pending:
    .quad 0
// Console input arrives faster than a tick, so each interrupt drains its
// device into its ring immediately and the kernel loop wakes readers from it.
// Draining in the handler is what keeps pasted input from being lost; the wake
// can wait for the next tick because the bytes are already safe. Ctrl-C is a
// signal, not input: the handler counts it in the ring's interrupt field so
// the terminal can see it without consuming the bytes a command is owed.
.align 16
goos_input_rings:
    .zero GOOS_INPUT_STRIDE * GOOS_INPUT_COUNT
// goos_keyboard_bytes receives the decoder's key sequence. Only IRQ1 uses it,
// with interrupts disabled, so it needs no lock.
goos_keyboard_bytes:
    .zero GOOS_KEYBOARD_BYTES

.section .text
// The input ring functions take the ring's index in RCX and leave its base in
// RDX; an index out of range answers as an empty ring.
.macro GOOS_INPUT_BASE empty
    cmpq $GOOS_INPUT_COUNT, %rcx
    jae \empty
    imulq $GOOS_INPUT_STRIDE, %rcx, %rcx
    leaq goos_input_rings(%rip), %rdx
    addq %rcx, %rdx
.endm

// goos_input_take removes one byte, returning it with bit 8 set, or zero when
// the ring is empty.
.global goos_input_take
goos_input_take:
    xorq %rax, %rax
    GOOS_INPUT_BASE .input_take_done
    movq GOOS_INPUT_TAIL(%rdx), %rcx
    cmpq GOOS_INPUT_HEAD(%rdx), %rcx
    je .input_take_done
    movzbq GOOS_INPUT_DATA(%rdx,%rcx,1), %rax
    orq $0x100, %rax
    incq %rcx
    andq $GOOS_INPUT_RING_MASK, %rcx
    movq %rcx, GOOS_INPUT_TAIL(%rdx)
.input_take_done:
    retq

// goos_input_pending reports whether a ring has anything for a reader: a
// byte, or an interrupt to deliver.
.global goos_input_pending
goos_input_pending:
    xorq %rax, %rax
    GOOS_INPUT_BASE .input_pending_done
    movq $1, %rax
    cmpq $0, GOOS_INPUT_INTERRUPT(%rdx)
    jne .input_pending_done
    movq GOOS_INPUT_TAIL(%rdx), %rcx
    cmpq GOOS_INPUT_HEAD(%rdx), %rcx
    jne .input_pending_done
    xorq %rax, %rax
.input_pending_done:
    retq

// goos_input_take_interrupt consumes a ring's pending interrupt.
.global goos_input_take_interrupt
goos_input_take_interrupt:
    xorq %rax, %rax
    GOOS_INPUT_BASE .input_interrupt_done
    cmpq $0, GOOS_INPUT_INTERRUPT(%rdx)
    je .input_interrupt_done
    movq $0, GOOS_INPUT_INTERRUPT(%rdx)
    movq $1, %rax
.input_interrupt_done:
    retq

// goos_input_dropped returns how many bytes arrived with a ring full.
.global goos_input_dropped
goos_input_dropped:
    xorq %rax, %rax
    GOOS_INPUT_BASE .input_dropped_done
    movq GOOS_INPUT_DROPPED(%rdx), %rax
.input_dropped_done:
    retq

// goos_ps2_keyboard_event_take returns and clears the indication that IRQ1
// successfully published keyboard input to the logical console.
.global goos_ps2_keyboard_event_take
goos_ps2_keyboard_event_take:
    xorq %rax, %rax
    cmpq $0, goos_ps2_keyboard_event_pending(%rip)
    je .ps2_keyboard_event_done
    movq $0, goos_ps2_keyboard_event_pending(%rip)
    movq $1, %rax
.ps2_keyboard_event_done:
    retq

// goos_enable_console_interrupt sets the UART's received-data-available bit.
// It runs after the IDT gate exists, so no byte can arrive without a handler.
.global goos_enable_console_interrupt
goos_enable_console_interrupt:
    movw $0x3f9, %dx
    movb $0x01, %al
    outb %al, %dx
    retq

.global goos_take_irq_11
goos_take_irq_11:
    xorq %rax, %rax
    cmpq $0, goos_irq_11_pending(%rip)
    je .take_irq_11_done
    decq goos_irq_11_pending(%rip)
    movq $1, %rax
.take_irq_11_done:
    retq

.global goos_set_irq_11_waiter
goos_set_irq_11_waiter:
    movq %rcx, goos_irq_11_waiter(%rip)
    retq

.global goos_rearm_irq_11
goos_rearm_irq_11:
    movl $0x26, 0x3fffe000
    movl $0xa02b, 0x3fffe010
    retq

.global goos_quiesce_irq_11
goos_quiesce_irq_11:
    movl $0x26, 0x3fffe000
    movl $0x1a02b, 0x3fffe010
    movq $0, goos_irq_11_pending(%rip)
    movq $0, goos_irq_11_waiter(%rip)
    retq

.section .text
.global goos_user_preemptions_read
goos_user_preemptions_read:
    movq goos_user_preemptions(%rip), %rax
    retq

.global goos_user_faults_read
goos_user_faults_read:
    movq goos_user_faults(%rip), %rax
    retq

.global goos_kernel_call_arg2_read
goos_kernel_call_arg2_read:
    movq goos_kernel_call_arg2(%rip), %rax
    retq

.section .rdata
.align 8
.global goos_task_entry_table
goos_task_entry_table:
    .quad goos_demo_task_a, goos_demo_task_b, goos_idle_task
