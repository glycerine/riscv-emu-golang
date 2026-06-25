package xendor

import "embed"

//go:embed opensbi/build/platform/generic/firmware/fw_dynamic.elf
//go:embed linux-6.17-hand-built/Image
//go:embed linux/initramfs.cpio.gz
var Bootables embed.FS
