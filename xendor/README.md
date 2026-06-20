# installing apks from the network and having them persist across reboots

Inside the guest alpine linux, having 
booted up with "make linux", to install 
local apks and also update apks on host
for next boot:
~~~
ROOT=/host/Users/jaten/ris/xendor/alpine-minirootfs-3.24.1-riscv64
APKDIR=/host/Users/jaten/ris/xendor/linux/alpine-nettools/apks

apk add --root "$ROOT" --no-network \
  "$APKDIR/libcap2-2.78-r0.apk" \
  "$APKDIR/zstd-libs-1.5.7-r2.apk" \
  "$APKDIR/libelf-0.195-r0.apk" \
  "$APKDIR/libmnl-1.0.5-r2.apk" \
  "$APKDIR/iproute2-minimal-7.0.0-r0.apk"
~~~

verify, still in guest:

~~~
apk --root "$ROOT" info -e iproute2-minimal
apk --root "$ROOT" info -W /sbin/ip
~~~

after that, on host, repack; this is archived in git repo.

~~~
cd ~/ris && make repack-initramfs
~~~
