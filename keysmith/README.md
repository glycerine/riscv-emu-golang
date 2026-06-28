keysmith
--------

Keysmith is a Go library to prepare an emu to serve as
a test fixture for any project that needs to login 
over ssh to a server. It supplies the emu server and
the current user with completely fresh ed25519 public/
private key pairs so that there are no security worries
about re-using keys or having to check them into 
a repo. It also provides for running on windows by
allowing compression to cpio archive where windows
lacks a native utility to do. Thus after 1) generating
the host key and writing into the pre-archival initramfs
directory; 2) generating the user's key pair and 
storing the user's public into the pre-archival
initramfs directory; we 3) create a cpio archive and
gzip it into place, ready to boot from (emu) or
embed (emul).

ed25519 key generation will be done using my
~/go/src/github.com/glycerine/rpc25519/selfcert/
library, which is imported as "github.com/glycerine/rpc25519/selfcert".
The command line utility 'selfy' (which we will not use here,
because we do not want to shell out on Windows),
demonstrates use: ~/go/src/github.com/glycerine/rpc25519/cmd/selfy/selfy.go

1. generate a fresh ed25519 keypair with no password to be the
host key for sshd, and add it to the sshd /etc/ssh/ssh_host_ed25519_key
file.

ROOT=$HOME/go/src/github.com/glycerine/riscv-emu-golang/\
xendor/alpine-minirootfs-3.24.1-riscv64

The equivalent of:
"/usr/bin/ssh-keygen" -t ed25519 -N "" -f "$ROOT/etc/ssh/ssh_host_ed25519_key"

2. create fresh ed25519 keypair to be the user login over ssh keys,
and put the public key into the guest image "$ROOT/root/.ssh/
This also has no password.

selfy -nopass -k emunet -ssh
 public-key replaces entirely the $ROOT/root/.ssh/authorized_keys
 to prevent stale/lost keys from still allowing login.

3. The moral equivalent of "make repack" but entirely in 
Golang since Windows does not have cpio.

Given

INITRAMFS_CPIO := $HOME/go/src/github.com/glycerine/riscv-emu-golang/\
xendor/linux/initramfs.cpio.gz

"make repack" does this
	cd $(ROOT) && find . -print0 | \
	cpio --null --create --format=newc --owner=root | \
	gzip -9 > $(INITRAMFS_CPIO)

And these are the operations we need to code up in Go
to allow us to, on Windows, repack the initial ram filesystem for booting Linux.

"compress/gzip" is in the Go standard library.
keysmith writes the newc cpio archive directly.

for the "find . -print0" equivalent in Go:
path/filepath.WalkDir is the recommended approach to walk a directory
because it is significantly faster and avoids reading
file info (like permissions and modification times) unless you explicitly ask for it.
