OUTPUT_ARCH( "riscv" )
ENTRY(_start)

SECTIONS
{
  /* Start text at 0x1000 (page 1) so libriscv's null-page guard
     doesn't reject the ELF. The riscv-tests link script starts at 0x0,
     but the bench guest has no .text.init section, which would cause
     .text to land at address 0. */
  . = 0x1000;

  .text : { *(.text .text.init) }
  . = ALIGN(0x1000);
  .data : { *(.data) }
  .bss : { *(.bss) }
  _end = .;

  /* Discard exception-handling frames — freestanding code doesn't need them,
     and they create a PT_LOAD segment at vaddr=0 which libriscv rejects
     (null-page guard protection). */
  /DISCARD/ : { *(.eh_frame) *(.eh_frame_hdr) }
}
