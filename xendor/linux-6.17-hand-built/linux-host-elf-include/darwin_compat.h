#ifndef _LINUX_HOST_DARWIN_COMPAT_H
#define _LINUX_HOST_DARWIN_COMPAT_H

#ifndef _UUID_T
#define _UUID_T
#endif

#ifndef __GETHOSTUUID_H
#define __GETHOSTUUID_H
#endif

#include <string.h>

#ifdef memcpy
#undef memcpy
#endif
#ifdef memmove
#undef memmove
#endif
#ifdef memset
#undef memset
#endif
#ifdef strcpy
#undef strcpy
#endif
#ifdef strlcpy
#undef strlcpy
#endif
#ifdef strlcat
#undef strlcat
#endif
#ifdef strncpy
#undef strncpy
#endif

#ifndef __attribute_const__
#define __attribute_const__ __attribute__((__const__))
#endif

/* Keep MacPorts libelf quiet when objtool builds with -Wundef. */
#ifndef __LIBELF_INTERNAL__
#define __LIBELF_INTERNAL__ 0
#endif
#ifndef __LIBELF_NEED_LINK_H
#define __LIBELF_NEED_LINK_H 0
#endif
#ifndef __LIBELF_NEED_SYS_LINK_H
#define __LIBELF_NEED_SYS_LINK_H 0
#endif
#ifndef __LIBELF64_IRIX
#define __LIBELF64_IRIX 0
#endif
#ifndef __LIBELF64_LINUX
#define __LIBELF64_LINUX 0
#endif

/*
 * MacPorts libelf provides GElf on Darwin, but its headers do not provide the
 * Linux x86_64 relocation constants used by objtool.
 */
#ifndef R_X86_64_NONE
#define R_X86_64_NONE 0
#define R_X86_64_64 1
#define R_X86_64_PC32 2
#define R_X86_64_GOT32 3
#define R_X86_64_PLT32 4
#define R_X86_64_COPY 5
#define R_X86_64_GLOB_DAT 6
#define R_X86_64_JUMP_SLOT 7
#define R_X86_64_RELATIVE 8
#define R_X86_64_GOTPCREL 9
#define R_X86_64_32 10
#define R_X86_64_32S 11
#define R_X86_64_16 12
#define R_X86_64_PC16 13
#define R_X86_64_8 14
#define R_X86_64_PC8 15
#define R_X86_64_DTPMOD64 16
#define R_X86_64_DTPOFF64 17
#define R_X86_64_TPOFF64 18
#define R_X86_64_TLSGD 19
#define R_X86_64_TLSLD 20
#define R_X86_64_DTPOFF32 21
#define R_X86_64_GOTTPOFF 22
#define R_X86_64_TPOFF32 23
#define R_X86_64_PC64 24
#define R_X86_64_GOTOFF64 25
#define R_X86_64_GOTPC32 26
#define R_X86_64_GOT64 27
#define R_X86_64_GOTPCREL64 28
#define R_X86_64_GOTPC64 29
#define R_X86_64_GOTPLT64 30
#define R_X86_64_PLTOFF64 31
#define R_X86_64_SIZE32 32
#define R_X86_64_SIZE64 33
#define R_X86_64_GOTPC32_TLSDESC 34
#define R_X86_64_TLSDESC_CALL 35
#define R_X86_64_TLSDESC 36
#define R_X86_64_IRELATIVE 37
#define R_X86_64_RELATIVE64 38
#define R_X86_64_GOTPCRELX 41
#define R_X86_64_REX_GOTPCRELX 42
#define R_X86_64_NUM 43
#endif

static inline char *linux_host_strchrnul(const char *s, int c)
{
	char *p = strchr(s, c);
	return p ? p : (char *)s + strlen(s);
}

#define strchrnul linux_host_strchrnul

#endif
