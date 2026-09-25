ENTRY(_start)

PHDRS
{
  text PT_LOAD FLAGS(5);
  rodata PT_LOAD FLAGS(4);
  data PT_LOAD FLAGS(6);
}

/* The stack and the heap are not part of the image (ADR 0104). The kernel
   reserves the stack below the top of the user range, with an unmapped guard
   of loader.StackGuardSize below it, and supplies its pages as it grows; the
   heap is anonymous memory the runtime reserves at start. _goos_stack_reserve
   is the stack a program asks for; elf2native records it in the image. */
_goos_stack_reserve = 0x100000;

SECTIONS
{
  . = 0x40002000;
  .text : ALIGN(0x1000) {
    KEEP(*(.text.entry))
    *(.text .text.*)
  } :text

  .rodata : ALIGN(0x1000) {
    *(.rodata .rodata.*)
  } :rodata

  .data : ALIGN(0x1000) {
    *(.data .data.*)
    *(.bss .bss.* COMMON)
  } :data

  /DISCARD/ : { *(.comment) *(.eh_frame*) *(.note*) }
}
