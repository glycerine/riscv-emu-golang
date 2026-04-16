OUTPUT_ARCH( "riscv" )
ENTRY(_start)

SECTIONS
{
   /* Start at 0 so offsets are relative to the beginning of your buffer */
  . = 0x0;

  .text.init : { *(.text.init) }
  . = ALIGN(0x1000);
  .tohost : { *(.tohost) }
  . = ALIGN(0x1000);
  .text : { *(.text) }
  . = ALIGN(0x1000);
  .data : { *(.data) }
  .bss : { *(.bss) }
  _end = .;
}

