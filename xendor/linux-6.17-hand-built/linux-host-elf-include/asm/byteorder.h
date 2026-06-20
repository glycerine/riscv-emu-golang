#ifndef _LINUX_HOST_ASM_BYTEORDER_H
#define _LINUX_HOST_ASM_BYTEORDER_H

#if defined(__BYTE_ORDER__) && __BYTE_ORDER__ == __ORDER_BIG_ENDIAN__
#include <linux/byteorder/big_endian.h>
#else
#include <linux/byteorder/little_endian.h>
#endif

#endif
