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
