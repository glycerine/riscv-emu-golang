#ifndef _LINUX_HOST_SYS_SENDFILE_H
#define _LINUX_HOST_SYS_SENDFILE_H

#include <errno.h>
#include <sys/types.h>
#include <unistd.h>

static inline ssize_t sendfile(int out_fd, int in_fd, off_t *offset, size_t count)
{
	char buf[65536];
	size_t done = 0;

	while (done < count) {
		size_t want = count - done;
		ssize_t nread;

		if (want > sizeof(buf))
			want = sizeof(buf);
		if (offset)
			nread = pread(in_fd, buf, want, *offset);
		else
			nread = read(in_fd, buf, want);
		if (nread < 0)
			return -1;
		if (nread == 0) {
			errno = EIO;
			return -1;
		}

		for (ssize_t written = 0; written < nread;) {
			ssize_t nwrite = write(out_fd, buf + written, nread - written);
			if (nwrite < 0)
				return -1;
			if (nwrite == 0) {
				errno = EIO;
				return -1;
			}
			written += nwrite;
		}

		if (offset)
			*offset += nread;
		done += nread;
	}

	return (ssize_t)done;
}

#endif
