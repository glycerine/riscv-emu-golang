OUTPUT_ARCH(riscv)
ENTRY(_start)

SECTIONS
{
 . = 0x0;

 PROVIDE(_fw_start = .);

 . = ALIGN(0x1000);



 .text :
  {
  PROVIDE(_text_start = .);
  *(.entry)
  *(.text)
  *(.text.*)
  . = ALIGN(8);
  PROVIDE(_text_end = .);
 }



 . = ALIGN(0x1000);



 PROVIDE(_rodata_start = .);

 .rodata :
 {
  *(.rodata .rodata.*)
  . = ALIGN(8);
 }

 .dynsym :
 {
  *(.dynsym)
 }

 . = ALIGN(0x1000);

 .rela.dyn : {
  PROVIDE(__rela_dyn_start = .);
  *(.rela*)
  PROVIDE(__rela_dyn_end = .);
 }

 PROVIDE(_rodata_end = .);







 . = ALIGN(1 << LOG2CEIL((SIZEOF(.rodata) + SIZEOF(.text)
    + SIZEOF(.dynsym) + SIZEOF(.rela.dyn))));

 PROVIDE(_fw_rw_start = .);



 .data :
 {
  PROVIDE(_data_start = .);

  *(.sdata)
  *(.sdata.*)
  *(.data)
  *(.data.*)
  *(.readmostly.data)
  *(*.data)
  . = ALIGN(8);

  PROVIDE(_data_end = .);
 }

 . = ALIGN(0x1000);

 .bss :
 {
  PROVIDE(_bss_start = .);
  *(.sbss)
  *(.sbss.*)
  *(.bss)
  *(.bss.*)
  . = ALIGN(8);
  PROVIDE(_bss_end = .);
 }



 . = ALIGN(0x1000);

 PROVIDE(_fw_end = .);

 PROVIDE(_fw_reloc_end = .);
}
