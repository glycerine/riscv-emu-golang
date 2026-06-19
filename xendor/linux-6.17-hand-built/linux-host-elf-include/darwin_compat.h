#ifndef _LINUX_HOST_DARWIN_COMPAT_H
#define _LINUX_HOST_DARWIN_COMPAT_H

#ifndef _UUID_T
#define _UUID_T
#endif

#ifndef __GETHOSTUUID_H
#define __GETHOSTUUID_H
#endif

#include <string.h>

static inline char *linux_host_strchrnul(const char *s, int c)
{
	char *p = strchr(s, c);
	return p ? p : (char *)s + strlen(s);
}

#define strchrnul linux_host_strchrnul

#endif
