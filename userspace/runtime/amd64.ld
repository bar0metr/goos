ENTRY(_start)

PHDRS
{
  text PT_LOAD FLAGS(5);
  rodata PT_LOAD FLAGS(4);
  data PT_LOAD FLAGS(6);
  stack PT_LOAD FLAGS(6);
}

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

  .heap (NOLOAD) : ALIGN(0x1000) {
    _heap_start = .;
    . += 0x20000;
    _heap_end = .;
  } :data

  /* 64 KiB of unmapped address space separate the stack from everything
     below it, so an overflow faults instead of corrupting the heap or globals
     (ADR 0083, ADR 0088). The size must equal loader.StackGuardSize: the
     loader rejects an image with a smaller gap, tools/stackframes bounds every
     frame by it, and kernel/loader's tests compare this line with it. */
  . = ALIGN(0x1000) + 0x10000;

  .stack (NOLOAD) : ALIGN(0x1000) {
    _stack_bottom = .;
    . += 0x8000;
    _stack_top = .;
  } :stack

  /DISCARD/ : { *(.comment) *(.eh_frame*) *(.note*) }
}
