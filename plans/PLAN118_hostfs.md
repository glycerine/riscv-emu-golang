MMIO hostio: guest to host filesystem passthrough
====================================

two parts: compiled into kernel hostfs "driver";
and the VFS layer.

Q: do we have a custom MMIO port now? that would be our next goal. here are the defintions/what I mean:

To get access to host disk and network, you have two choices: the "Standard/Hard" path (VirtIO) or the "Cheating/Easy" path (Custom I/O).

Since you are writing your own Go-based RISC-V emulator, you are in a unique position to choose the complexity.

### 1. The "Standard" Path: VirtIO-MMIO

This is the "correct" way. Linux kernels are built to probe the Device Tree for `virtio,mmio` nodes. When it finds one, it maps that memory region and begins interacting with a defined set of registers (the VirtIO-MMIO specification).

**To make this work:**

* **Update your Device Tree (FDT):** You must add nodes under `/soc` that define the base address, size, and interrupt for each VirtIO device.
* Example:
```dts
virtio@10008000 {
   compatible = "virtio,mmio";
reg = <0x0 0x10008000 0x0 0x1000>;
    interrupts = <1>;
 interrupt-parent = <&plic>;
 };
 
 ```
 
 * **Implement the Registers:** You must trap memory accesses to the range defined in the FDT (`0x10008000`–`0x10009000`). The Linux VirtIO driver will write to these registers to discover device features, negotiate ring buffers, and "kick" the device (notify it that work is ready).
 * **Handle the "Kick":** When the guest writes to the "Queue Notify" register, your Go program must perform the requested operation (e.g., read a block from a file on your Mac, put it in the guest's RAM, and fire an interrupt).
 
 ---
 
 ### 2. The "Cheating" Path: Custom MMIO Port (Easiest) WHAT WE DID.
 
 If you don't want to implement the massive VirtIO specification (which is complex and error-prone), you can create a **simple custom device** and a tiny driver inside your Guest Linux.
 
 **How it works:**
 
 1. **Define a Memory Region:** Pick an unused address (e.g., `0x40000000`).
 2. **Map it in FDT:** You don't even need a standard `virtio` node; you can just add a simple node with your own `compatible` string.
 3. **Implement in Go:** When your emulator detects a read/write to `0x40000000`:
 * **Write:** The guest puts a pointer/command there. Your Go code executes the command on the host (e.g., `os.ReadFile`).
 * **Read:** The guest checks that address to see if the host has finished the task.
 
 
 4. **Guest Driver:** You write a tiny kernel module (or even just an app that uses `/dev/mem` to access that physical address) that talks to your emulator's "port."
 
 **Why do this?**
 
 * **Zero complexity:** No ring buffers, no descriptor tables, no VQ (VirtQueue) handshake.
 * **Instant:** You can have a "Host-to-Guest" pipe working in an hour.
 
 ---
 
 ### Comparison: Which to choose?
 
 | Feature | VirtIO-MMIO | Custom MMIO |
 | --- | --- | --- |
 | **Complexity** | High (spec-heavy) | Very Low |
 | **Compatibility** | Works with standard Linux | Requires custom kernel/driver |
 | **Performance** | High (optimized) | Moderate |
 | **Effort** | Weeks | Hours |
 
 ### Recommendation
 
 If your goal is to **learn emulation** or **run Linux without modifying it**, implement **VirtIO-MMIO**. Start by looking at how simple projects like [crosvm](https://chromium.googlesource.com/chromiumos/platform/crosvm/) or [QEMU](https://www.qemu.org/) handle the `virtio-mmio` register access.
 
 If your goal is simply to **get a shell running and move data between the Host and Guest** as quickly as possible, **build a custom MMIO port**. It bypasses the need for the kernel to understand VirtIO drivers, and you can map your host filesystem directly into that memory range.
 
 Let us do this. Do we need a planning pass? Make a plan, then do it.
 
 

Q: are you building the guest driver as a kernel module that we will compile in?

Not yet. I’m currently building only the emulator-side MMIO hostio device.

For the guest side, yes: we should make it a tiny **built-in Linux driver**, not a loadable module, because we turned kernel modules off.

Best shape:

- Add a driver in `~/linux`, probably `drivers/misc/riscv_hostio.c`.
- Match FDT compatible string: `glycerine,riscv-hostio-v1`.
- `ioremap()` the MMIO range from the device tree.
- Allocate command/data buffers in kernel memory and pass their physical addresses to hostio.
- Expose `/dev/hostio`.
- Userland talks to `/dev/hostio` via `ioctl`/`read`/`write` style calls.
- Compile it into the kernel with something like `CONFIG_RISCV_HOSTIO=y`.

So the full stack is:

```text
guest app -> /dev/hostio -> built-in Linux driver -> custom MMIO -> Go emulator -> host os.Open/os.Read/os.Write/os.Mkdir...
```

I should finish the emulator-side ABI first, then add the built-in guest driver to `~/linux` and rebuild the kernel.


Q: I am still unclear on one point. How will the guest linux commands like "cat" and "ls" and "awk" talk to the host filesystem? presumably the host filesystem is "mounted" right?

Right: for normal Linux commands like `cat`, `ls`, `awk`, etc. to work transparently, the host filesystem needs to appear as a **mounted filesystem** inside guest Linux.

A raw `/dev/hostio` character device alone would only let custom guest programs talk to the host. It would **not** make `ls /host` or `cat /host/foo` work automatically.

There are three layers we could build:

1. **Custom MMIO device only**
  Guest can send “open/read/write” commands to Go, but only custom code using that protocol can use it.

2. **Character device `/dev/hostio`**
  Easier kernel driver. Still only custom tools/libraries can use it. Normal `cat` and `ls` do not magically use it.

3. **Real Linux filesystem driver**
  This is what you mean by “mounted.” We would implement a tiny kernel filesystem, maybe `hostfs`, whose VFS operations call our hostio MMIO device.

Then inside Linux:

```sh
mount -t hostfs none /host
ls /host
cat /host/file.txt
awk '{print $1}' /host/file.txt
```

Those standard commands would work because they talk to Linux VFS, and our `hostfs` driver translates VFS calls into emulator host Go `os.Open`, `ReadDir`, `Read`, `Write`, `Mkdir`, etc.

So: if the goal is normal shell commands accessing host files, the next target should not be just `/dev/hostio`. It should be:

```text
Go hostio MMIO device + built-in Linux hostfs filesystem driver
```

The custom MMIO port is still useful, but it becomes the transport underneath the filesystem driver.

result
------

Implemented and verified end to end.

What changed:
- Added emulator `-hostio` MMIO device and FDT advertisement at `0x10001000`: [cmd/emu/emu.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/cmd/emu/emu.go:153), [cmd/emu/bios.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/cmd/emu/bios.go:42), [cmd/emu/hostio.go](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/cmd/emu/hostio.go:13)
- Added a Linux built-in `hostfs` backend that talks to hostio, not a module: [/Users/jaten/linux/fs/hostfs/hostfs_hostio.c](/Users/jaten/linux/fs/hostfs/hostfs_hostio.c:1), [/Users/jaten/linux/fs/Kconfig](/Users/jaten/linux/fs/Kconfig:294)
- Kept modules off and confirmed `CONFIG_HOSTFS=y`, `io_uring`, networking, virtio-mmio/net/block, ext4, and XFS remain enabled.
- Rebuilt `/Users/jaten/linux/arch/riscv/boot/Image` and copied it to `/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/xendor/linux-6.17-hand-built/Image`.
- Updated `make linux` to run with `-hostio`: [Makefile](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/Makefile:995)

Inside the guest, normal commands should use it by mounting:

```sh
mkdir -p /host
mount -t hostfs none /host -o /Users/jaten
ls /host
cat /host/some-file
echo hello > /host/tmp/from-guest.txt
```

Verification passed:
- Hostio unit tests passed.
- Linux `fs/hostfs/` built cleanly.
- Full RISC-V Linux `Image` built cleanly.
- Boot-to-`/init` smoke passed: `6.408s` host time, kernel timestamp `3.311663s`.
- Guest `hostfs` smoke passed: guest mounted a host temp dir, `cat` read a host-created file, guest wrote a file, and host read it back. Took `8.144s`.

Current caveats: this is intentionally full host passthrough with emulator-process permissions. `mknod` and hard links return `EPERM`; uid/gid/time setattr is minimal; fd table is fixed at 1024; `readdir` uses a 256 KiB response buffer.

