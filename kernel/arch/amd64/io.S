//go:build uefi && amd64

.set GOOS_KERNEL_SLOT, 0
.set GOOS_IDLE_SLOT, 3
.set GOOS_YIELD_VECTOR, 0x81
.set GOOS_FPU_AREA_SHIFT, 9
.set GOOS_CR4_SSE, 0x600

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

.global goos_load_cr3
goos_load_cr3:
    movq %rcx, %cr3
    retq

.global goos_read_rsp
goos_read_rsp:
    movq %rsp, %rax
    retq

.global goos_probe_address_spaces
goos_probe_address_spaces:
    pushfq
    cli
    movq %rcx, %cr3
    movzbq (%r9), %r10
    movq %rdx, %cr3
    movzbq (%r9), %r11
    movq %r8, %cr3
    movq %r11, %rax
    shlq $8, %rax
    orq %r10, %rax
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
    cmpq $0x23, 24(%rsp)
    je .terminate_user_fault
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

.terminate_user_fault:
    incq goos_user_faults(%rip)
    movq goos_current_task(%rip), %rcx
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

.section .text
.global goos_user_call
goos_user_call:
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
    movq %rax, %r9
    shlq $GOOS_FPU_AREA_SHIFT, %r9
    leaq goos_task_fpu(%rip), %r10
    fxrstor64 (%r10,%r9)
    movq %rax, goos_current_task(%rip)
    leaq goos_task_roots(%rip), %r10
    movq (%r10,%rax,8), %r11
    movq %cr3, %r10
    cmpq %r10, %r11
    je .switch_rsp0
    movq %r11, %cr3
.switch_rsp0:
    leaq goos_task_rsp0(%rip), %r10
    movq (%r10,%rax,8), %r11
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
    iretq

// goos_irq_yield switches away from a kernel task without advancing time.
// Its vector is not callable from ring 3.
.global goos_irq_yield
goos_irq_yield:
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

.global goos_irq_11
goos_irq_11:
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
    addq $64, %rax
    subq $20480, %rax
	 movq $0, (%rdx,%r8,8)
    leaq goos_task_contexts(%rip), %rdx
	 movq $0, (%rdx,%r8,8)
    leaq goos_task_roots(%rip), %rdx
	 movq $0, (%rdx,%r8,8)
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
goos_current_task:
    .quad 0
goos_task_count:
    .quad 1
goos_task_contexts:
    .zero 512
goos_task_roots:
    .zero 512
goos_task_rsp0:
    .zero 512
goos_task_generations:
	.zero 512
goos_task_exit_codes:
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
goos_irq_11_pending:
    .quad 0
goos_irq_11_waiter:
    .quad 0

.section .text
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
