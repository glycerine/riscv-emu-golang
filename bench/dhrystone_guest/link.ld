OUTPUT_ARCH( "riscv" )
ENTRY(_start)

SECTIONS
{
  /* Start text at 0x1000 (page 1) so libriscv's null-page guard
     doesn't reject the ELF. */
  . = 0x1000;

  .text : { *(.text .text.init) }
  . = ALIGN(0x1000);
  .rodata : { *(.rodata .rodata.*) }
  . = ALIGN(0x1000);
  .data : { *(.data .data.*) }
  . = ALIGN(16);
  .bss : { *(.bss .bss.* COMMON) }
  . = ALIGN(16);
  _end = .;

  /* Discard exception-handling frames — freestanding code doesn't need them. */
  /DISCARD/ : { *(.eh_frame) *(.eh_frame_hdr) }
}
