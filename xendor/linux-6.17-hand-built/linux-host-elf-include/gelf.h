#ifndef _LINUX_HOST_GELF_WRAPPER_H
#define _LINUX_HOST_GELF_WRAPPER_H

#include_next <gelf.h>

static inline GElf_Sym *gelf_getsymshndx(Elf_Data *symdata, Elf_Data *shndxdata,
					 int ndx, GElf_Sym *sym,
					 Elf32_Word *xshndx)
{
	GElf_Sym *ret = gelf_getsym(symdata, ndx, sym);
	if (!ret)
		return NULL;

	if (xshndx) {
		if (shndxdata && shndxdata->d_buf)
			*xshndx = ((Elf32_Word *)shndxdata->d_buf)[ndx];
		else
			*xshndx = sym->st_shndx;
	}

	return ret;
}

static inline int gelf_update_symshndx(Elf_Data *symdata, Elf_Data *shndxdata,
				       int ndx, GElf_Sym *sym,
				       Elf32_Word xshndx)
{
	if (shndxdata && shndxdata->d_buf) {
		((Elf32_Word *)shndxdata->d_buf)[ndx] = xshndx;
		elf_flagdata(shndxdata, ELF_C_SET, ELF_F_DIRTY);
	}

	return gelf_update_sym(symdata, ndx, sym);
}

#endif
