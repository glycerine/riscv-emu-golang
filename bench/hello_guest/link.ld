OUTPUT_ARCH( "riscv" )
ENTRY(_start)

SECTIONS
{
  /* Start text at 0x1000 so libriscv's null-page guard doesn't reject
     the ELF (a PT_LOAD at vaddr=0 is rejected). */
  . = 0x1000;

  .text : { *(.text .text.init) }
  . = ALIGN(0x1000);
  .rodata : { *(.rodata .rodata.*) }
  . = ALIGN(0x1000);
  .data : { *(.data) }
  .bss : { *(.bss) }
  _end = .;

  /DISCARD/ : { *(.eh_frame) *(.eh_frame_hdr) }
}
