ENTRY(_start)

PHDRS
{
  text PT_LOAD FLAGS(5);
  rodata PT_LOAD FLAGS(4);
  data PT_LOAD FLAGS(6);
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

  .stack (NOLOAD) : ALIGN(0x1000) {
    _stack_bottom = .;
    . += 0x8000;
    _stack_top = .;
  } :data

  .heap (NOLOAD) : ALIGN(0x1000) {
    _heap_start = .;
    . += 0x10000;
    _heap_end = .;
  } :data

  /DISCARD/ : { *(.comment) *(.eh_frame*) *(.note*) }
}
