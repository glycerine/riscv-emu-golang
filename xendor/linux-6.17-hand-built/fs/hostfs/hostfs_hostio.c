// SPDX-License-Identifier: GPL-2.0
/*
 * hostfs backend for riscv-emu-golang's custom hostio MMIO device.
 */

#include <linux/errno.h>
#include <linux/fs.h>
#include <linux/io.h>
#include <linux/mm.h>
#include <linux/mutex.h>
#include <linux/of.h>
#include <linux/of_address.h>
#include <linux/slab.h>
#include <linux/string.h>
#include <linux/statfs.h>
#include <linux/time64.h>
#include <linux/types.h>
#include <linux/unaligned.h>
#include <asm/page.h>
#include "hostfs.h"

#define HIO_MAGIC		0x314f4948u
#define HIO_VERSION		1u

#define HIO_REG_MAGIC		0x00
#define HIO_REG_VERSION		0x04
#define HIO_REG_STATUS		0x08
#define HIO_REG_ERRNO		0x0c
#define HIO_REG_CMD_ADDR	0x10
#define HIO_REG_CMD_SIZE	0x18
#define HIO_REG_SUBMIT		0x20
#define HIO_REG_RESULT		0x28

#define HIO_STATUS_OK		1u
#define HIO_STATUS_ERR		2u

#define HIO_CMD_SIZE		96
#define HIO_STAT_SIZE		32
#define HIO_DIRENT_HDR_SIZE	32
#define HIO_READDIR_BUF_SIZE	(256 * 1024)

#define HIO_OP_OPEN		1u
#define HIO_OP_CREATE		2u
#define HIO_OP_CLOSE		3u
#define HIO_OP_READ		4u
#define HIO_OP_WRITE		5u
#define HIO_OP_SEEK		6u
#define HIO_OP_MKDIR		7u
#define HIO_OP_REMOVE		9u
#define HIO_OP_RENAME		11u
#define HIO_OP_STAT		14u
#define HIO_OP_FSTAT		15u
#define HIO_OP_TRUNCATE		16u
#define HIO_OP_FTRUNCATE	17u
#define HIO_OP_SYNC		18u
#define HIO_OP_READDIR		19u
#define HIO_OP_LSTAT		20u
#define HIO_OP_READLINK		21u
#define HIO_OP_SYMLINK		22u
#define HIO_OP_CHMOD		23u

#define HIO_OPEN_RDONLY		0u
#define HIO_OPEN_WRONLY		1u
#define HIO_OPEN_RDWR		2u
#define HIO_OPEN_CREATE		BIT(8)
#define HIO_OPEN_EXCL		BIT(9)
#define HIO_OPEN_TRUNC		BIT(10)
#define HIO_OPEN_APPEND		BIT(11)

#define GO_MODE_DIR		BIT(31)
#define GO_MODE_SYMLINK		BIT(27)

struct hio_cmd {
	__le32 op;
	__le32 flags;
	__le64 path;
	__le64 path_len;
	__le64 path2;
	__le64 path2_len;
	__le64 buf;
	__le64 len;
	__le64 offset;
	__le64 mode;
	__le64 handle;
	__le64 result;
	__le32 err;
	__le32 status;
} __packed;

struct hio_stat {
	u64 size;
	u64 go_mode;
	u64 mtime_ns;
	u64 is_dir;
};

struct hio_file {
	u64 host_handle;
	char *path;
};

struct hio_dir {
	char *buf;
	size_t len;
	size_t off;
	unsigned long long pos;
	char name[NAME_MAX + 1];
};

static void __iomem *hio_base;
static DEFINE_MUTEX(hio_lock);
static struct hio_cmd *hio_cmd;
static phys_addr_t hio_cmd_phys;

static DEFINE_MUTEX(hio_files_lock);
static struct hio_file hio_files[1024];

static u64 hio_phys(const void *p)
{
	return virt_to_phys((void *)p);
}

static void hio_write64(u32 reg, u64 value)
{
	writel((u32)value, hio_base + reg);
	writel((u32)(value >> 32), hio_base + reg + 4);
}

static int hio_init(void)
{
	struct device_node *np;
	u32 magic, version;

	if (hio_base)
		return 0;

	np = of_find_compatible_node(NULL, NULL, "glycerine,riscv-hostio-v1");
	if (!np)
		return -ENODEV;

	hio_base = of_iomap(np, 0);
	of_node_put(np);
	if (!hio_base)
		return -ENODEV;

	magic = readl(hio_base + HIO_REG_MAGIC);
	version = readl(hio_base + HIO_REG_VERSION);
	if (magic != HIO_MAGIC || version != HIO_VERSION)
		return -ENODEV;

	hio_cmd = kzalloc(sizeof(*hio_cmd), GFP_KERNEL);
	if (!hio_cmd)
		return -ENOMEM;
	hio_cmd_phys = virt_to_phys(hio_cmd);

	pr_info("hostfs: using riscv hostio MMIO backend\n");
	return 0;
}

static int hio_submit(struct hio_cmd *cmd)
{
	u32 status, err;
	int ret;

	ret = hio_init();
	if (ret)
		return ret;

	mutex_lock(&hio_lock);
	memcpy(hio_cmd, cmd, sizeof(*cmd));
	wmb();
	hio_write64(HIO_REG_CMD_ADDR, hio_cmd_phys);
	hio_write64(HIO_REG_CMD_SIZE, HIO_CMD_SIZE);
	writel(1, hio_base + HIO_REG_SUBMIT);
	rmb();
	memcpy(cmd, hio_cmd, sizeof(*cmd));
	status = le32_to_cpu(cmd->status);
	err = le32_to_cpu(cmd->err);
	mutex_unlock(&hio_lock);

	if (status == HIO_STATUS_OK)
		return 0;
	if (status == HIO_STATUS_ERR && err)
		return -(int)err;
	return -EIO;
}

static u64 hio_path_ino(const char *path)
{
	u64 h = 1469598103934665603ULL;

	while (*path) {
		h ^= (u8)*path++;
		h *= 1099511628211ULL;
	}
	return h ?: 1;
}

static umode_t hio_mode_to_linux(u64 go_mode)
{
	umode_t perm = go_mode & 0777;

	if (go_mode & GO_MODE_DIR)
		return S_IFDIR | perm;
	if (go_mode & GO_MODE_SYMLINK)
		return S_IFLNK | 0777;
	return S_IFREG | perm;
}

static void hio_stat_to_hostfs(const char *path, const struct hio_stat *in,
			       struct hostfs_stat *out)
{
	umode_t mode = hio_mode_to_linux(in->go_mode);
	time64_t sec = div_s64((s64)in->mtime_ns, NSEC_PER_SEC);
	long nsec = (long)((s64)in->mtime_ns - sec * NSEC_PER_SEC);

	memset(out, 0, sizeof(*out));
	out->ino = hio_path_ino(path);
	out->mode = mode;
	out->nlink = S_ISDIR(mode) ? 2 : 1;
	out->uid = 0;
	out->gid = 0;
	out->size = in->size;
	out->atime.tv_sec = sec;
	out->atime.tv_nsec = nsec;
	out->mtime = out->atime;
	out->ctime = out->atime;
	out->btime = out->atime;
	out->blksize = PAGE_SIZE;
	out->blocks = (in->size + 511) >> 9;
	out->dev.maj = 0;
	out->dev.min = 1;
}

static int hio_close_handle(u64 handle);

static int hio_alloc_fd(u64 host_handle, const char *path)
{
	int fd;
	char *dup;

	dup = kstrdup(path, GFP_KERNEL);
	if (!dup) {
		hio_close_handle(host_handle);
		return -ENOMEM;
	}

	mutex_lock(&hio_files_lock);
	for (fd = 0; fd < ARRAY_SIZE(hio_files); fd++) {
		if (!hio_files[fd].host_handle) {
			hio_files[fd].host_handle = host_handle;
			hio_files[fd].path = dup;
			mutex_unlock(&hio_files_lock);
			return fd;
		}
	}
	mutex_unlock(&hio_files_lock);
	kfree(dup);
	hio_close_handle(host_handle);
	return -EMFILE;
}

static u64 hio_fd_handle(int fd)
{
	u64 handle = 0;

	if (fd < 0 || fd >= ARRAY_SIZE(hio_files))
		return 0;
	mutex_lock(&hio_files_lock);
	handle = hio_files[fd].host_handle;
	mutex_unlock(&hio_files_lock);
	return handle;
}

static char *hio_fd_path(int fd)
{
	char *path = NULL;

	if (fd < 0 || fd >= ARRAY_SIZE(hio_files))
		return NULL;
	mutex_lock(&hio_files_lock);
	if (hio_files[fd].path)
		path = kstrdup(hio_files[fd].path, GFP_KERNEL);
	mutex_unlock(&hio_files_lock);
	return path;
}

static int hio_close_handle(u64 handle)
{
	struct hio_cmd cmd = {
		.op = cpu_to_le32(HIO_OP_CLOSE),
		.handle = cpu_to_le64(handle),
	};

	return hio_submit(&cmd);
}

int stat_file(const char *path, struct hostfs_stat *p, int fd)
{
	struct hio_stat *st;
	struct hio_cmd cmd = {
		.op = cpu_to_le32(fd >= 0 ? HIO_OP_FSTAT : HIO_OP_LSTAT),
		.path = cpu_to_le64(hio_phys(path)),
		.path_len = cpu_to_le64(strlen(path)),
		.len = cpu_to_le64(sizeof(*st)),
	};
	char *fd_path = NULL;
	int ret;

	st = kzalloc(sizeof(*st), GFP_KERNEL);
	if (!st)
		return -ENOMEM;
	cmd.buf = cpu_to_le64(hio_phys(st));
	if (fd >= 0) {
		cmd.handle = cpu_to_le64(hio_fd_handle(fd));
		fd_path = hio_fd_path(fd);
		if (fd_path)
			path = fd_path;
	}
	ret = hio_submit(&cmd);
	if (!ret)
		hio_stat_to_hostfs(path, st, p);
	kfree(fd_path);
	kfree(st);
	return ret;
}

int access_file(char *path, int r, int w, int x)
{
	struct hostfs_stat st;

	return stat_file(path, &st, -1);
}

static u32 hio_open_flags(int r, int w, int create, int trunc, int append)
{
	u32 flags = HIO_OPEN_RDONLY;

	if (r && w)
		flags = HIO_OPEN_RDWR;
	else if (w)
		flags = HIO_OPEN_WRONLY;
	if (create)
		flags |= HIO_OPEN_CREATE;
	if (trunc)
		flags |= HIO_OPEN_TRUNC;
	if (append)
		flags |= HIO_OPEN_APPEND;
	return flags;
}

int open_file(char *path, int r, int w, int append)
{
	struct hio_cmd cmd = {
		.op = cpu_to_le32(HIO_OP_OPEN),
		.flags = cpu_to_le32(hio_open_flags(r, w, 0, 0, append)),
		.path = cpu_to_le64(hio_phys(path)),
		.path_len = cpu_to_le64(strlen(path)),
		.mode = cpu_to_le64(0644),
	};
	int ret;

	ret = hio_submit(&cmd);
	if (ret)
		return ret;
	return hio_alloc_fd(le64_to_cpu(cmd.result), path);
}

int file_create(char *name, int mode)
{
	struct hio_cmd cmd = {
		.op = cpu_to_le32(HIO_OP_OPEN),
		.flags = cpu_to_le32(hio_open_flags(1, 1, 1, 1, 0)),
		.path = cpu_to_le64(hio_phys(name)),
		.path_len = cpu_to_le64(strlen(name)),
		.mode = cpu_to_le64(mode),
	};
	int ret;

	ret = hio_submit(&cmd);
	if (ret)
		return ret;
	return hio_alloc_fd(le64_to_cpu(cmd.result), name);
}

void close_file(void *stream)
{
	int fd = *((int *)stream);
	u64 handle;
	char *path;

	if (fd < 0 || fd >= ARRAY_SIZE(hio_files))
		return;
	mutex_lock(&hio_files_lock);
	handle = hio_files[fd].host_handle;
	path = hio_files[fd].path;
	hio_files[fd].host_handle = 0;
	hio_files[fd].path = NULL;
	mutex_unlock(&hio_files_lock);
	kfree(path);
	if (handle)
		hio_close_handle(handle);
}

int replace_file(int oldfd, int fd)
{
	u64 old_handle, new_handle;
	char *old_path, *new_path;

	if (oldfd < 0 || oldfd >= ARRAY_SIZE(hio_files) ||
	    fd < 0 || fd >= ARRAY_SIZE(hio_files))
		return -EBADF;

	mutex_lock(&hio_files_lock);
	old_handle = hio_files[oldfd].host_handle;
	old_path = hio_files[oldfd].path;
	new_handle = hio_files[fd].host_handle;
	new_path = hio_files[fd].path;
	hio_files[fd].host_handle = old_handle;
	hio_files[fd].path = old_path;
	hio_files[oldfd].host_handle = 0;
	hio_files[oldfd].path = NULL;
	mutex_unlock(&hio_files_lock);

	kfree(new_path);
	if (new_handle)
		hio_close_handle(new_handle);
	return 0;
}

int read_file(int fd, unsigned long long *offset, char *buf, int len)
{
	char *bounce;
	struct hio_cmd seek = {
		.op = cpu_to_le32(HIO_OP_SEEK),
		.flags = cpu_to_le32(SEEK_SET),
		.handle = cpu_to_le64(hio_fd_handle(fd)),
		.offset = cpu_to_le64(*offset),
	};
	struct hio_cmd cmd = {
		.op = cpu_to_le32(HIO_OP_READ),
		.handle = cpu_to_le64(hio_fd_handle(fd)),
		.len = cpu_to_le64(len),
	};
	int ret;

	if (!le64_to_cpu(cmd.handle))
		return -EBADF;
	bounce = kmalloc(len, GFP_KERNEL);
	if (!bounce)
		return -ENOMEM;
	cmd.buf = cpu_to_le64(hio_phys(bounce));
	ret = hio_submit(&seek);
	if (ret)
		goto out;
	ret = hio_submit(&cmd);
	if (ret)
		goto out;
	memcpy(buf, bounce, le64_to_cpu(cmd.result));
	*offset += le64_to_cpu(cmd.result);
	ret = le64_to_cpu(cmd.result);
out:
	kfree(bounce);
	return ret;
}

int write_file(int fd, unsigned long long *offset, const char *buf, int len)
{
	char *bounce;
	struct hio_cmd seek = {
		.op = cpu_to_le32(HIO_OP_SEEK),
		.flags = cpu_to_le32(SEEK_SET),
		.handle = cpu_to_le64(hio_fd_handle(fd)),
		.offset = cpu_to_le64(*offset),
	};
	struct hio_cmd cmd = {
		.op = cpu_to_le32(HIO_OP_WRITE),
		.handle = cpu_to_le64(hio_fd_handle(fd)),
		.len = cpu_to_le64(len),
	};
	int ret;

	if (!le64_to_cpu(cmd.handle))
		return -EBADF;
	bounce = kmemdup(buf, len, GFP_KERNEL);
	if (!bounce)
		return -ENOMEM;
	cmd.buf = cpu_to_le64(hio_phys(bounce));
	ret = hio_submit(&seek);
	if (ret)
		goto out;
	ret = hio_submit(&cmd);
	if (ret)
		goto out;
	*offset += le64_to_cpu(cmd.result);
	ret = le64_to_cpu(cmd.result);
out:
	kfree(bounce);
	return ret;
}

int lseek_file(int fd, long long offset, int whence)
{
	struct hio_cmd cmd = {
		.op = cpu_to_le32(HIO_OP_SEEK),
		.flags = cpu_to_le32(whence),
		.handle = cpu_to_le64(hio_fd_handle(fd)),
		.offset = cpu_to_le64(offset),
	};

	if (!le64_to_cpu(cmd.handle))
		return -EBADF;
	return hio_submit(&cmd);
}

int fsync_file(int fd, int datasync)
{
	struct hio_cmd cmd = {
		.op = cpu_to_le32(HIO_OP_SYNC),
		.handle = cpu_to_le64(hio_fd_handle(fd)),
	};

	if (!le64_to_cpu(cmd.handle))
		return -EBADF;
	return hio_submit(&cmd);
}

void *open_dir(char *path, int *err_out)
{
	struct hio_cmd cmd = {
		.op = cpu_to_le32(HIO_OP_READDIR),
		.path = cpu_to_le64(hio_phys(path)),
		.path_len = cpu_to_le64(strlen(path)),
		.len = cpu_to_le64(HIO_READDIR_BUF_SIZE),
	};
	struct hio_dir *dir;
	int ret;

	dir = kzalloc(sizeof(*dir), GFP_KERNEL);
	if (!dir) {
		*err_out = ENOMEM;
		return NULL;
	}
	dir->buf = kmalloc(HIO_READDIR_BUF_SIZE, GFP_KERNEL);
	if (!dir->buf) {
		kfree(dir);
		*err_out = ENOMEM;
		return NULL;
	}
	cmd.buf = cpu_to_le64(hio_phys(dir->buf));
	ret = hio_submit(&cmd);
	if (ret) {
		kfree(dir->buf);
		kfree(dir);
		*err_out = -ret;
		return NULL;
	}
	dir->len = le64_to_cpu(cmd.result);
	*err_out = 0;
	return dir;
}

void seek_dir(void *stream, unsigned long long pos)
{
	struct hio_dir *dir = stream;

	dir->off = 0;
	dir->pos = 0;
	while (dir->pos < pos && dir->off + HIO_DIRENT_HDR_SIZE <= dir->len) {
		u32 name_len = get_unaligned_le32(dir->buf + dir->off + 24);
		size_t next = dir->off + HIO_DIRENT_HDR_SIZE + name_len;

		if (next > dir->len)
			break;
		dir->off = next;
		dir->pos++;
	}
}

char *read_dir(void *stream, unsigned long long *pos_out,
	       unsigned long long *ino_out, int *len_out,
	       unsigned int *type_out)
{
	struct hio_dir *dir = stream;
	u64 go_mode;
	u32 name_len;

	if (dir->off + HIO_DIRENT_HDR_SIZE > dir->len)
		return NULL;
	go_mode = get_unaligned_le64(dir->buf + dir->off + 8);
	name_len = get_unaligned_le32(dir->buf + dir->off + 24);
	if (name_len > NAME_MAX ||
	    dir->off + HIO_DIRENT_HDR_SIZE + name_len > dir->len)
		return NULL;

	memcpy(dir->name, dir->buf + dir->off + HIO_DIRENT_HDR_SIZE, name_len);
	dir->name[name_len] = '\0';
	*len_out = name_len;
	*ino_out = hio_path_ino(dir->name);
	if (go_mode & GO_MODE_DIR)
		*type_out = DT_DIR;
	else if (go_mode & GO_MODE_SYMLINK)
		*type_out = DT_LNK;
	else
		*type_out = DT_REG;

	dir->off += HIO_DIRENT_HDR_SIZE + name_len;
	dir->pos++;
	*pos_out = dir->pos;
	return dir->name;
}

void close_dir(void *stream)
{
	struct hio_dir *dir = stream;

	kfree(dir->buf);
	kfree(dir);
}

static int hio_path_op(const char *path, u32 op, u64 mode)
{
	struct hio_cmd cmd = {
		.op = cpu_to_le32(op),
		.path = cpu_to_le64(hio_phys(path)),
		.path_len = cpu_to_le64(strlen(path)),
		.mode = cpu_to_le64(mode),
	};

	return hio_submit(&cmd);
}

int do_mkdir(const char *file, int mode)
{
	return hio_path_op(file, HIO_OP_MKDIR, mode);
}

int hostfs_do_rmdir(const char *file)
{
	return hio_path_op(file, HIO_OP_REMOVE, 0);
}

int unlink_file(const char *file)
{
	return hio_path_op(file, HIO_OP_REMOVE, 0);
}

int rename_file(char *from, char *to)
{
	struct hio_cmd cmd = {
		.op = cpu_to_le32(HIO_OP_RENAME),
		.path = cpu_to_le64(hio_phys(from)),
		.path_len = cpu_to_le64(strlen(from)),
		.path2 = cpu_to_le64(hio_phys(to)),
		.path2_len = cpu_to_le64(strlen(to)),
	};

	return hio_submit(&cmd);
}

int rename2_file(char *from, char *to, unsigned int flags)
{
	if (flags)
		return -EINVAL;
	return rename_file(from, to);
}

int hostfs_do_readlink(char *file, char *buf, int size)
{
	struct hio_cmd cmd = {
		.op = cpu_to_le32(HIO_OP_READLINK),
		.path = cpu_to_le64(hio_phys(file)),
		.path_len = cpu_to_le64(strlen(file)),
		.buf = cpu_to_le64(hio_phys(buf)),
		.len = cpu_to_le64(size),
	};
	int ret = hio_submit(&cmd);

	if (ret)
		return ret;
	return le64_to_cpu(cmd.result);
}

int make_symlink(const char *from, const char *to)
{
	struct hio_cmd cmd = {
		.op = cpu_to_le32(HIO_OP_SYMLINK),
		.path = cpu_to_le64(hio_phys(to)),
		.path_len = cpu_to_le64(strlen(to)),
		.path2 = cpu_to_le64(hio_phys(from)),
		.path2_len = cpu_to_le64(strlen(from)),
	};

	return hio_submit(&cmd);
}

int set_attr(const char *file, struct hostfs_iattr *attrs, int fd)
{
	int ret = 0;

	if (attrs->ia_valid & HOSTFS_ATTR_MODE)
		ret = hio_path_op(file, HIO_OP_CHMOD, attrs->ia_mode);
	if (!ret && (attrs->ia_valid & HOSTFS_ATTR_SIZE)) {
		struct hio_cmd cmd = {
			.op = cpu_to_le32(fd >= 0 ? HIO_OP_FTRUNCATE : HIO_OP_TRUNCATE),
			.path = cpu_to_le64(hio_phys(file)),
			.path_len = cpu_to_le64(strlen(file)),
			.handle = cpu_to_le64(fd >= 0 ? hio_fd_handle(fd) : 0),
			.offset = cpu_to_le64(attrs->ia_size),
		};
		ret = hio_submit(&cmd);
	}
	return ret;
}

int do_mknod(const char *file, int mode, unsigned int major, unsigned int minor)
{
	return -EPERM;
}

int link_file(const char *to, const char *from)
{
	return -EPERM;
}

int do_statfs(char *root, long *bsize_out, long long *blocks_out,
	      long long *bfree_out, long long *bavail_out,
	      long long *files_out, long long *ffree_out,
	      void *fsid_out, int fsid_size, long *namelen_out)
{
	*bsize_out = PAGE_SIZE;
	*blocks_out = 1ULL << 30;
	*bfree_out = 1ULL << 29;
	*bavail_out = 1ULL << 29;
	*files_out = 1ULL << 30;
	*ffree_out = 1ULL << 29;
	memset(fsid_out, 0, fsid_size);
	*namelen_out = NAME_MAX;
	return 0;
}
