.section .text.entry,"ax",@progbits
.global _start
.type _start,@function
_start:
    andq $-16, %rsp
    call userspaceMain
    ud2
.size _start, .-_start

.section .text.kcall,"ax",@progbits
.global goos_debug_write
.type goos_debug_write,@function
goos_debug_write:
    movl $2, %eax
    int $0x80
    ret
.size goos_debug_write, .-goos_debug_write

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

.global goos_exit
.type goos_exit,@function
goos_exit:
    movl $1, %eax
    int $0x80
    ud2
.size goos_exit, .-goos_exit

.section .note.GNU-stack,"",@progbits
