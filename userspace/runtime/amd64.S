.section .text.entry,"ax",@progbits
.global _start
.type _start,@function
_start:
    andq $-16, %rsp
    movq %rsp, goos_stack_top(%rip)
    xorl %esi, %esi
    call goos_tcb_install
    call userspaceMain
    // A program built with the goroutine runtime ends when its main
    // function returns; any other program never returns here.
    movq $goos_main_returned, %rax
    testq %rax, %rax
    jz 1f
    call *%rax
1:
    ud2
.size _start, .-_start
.weak goos_main_returned

// Thread control blocks (ADR 0204). Every thread's FS base points at the
// block of its slot, so the runtime finds the thread it runs on with one
// FS-relative load, whatever stack it runs on. A block is 64 bytes:
//
//	0   the block's own address
//	8   the thread's slot
//	16  GOOS_TCB_MAGIC
//	24  the goroutine runtime's record of the thread (M), or zero
//	32  the pin count: nonzero while the thread uses state that belongs
//	    to it, such as its system-call descriptors or allocator caches,
//	    and must not be preempted onto another goroutine
//	40  the goroutine the thread runs, or zero
//	48  the thread's ID, once the runtime has read it
//	56  the TinyGo task of the code the thread runs, in the goroutine
//	    runtime: the running goroutine's, or the scheduler stack's
.set GOOS_TCB_MAGIC, 0x31424354534f4f47
.section .bss.goos_tcbs,"aw",@nobits
.balign 64
.global goos_tcbs
goos_tcbs:
    .zero 64 * 16

// goos_tcb_install makes the block of the slot in rsi the calling thread's
// and sets its FS base to it. Only rax, rcx, and rdx change. A kernel that
// refuses the base leaves the thread unable to name itself, so it stops.
.section .text.goos_tcb_install,"ax",@progbits
.global goos_tcb_install
.type goos_tcb_install,@function
goos_tcb_install:
    pushq %rdi
    pushq %rsi
    movq %rsi, %rax
    shlq $6, %rax
    leaq goos_tcbs(%rip), %rdi
    addq %rax, %rdi
    movq %rdi, 0(%rdi)
    movq %rsi, 8(%rdi)
    movabsq $GOOS_TCB_MAGIC, %rax
    movq %rax, 16(%rdi)
    movq $0, 24(%rdi)
    movq $0, 32(%rdi)
    movq $0, 40(%rdi)
    movq $0, 48(%rdi)
    movq $0, 56(%rdi)
    movl $66, %eax
    int $0x80
    testq %rax, %rax
    jnz 1f
    popq %rsi
    popq %rdi
    ret
1:
    ud2
.size goos_tcb_install, .-goos_tcb_install

// goos_g_current_task() *task.Task returns the task of the code that runs,
// for TinyGo's task.Current. One FS-relative load reads it, so a goroutine
// switched to another thread between two instructions never reads another
// thread's. A thread the goroutine runtime has not set up yet takes the
// slow path.
.weak goos_g_current_task_slow
.global goos_g_current_task
.type goos_g_current_task,@function
goos_g_current_task:
    movq %fs:56, %rax
    testq %rax, %rax
    jz 1f
    ret
1:
    jmp goos_g_current_task_slow
.size goos_g_current_task, .-goos_g_current_task

// goos_tls_block() uintptr returns the calling thread's control block.
.global goos_tls_block
.type goos_tls_block,@function
goos_tls_block:
    movq %fs:0, %rax
    ret
.size goos_tls_block, .-goos_tls_block

// goos_tls_slot() uint64 returns the calling thread's slot.
.global goos_tls_slot
.type goos_tls_slot,@function
goos_tls_slot:
    movq %fs:8, %rax
    ret
.size goos_tls_slot, .-goos_tls_slot

// goos_tls_pin() uint64 raises the calling thread's pin count and returns
// its slot; goos_tls_unpin() lowers the count again.
.global goos_tls_pin
.type goos_tls_pin,@function
goos_tls_pin:
    incq %fs:32
    movq %fs:8, %rax
    ret
.size goos_tls_pin, .-goos_tls_pin

.global goos_tls_unpin
.type goos_tls_unpin,@function
goos_tls_unpin:
    decq %fs:32
    ret
.size goos_tls_unpin, .-goos_tls_unpin

// goos_gc_registers(buffer *[6]uintptr) uintptr stores the callee-saved
// registers in buffer and returns the stack pointer its caller resumes with.
// The collector scans both: a pointer the compiler keeps only in a register
// of a frame up the stack is then still found.
.section .text.goos_gc_registers,"ax",@progbits
.global goos_gc_registers
.type goos_gc_registers,@function
goos_gc_registers:
    movq %rbx, 0(%rdi)
    movq %rbp, 8(%rdi)
    movq %r12, 16(%rdi)
    movq %r13, 24(%rdi)
    movq %r14, 32(%rdi)
    movq %r15, 40(%rdi)
    leaq 8(%rsp), %rax
    ret
.size goos_gc_registers, .-goos_gc_registers

// goos_gc_scrub(limit uintptr) zeroes the stack below its caller's frame:
// the 4 KiB under its own return address, or from limit up when limit is
// higher. The caller passes the lowest address of the stack reservation, so
// no write leaves the stack; the bytes zeroed lie below every live frame and
// below the return address, and only rax, rcx, rdi, and the direction flag,
// all caller-saved, change. The collector's frames are then built on zeroes:
// a slot one of them reserves but has not written yet holds no address an
// earlier call left there, which the conservative scan would take for a
// root. tools/stackframes knows this function as the one that writes below
// the red zone, and checks its bound (ADR 0202).
.section .text.goos_gc_scrub,"ax",@progbits
.global goos_gc_scrub
.type goos_gc_scrub,@function
goos_gc_scrub:
    leaq -4096(%rsp), %rax
    cmpq %rdi, %rax
    cmovbq %rdi, %rax
    movq %rsp, %rcx
    subq %rax, %rcx
    jbe 1f
    shrq $3, %rcx
    movq %rax, %rdi
    xorl %eax, %eax
    cld
    rep stosq
1:
    ret
.size goos_gc_scrub, .-goos_gc_scrub

// goos_stack_reserve() uintptr returns _goos_stack_reserve, the stack the
// program asks the kernel for (ADR 0104). The value is an immediate in the
// code: a Go reference to the absolute symbol put a word holding it after the
// globals, and the nonzero tail kept elf2native from dropping the zeros
// before it, so every image stored its whole zero data.
.section .text.goos_stack_reserve,"ax",@progbits
.global goos_stack_reserve
.type goos_stack_reserve,@function
goos_stack_reserve:
    movl $_goos_stack_reserve, %eax
    ret
.size goos_stack_reserve, .-goos_stack_reserve

// goos_stack_top is the stack pointer _start called the program with: the
// upper end of every frame the collector scans.
.section .bss.goos_stack_top,"aw",@nobits
.balign 8
.global goos_stack_top
goos_stack_top:
    .zero 8

.section .text.kcall,"ax",@progbits
.global goos_ipc_send
.type goos_ipc_send,@function
goos_ipc_send:
    movl $3, %eax
    int $0x80
    ret
.size goos_ipc_send, .-goos_ipc_send

.global goos_ipc_receive
.type goos_ipc_receive,@function
goos_ipc_receive:
    movl $4, %eax
    int $0x80
    ret
.size goos_ipc_receive, .-goos_ipc_receive

.global goos_ipc_wait_receive
.type goos_ipc_wait_receive,@function
goos_ipc_wait_receive:
    movl $5, %eax
    int $0x80
    ret
.size goos_ipc_wait_receive, .-goos_ipc_wait_receive

.global goos_console_write
.type goos_console_write,@function
goos_console_write:
    movl $6, %eax
    int $0x80
    ret
.size goos_console_write, .-goos_console_write

.global goos_console_read
.type goos_console_read,@function
goos_console_read:
    movl $7, %eax
    int $0x80
    ret
.size goos_console_read, .-goos_console_read

.global goos_clock_read
.type goos_clock_read,@function
goos_clock_read:
    movl $8, %eax
    int $0x80
    ret
.size goos_clock_read, .-goos_clock_read

.global goos_startup_read
.type goos_startup_read,@function
goos_startup_read:
    movl $9, %eax
    int $0x80
    ret
.size goos_startup_read, .-goos_startup_read

.global goos_channel_create
.type goos_channel_create,@function
goos_channel_create:
    movl $10, %eax
    int $0x80
    ret
.size goos_channel_create, .-goos_channel_create

.global goos_handle_close
.type goos_handle_close,@function
goos_handle_close:
    movl $11, %eax
    int $0x80
    ret
.size goos_handle_close, .-goos_handle_close

.global goos_process_wait
.type goos_process_wait,@function
goos_process_wait:
    movl $12, %eax
    int $0x80
    ret
.size goos_process_wait, .-goos_process_wait

.global goos_system_info
.type goos_system_info,@function
goos_system_info:
    movl $13, %eax
    int $0x80
    ret
.size goos_system_info, .-goos_system_info

.global goos_port_read
.type goos_port_read,@function
goos_port_read:
    movl $14, %eax
    int $0x80
    ret
.size goos_port_read, .-goos_port_read

.global goos_port_write
.type goos_port_write,@function
goos_port_write:
    movl $15, %eax
    int $0x80
    ret
.size goos_port_write, .-goos_port_write

.global goos_dma_allocate
.type goos_dma_allocate,@function
goos_dma_allocate:
    movl $16, %eax
    int $0x80
    ret
.size goos_dma_allocate, .-goos_dma_allocate

.global goos_irq_wait
.type goos_irq_wait,@function
goos_irq_wait:
    movl $17, %eax
    int $0x80
    ret
.size goos_irq_wait, .-goos_irq_wait

.global goos_irq_poll
.type goos_irq_poll,@function
goos_irq_poll:
    movl $71, %eax
    int $0x80
    ret
.size goos_irq_poll, .-goos_irq_poll

.global goos_stream_create
.type goos_stream_create,@function
goos_stream_create:
    movl $18, %eax
    int $0x80
    ret
.size goos_stream_create, .-goos_stream_create

.global goos_stream_read
.type goos_stream_read,@function
goos_stream_read:
    movl $19, %eax
    int $0x80
    ret
.size goos_stream_read, .-goos_stream_read

.global goos_stream_write
.type goos_stream_write,@function
goos_stream_write:
    movl $20, %eax
    int $0x80
    ret
.size goos_stream_write, .-goos_stream_write

// Panic reporting writes one byte without Go heap allocations or M-local
// syscall scratch. RDI is the stderr stream handle and SIL is the byte.
.global goos_panic_write_byte
.type goos_panic_write_byte,@function
goos_panic_write_byte:
    subq $40, %rsp
    movb %sil, 24(%rsp)
    leaq 24(%rsp), %rax
    movq %rax, 0(%rsp)
    movq $1, 8(%rsp)
    movq $0, 16(%rsp)
    movq %rsp, %rsi
    movl $24, %edx
    movl $20, %eax
    int $0x80
    addq $40, %rsp
    ret
.size goos_panic_write_byte, .-goos_panic_write_byte

.global goos_stream_wait
.type goos_stream_wait,@function
goos_stream_wait:
    movl $21, %eax
    int $0x80
    ret
.size goos_stream_wait, .-goos_stream_wait

.global goos_wait_any
.type goos_wait_any,@function
goos_wait_any:
    movl $22, %eax
    int $0x80
    ret
.size goos_wait_any, .-goos_wait_any

.global goos_diagnostics
.type goos_diagnostics,@function
goos_diagnostics:
    movl $23, %eax
    int $0x80
    ret
.size goos_diagnostics, .-goos_diagnostics

.global goos_clock_read_nanos
.type goos_clock_read_nanos,@function
goos_clock_read_nanos:
    movl $24, %eax
    int $0x80
    ret
.size goos_clock_read_nanos, .-goos_clock_read_nanos

.global goos_log_write
.type goos_log_write,@function
goos_log_write:
    movl $25, %eax
    int $0x80
    ret
.size goos_log_write, .-goos_log_write

.global goos_log_read
.type goos_log_read,@function
goos_log_read:
    movl $26, %eax
    int $0x80
    ret
.size goos_log_read, .-goos_log_read

.global goos_handle_info
.type goos_handle_info,@function
goos_handle_info:
    movl $27, %eax
    int $0x80
    ret
.size goos_handle_info, .-goos_handle_info

.global goos_process_info
.type goos_process_info,@function
goos_process_info:
    movl $28, %eax
    int $0x80
    ret
.size goos_process_info, .-goos_process_info

.global goos_system_reset
.type goos_system_reset,@function
goos_system_reset:
    movl $29, %eax
    int $0x80
    ret
.size goos_system_reset, .-goos_system_reset

.global goos_memory_map
.type goos_memory_map,@function
goos_memory_map:
    movl $30, %eax
    int $0x80
    ret
.size goos_memory_map, .-goos_memory_map

.global goos_process_kill
.type goos_process_kill,@function
goos_process_kill:
    movl $31, %eax
    int $0x80
    ret
.size goos_process_kill, .-goos_process_kill

.global goos_image_map
.type goos_image_map,@function
goos_image_map:
    movl $32, %eax
    int $0x80
    ret
.size goos_image_map, .-goos_image_map

.global goos_log_bounds
.type goos_log_bounds,@function
goos_log_bounds:
    movl $33, %eax
    int $0x80
    ret
.size goos_log_bounds, .-goos_log_bounds

.global goos_console_interrupt
.type goos_console_interrupt,@function
goos_console_interrupt:
    movl $34, %eax
    int $0x80
    ret
.size goos_console_interrupt, .-goos_console_interrupt

.global goos_console_info
.type goos_console_info,@function
goos_console_info:
    movl $49, %eax
    int $0x80
    ret
.size goos_console_info, .-goos_console_info

.global goos_cpu_info
.type goos_cpu_info,@function
goos_cpu_info:
    movl $50, %eax
    int $0x80
    ret
.size goos_cpu_info, .-goos_cpu_info

.global goos_cpu_entry
.type goos_cpu_entry,@function
goos_cpu_entry:
    movl $51, %eax
    int $0x80
    ret
.size goos_cpu_entry, .-goos_cpu_entry

.global goos_cpu_cache_info
.type goos_cpu_cache_info,@function
goos_cpu_cache_info:
    movl $52, %eax
    int $0x80
    ret
.size goos_cpu_cache_info, .-goos_cpu_cache_info

.global goos_cpu_runtime
.type goos_cpu_runtime,@function
goos_cpu_runtime:
    movl $53, %eax
    int $0x80
    ret
.size goos_cpu_runtime, .-goos_cpu_runtime

.global goos_task_affinity
.type goos_task_affinity,@function
goos_task_affinity:
    movl $54, %eax
    int $0x80
    ret
.size goos_task_affinity, .-goos_task_affinity

.global goos_irq_take
.type goos_irq_take,@function
goos_irq_take:
    movl $55, %eax
    int $0x80
    ret
.size goos_irq_take, .-goos_irq_take

.global goos_random_read
.type goos_random_read,@function
goos_random_read:
    movl $56, %eax
    int $0x80
    ret
.size goos_random_read, .-goos_random_read

.global goos_validation_fault
.type goos_validation_fault,@function
goos_validation_fault:
    movl $35, %eax
    int $0x80
    ret
.size goos_validation_fault, .-goos_validation_fault

.global goos_system_power_off
.type goos_system_power_off,@function
goos_system_power_off:
    movl $36, %eax
    int $0x80
    ret
.size goos_system_power_off, .-goos_system_power_off

.global goos_power_off_ready
.type goos_power_off_ready,@function
goos_power_off_ready:
    movl $57, %eax
    int $0x80
    ret
.size goos_power_off_ready, .-goos_power_off_ready

.global goos_executable_create
.type goos_executable_create,@function
goos_executable_create:
    movl $37, %eax
    int $0x80
    ret
.size goos_executable_create, .-goos_executable_create

.global goos_executable_write
.type goos_executable_write,@function
goos_executable_write:
    movl $38, %eax
    int $0x80
    ret
.size goos_executable_write, .-goos_executable_write

.global goos_vm_map
.type goos_vm_map,@function
goos_vm_map:
    movl $39, %eax
    int $0x80
    ret
.size goos_vm_map, .-goos_vm_map

.global goos_vm_unmap
.type goos_vm_unmap,@function
goos_vm_unmap:
    movl $40, %eax
    int $0x80
    ret
.size goos_vm_unmap, .-goos_vm_unmap

.global goos_vm_protect
.type goos_vm_protect,@function
goos_vm_protect:
    movl $41, %eax
    int $0x80
    ret
.size goos_vm_protect, .-goos_vm_protect

.global goos_vm_discard
.type goos_vm_discard,@function
goos_vm_discard:
    movl $42, %eax
    int $0x80
    ret
.size goos_vm_discard, .-goos_vm_discard

.global goos_vm_query
.type goos_vm_query,@function
goos_vm_query:
    movl $43, %eax
    int $0x80
    ret
.size goos_vm_query, .-goos_vm_query

.global goos_memory_create
.type goos_memory_create,@function
goos_memory_create:
    movl $44, %eax
    int $0x80
    ret
.size goos_memory_create, .-goos_memory_create

.global goos_handle_duplicate
.type goos_handle_duplicate,@function
goos_handle_duplicate:
    movl $45, %eax
    int $0x80
    ret
.size goos_handle_duplicate, .-goos_handle_duplicate

.global goos_clock_now
.type goos_clock_now,@function
goos_clock_now:
    movl $46, %eax
    int $0x80
    ret
.size goos_clock_now, .-goos_clock_now

.global goos_clock_set
.type goos_clock_set,@function
goos_clock_set:
    movl $47, %eax
    int $0x80
    ret
.size goos_clock_set, .-goos_clock_set

.global goos_executable_source
.type goos_executable_source,@function
goos_executable_source:
    movl $48, %eax
    int $0x80
    ret
.size goos_executable_source, .-goos_executable_source

.global goos_exit
.type goos_exit,@function
goos_exit:
    movl $1, %eax
    int $0x80
    ud2
.size goos_exit, .-goos_exit

// The native thread calls (ADR 0203), operations 59 to 65.
.global goos_thread_create
.type goos_thread_create,@function
goos_thread_create:
    movl $59, %eax
    int $0x80
    ret
.size goos_thread_create, .-goos_thread_create

.global goos_thread_exit
.type goos_thread_exit,@function
goos_thread_exit:
    movl $60, %eax
    int $0x80
    ud2
.size goos_thread_exit, .-goos_thread_exit

.global goos_thread_join
.type goos_thread_join,@function
goos_thread_join:
    movl $61, %eax
    int $0x80
    ret
.size goos_thread_join, .-goos_thread_join

.global goos_thread_self
.type goos_thread_self,@function
goos_thread_self:
    movl $62, %eax
    int $0x80
    ret
.size goos_thread_self, .-goos_thread_self

.global goos_thread_yield
.type goos_thread_yield,@function
goos_thread_yield:
    movl $63, %eax
    int $0x80
    ret
.size goos_thread_yield, .-goos_thread_yield

.global goos_thread_suspend
.type goos_thread_suspend,@function
goos_thread_suspend:
    movl $64, %eax
    int $0x80
    ret
.size goos_thread_suspend, .-goos_thread_suspend

.global goos_thread_resume
.type goos_thread_resume,@function
goos_thread_resume:
    movl $65, %eax
    int $0x80
    ret
.size goos_thread_resume, .-goos_thread_resume

// The goroutine runtime's calls (ADR 0204), operations 66 to 70.
.global goos_thread_set_base
.type goos_thread_set_base,@function
goos_thread_set_base:
    movl $66, %eax
    int $0x80
    ret
.size goos_thread_set_base, .-goos_thread_set_base

.global goos_thread_preempt
.type goos_thread_preempt,@function
goos_thread_preempt:
    movl $67, %eax
    int $0x80
    ret
.size goos_thread_preempt, .-goos_thread_preempt

.global goos_thread_wake
.type goos_thread_wake,@function
goos_thread_wake:
    movl $69, %eax
    int $0x80
    ret
.size goos_thread_wake, .-goos_thread_wake

.global goos_thread_wake_take
.type goos_thread_wake_take,@function
goos_thread_wake_take:
    movl $70, %eax
    int $0x80
    ret
.size goos_thread_wake_take, .-goos_thread_wake_take

// goos_thread_entry is where the kernel starts a thread ThreadCreate made:
// at its stack top, 16-byte aligned, with the runtime's argument in rdi and
// the thread's slot in rsi. It calls goos_thread_main, which runs the
// thread's function and ends the thread; a program that links no thread
// support leaves the symbol undefined and never creates a thread.
.weak goos_thread_main
.global goos_thread_entry
.type goos_thread_entry,@function
goos_thread_entry:
    andq $-16, %rsp
    xorl %ebp, %ebp
    call goos_tcb_install
    call goos_thread_main
    ud2
.size goos_thread_entry, .-goos_thread_entry

// goos_worker_entry starts a thread the goroutine runtime made to run
// goroutines (ADR 0204), as goos_thread_entry does for thread.Start.
.weak goos_worker_main
.global goos_worker_entry
.type goos_worker_entry,@function
goos_worker_entry:
    andq $-16, %rsp
    xorl %ebp, %ebp
    call goos_tcb_install
    call goos_worker_main
    ud2
.size goos_worker_entry, .-goos_worker_entry

.global goos_worker_entry_address
.type goos_worker_entry_address,@function
goos_worker_entry_address:
    leaq goos_worker_entry(%rip), %rax
    ret
.size goos_worker_entry_address, .-goos_worker_entry_address

// goos_g_swap(next uintptr, save *uintptr) switches stacks: it pushes the
// callee-saved registers, stores the stack pointer at save, loads next, and
// pops the registers saved there. The System V ABI makes every other
// register, and every XMM register, the caller's to save.
.section .text.goos_g_swap,"ax",@progbits
.global goos_g_swap
.type goos_g_swap,@function
goos_g_swap:
.global goos_g_swap_start
goos_g_swap_start:
    pushq %r15
    pushq %r14
    pushq %r13
    pushq %r12
    pushq %rbp
    pushq %rbx
    movq %rsp, (%rsi)
    movq %rdi, %rsp
    popq %rbx
    popq %rbp
    popq %r12
    popq %r13
    popq %r14
    popq %r15
    ret
// goos_g_swap_start and goos_g_swap_end bound the switch for the
// collector's validation counts, as the preempt_entry labels bound the entry.
.global goos_g_swap_end
goos_g_swap_end:
.size goos_g_swap, .-goos_g_swap

// goos_g_entry is the first frame of a goroutine. Its first swap pops r12,
// the compiler's start wrapper, and r13, the wrapper's argument bundle.
// goos_g_begin first drops the pin the scheduler stack handed over.
.weak goos_g_exit
.weak goos_g_begin
.global goos_g_entry
.type goos_g_entry,@function
goos_g_entry:
    xorl %ebp, %ebp
    call goos_g_begin
    movq %r13, %rdi
    callq *%r12
    xorl %edi, %edi
    call goos_g_exit
    ud2
.size goos_g_entry, .-goos_g_entry

.global goos_g_entry_address
.type goos_g_entry_address,@function
goos_g_entry_address:
    leaq goos_g_entry(%rip), %rax
    ret
.size goos_g_entry_address, .-goos_g_entry_address

// goos_g_mstart is the first frame of a scheduler stack the runtime made
// for a thread: it runs goos_g_mloop with r13, which never returns.
.weak goos_g_mloop
.global goos_g_mstart
.type goos_g_mstart,@function
goos_g_mstart:
    xorl %ebp, %ebp
    movq %r13, %rdi
    call goos_g_mloop
    ud2
.size goos_g_mstart, .-goos_g_mstart

.global goos_g_mstart_address
.type goos_g_mstart_address,@function
goos_g_mstart_address:
    leaq goos_g_mstart(%rip), %rax
    ret
.size goos_g_mstart_address, .-goos_g_mstart_address

// goos_g_stack_pointer() uintptr returns its caller's stack pointer.
.global goos_g_stack_pointer
.type goos_g_stack_pointer,@function
goos_g_stack_pointer:
    leaq 8(%rsp), %rax
    ret
.size goos_g_stack_pointer, .-goos_g_stack_pointer

// goos_preempt_entry is where the kernel diverts a thread that registered
// it with ThreadPreempt (ADR 0204). It arrives with every register as the
// interrupted code left it, except RIP, which the kernel keeps, and RSP,
// lowered by the 128-byte red zone. It saves the flags, the fifteen
// general-purpose registers, and the FPU and SSE state on that stack, takes
// the interrupted RIP from the kernel into the slot above them, and calls
// goos_g_preempt with the direction flag clear. The SSE state is saved before
// the take: a native call keeps only the general-purpose registers, and the
// interrupted code may hold live XMM values. Restoring everything and
// returning through the slot with RET 128 resumes the interrupted code with
// its stack pointer and red zone as they were. goos_g_preempt may switch
// goroutines; the frame stays on this stack until the goroutine runs again.
.weak goos_g_preempt
.section .text.goos_preempt_entry,"ax",@progbits
.global goos_preempt_entry
.type goos_preempt_entry,@function
goos_preempt_entry:
.global goos_preempt_entry_start
goos_preempt_entry_start:
    pushq $0
    pushfq
    pushq %rax
    pushq %rbx
    pushq %rcx
    pushq %rdx
    pushq %rsi
    pushq %rdi
    pushq %rbp
    pushq %r8
    pushq %r9
    pushq %r10
    pushq %r11
    pushq %r12
    pushq %r13
    pushq %r14
    pushq %r15
    movq %rsp, %rbx
    subq $512, %rsp
    andq $-16, %rsp
    fxsave64 (%rsp)
    leaq 128(%rbx), %rdi
    movl $68, %eax
    int $0x80
    testq %rax, %rax
    jnz 1f
    cld
    call goos_g_preempt
    fxrstor64 (%rsp)
    movq %rbx, %rsp
    popq %r15
    popq %r14
    popq %r13
    popq %r12
    popq %r11
    popq %r10
    popq %r9
    popq %r8
    popq %rbp
    popq %rdi
    popq %rsi
    popq %rdx
    popq %rcx
    popq %rbx
    popq %rax
    popfq
    ret $128
1:
    ud2
.global goos_preempt_entry_end
goos_preempt_entry_end:
.size goos_preempt_entry, .-goos_preempt_entry

.global goos_preempt_entry_address
.type goos_preempt_entry_address,@function
goos_preempt_entry_address:
    leaq goos_preempt_entry(%rip), %rax
    ret
.size goos_preempt_entry_address, .-goos_preempt_entry_address

// goos_thread_entry_address() uintptr returns where goos_thread_entry lies,
// from the code, so no data word holds it.
.global goos_thread_entry_address
.type goos_thread_entry_address,@function
goos_thread_entry_address:
    leaq goos_thread_entry(%rip), %rax
    ret
.size goos_thread_entry_address, .-goos_thread_entry_address

// goos_probe_vector_spin(stop *uint32) uint64 fills XMM0 to XMM14 with a
// pattern and loops without calls until *stop is nonzero. It returns how
// many times a register was found without the pattern, which it then puts
// back. Only goprobe calls it, to check that a preemption keeps the SSE
// state of the code it interrupts.
.section .text.goos_probe_vector_spin,"ax",@progbits
.global goos_probe_vector_spin
.type goos_probe_vector_spin,@function
goos_probe_vector_spin:
    subq $24, %rsp
    movabsq $0x5a3c96e1c3a5f00f, %rax
    movq %rax, 0(%rsp)
    notq %rax
    movq %rax, 8(%rsp)
    xorl %ecx, %ecx
.irp reg, 0,1,2,3,4,5,6,7,8,9,10,11,12,13,14
    movdqu (%rsp), %xmm\reg
.endr
1:
    cmpl $0, (%rdi)
    jne 3f
.irp reg, 0,1,2,3,4,5,6,7,8,9,10,11,12,13,14
    movdqu (%rsp), %xmm15
    pcmpeqb %xmm\reg, %xmm15
    pmovmskb %xmm15, %eax
    cmpl $0xffff, %eax
    je 2f
    incq %rcx
    movdqu (%rsp), %xmm\reg
2:
.endr
    jmp 1b
3:
    movq %rcx, %rax
    addq $24, %rsp
    ret
.size goos_probe_vector_spin, .-goos_probe_vector_spin

// goos_probe_xmm_root(index, token uint64, state *[2]uint32) uint64 holds
// a heap address in one XMM register while it spins. token is the address
// with every bit inverted. It zeroes XMM0 to XMM15, puts the address in
// XMM[index] alone, zeroes every caller-saved general-purpose register that
// held the address or the token, sets state[1], and loops without calls or
// stack writes until state[0] is nonzero. It returns the register's value,
// the address again when the register kept it, so the caller holds the list
// once more while it checks it. An index of 16 or more holds the address
// nowhere and returns zero. Only goprobe calls it, to show that a
// collection finds a root that only one XMM register of a held thread
// holds.
.section .text.goos_probe_xmm_root,"ax",@progbits
.global goos_probe_xmm_root
.type goos_probe_xmm_root,@function
goos_probe_xmm_root:
.irp reg, 0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15
    pxor %xmm\reg, %xmm\reg
.endr
    notq %rsi
    xorl %eax, %eax
    xorl %ecx, %ecx
    xorl %r8d, %r8d
    xorl %r9d, %r9d
    xorl %r10d, %r10d
    xorl %r11d, %r11d
    cmpq $16, %rdi
    jae .Lxmm_root_none
    // Each register's block starts 64 bytes after the one before it.
    shlq $6, %rdi
    leaq .Lxmm_root_hold0(%rip), %rax
    addq %rdi, %rax
    xorl %edi, %edi
    jmp *%rax
.irp reg, 0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15
.balign 64
.Lxmm_root_hold\reg:
    movq %rsi, %xmm\reg
    xorl %esi, %esi
    xorl %eax, %eax
    movl $1, 4(%rdx)
1:
    pause
    cmpl $0, (%rdx)
    je 1b
    movq %xmm\reg, %rax
    pxor %xmm\reg, %xmm\reg
    ret
.endr
.Lxmm_root_none:
    xorl %esi, %esi
    movl $1, 4(%rdx)
1:
    pause
    cmpl $0, (%rdx)
    je 1b
    xorl %eax, %eax
    ret
.size goos_probe_xmm_root, .-goos_probe_xmm_root

// goos_stack_pointer() uintptr returns its caller's stack pointer, which
// names the caller's thread (userspace/runtime/tls).
.global goos_stack_pointer
.type goos_stack_pointer,@function
goos_stack_pointer:
    leaq 8(%rsp), %rax
    ret
.size goos_stack_pointer, .-goos_stack_pointer

// TinyGo 0.42.0 amd64 defer-frame ABI: saved SP, then continuation PC.
// The compiler restores callee-saved values at that continuation.
.section .text.tinygo_longjmp,"ax",@progbits
.global tinygo_longjmp
.type tinygo_longjmp,@function
tinygo_longjmp:
    movq 0(%rdi), %rsp
    movq 8(%rdi), %rax
    jmpq *%rax
.size tinygo_longjmp, .-tinygo_longjmp

.section .note.GNU-stack,"",@progbits

.section .text.goos_log_read_sequence,"ax",@progbits
.global goos_log_read_sequence
.type goos_log_read_sequence,@function
goos_log_read_sequence:
    movl $58, %eax
    int $0x80
    ret
.size goos_log_read_sequence, .-goos_log_read_sequence
