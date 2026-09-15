.section .text.entry,"ax",@progbits
.global _start
.type _start,@function
_start:
    andq $-16, %rsp
    call initMain
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

.global goos_ipc_wait_receive
.type goos_ipc_wait_receive,@function
goos_ipc_wait_receive:
    movl $5, %eax
    int $0x80
    ret
.size goos_ipc_wait_receive, .-goos_ipc_wait_receive

.section .note.GNU-stack,"",@progbits
