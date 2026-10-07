.section .text.entry,"ax",@progbits
.global _start
.type _start,@function
_start:
    andq $-16, %rsp
    call userspaceMain
    ud2
.size _start, .-_start

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

.section .note.GNU-stack,"",@progbits
