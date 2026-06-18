guest ethernet to host tailscale node
=====================================

great. Next we add networking. there are two parts. I will give them to you both here. You should create a plan to implement them both. 1) Here is the guest kernel facing API side, the virtio net driver:

You hit the nail on the head. You are looking to implement the **VirtIO-MMIO** transport.

In the RISC-V world (and other embedded environments without PCI), **VirtIO-MMIO** is the standard way to expose virtual hardware to the guest kernel. It allows you to map device registers directly into the guest’s physical address space so the kernel can interact with your Go code using simple memory loads and stores.

### The API: VirtIO-MMIO

Your Go program acts as the "backend" for this MMIO interface. The Linux kernel's `virtio-mmio` driver expects to see a specific memory-mapped structure starting at the base address you define in your Device Tree (FDT).

#### 1. The Register Space

You must implement a small region of memory (usually 4KB, e.g., at `0x10008000`) that behaves like hardware registers. When Linux writes to these, you handle the action:

* **`MagicValue` (0x000):** Should read `0x74726976` ("virt").
* **`DeviceID` (0x008):** For a network card, this must be `1` (as per the VirtIO spec).
* **`QueueNotify` (0x050):** This is the **most critical register**. When the kernel writes a queue index here, it is signaling, *"Hey, I've put new work (Ethernet frames) in the ring buffer for this queue; come process it!"*

#### 2. The Data Plane (VirtQueues)

You don't process packets by reading registers; you process them by reading **shared memory**.

* The Guest kernel and your Go program share the same physical RAM.
* The kernel places packet data into a **VirtQueue** (a ring buffer in RAM) and then writes to the `QueueNotify` register to "kick" you.
* Your Go code then reads the guest memory at the address specified by the descriptors in that ring buffer.

### Why this is the "Golden Path" for your One-Process Architecture

Since you want to embed `tsnet` inside your Go process, the workflow for your "hardware" is now remarkably simple:

1. **Guest Boot:** Linux parses the Device Tree, finds the `virtio,mmio` node, and probes the device. It performs a "handshake" by reading/writing your MMIO registers to negotiate features (like "I support checksum offloading," etc.).
2. **The "Kick":** When the guest kernel wants to send a network packet, it fills a buffer with an Ethernet frame, updates the VirtQueue structure in RAM, and writes to your `QueueNotify` register.
3. **The Bridge:** Your Go program intercepts that write to `QueueNotify`. You grab the frame from the guest RAM, pass it to your embedded `tsnet` stack, and the frame is on its way across the Tailscale network.
4. **The Callback:** When `tsnet` receives a packet from the network, it hands it to your `LinkEndpoint` (which you implement), you place the frame into a buffer in guest RAM, update the VirtQueue, and fire an interrupt (via your emulated PLIC) to tell the guest, *"Data is ready!"*

### Implementation Tip

You do not need to implement the *entire* VirtIO spec—just the parts the Linux `virtio_net` driver touches.

* Look at the [Linux `virtio_mmio.c](https://www.google.com/search?q=%5Bhttps://github.com/torvalds/linux/blob/master/drivers/virtio/virtio_mmio.c%5D(https://github.com/torvalds/linux/blob/master/drivers/virtio/virtio_mmio.c))` source code. It is the best documentation on what registers the guest kernel actually checks during the probe and initialization phase.

Since you're going for speed, keep your MMIO register handling as a simple `switch` statement in your main emulator loop that maps to your internal `tsnet` state.

2. The host facing implementation of the simulated ethernet card uses the tsnet tailscale library to create an in-process tailscaled node. See ~/go/src/github.com/tailscale/tailscale/tsnet/README.md and this design doc:

"Fast" is exactly why this architecture is the "golden path" for an emulator. By embedding Tailscale’s `tsnet` library directly into your Go process, you avoid all the overhead of context switching between host processes, managing Unix sockets, or dealing with external daemons.

### The One-Process Architecture

Since you want one single, high-performance process, you are essentially creating a **Tailscale-aware emulator**.

#### 1. Why `tsnet` is the correct tool

Tailscale provides a library called `tsnet` specifically designed for this purpose. Unlike the standard `tailscaled` daemon (which manages system-wide networking), `tsnet` runs as a library within your Go application memory space.

* **No `tun` interfaces required:** It doesn't touch the host's system networking unless you specifically tell it to.
* **Full gVisor Integration:** It bundles the gVisor `netstack`, so you get a complete, high-performance TCP/IP stack sitting right next to your emulator's instruction loop.
* **Direct Handoff:** You don't have to serialize data. You simply take the memory buffer containing your Ethernet frame from the VirtIO queue and pass it directly to the `netstack` stack provided by `tsnet`.

#### 2. The Implementation Logic

Instead of the complex "proxy" logic, your loop inside the emulator looks like this:

```go
// Simplified "VirtIO-Net to tsnet" bridge
func (emu *Emulator) HandleVirtIOKick() {
    // 1. Grab the raw Ethernet frame from the Guest RAM
    frame := emu.ReadGuestMemory(emu.VirtIOBufferAddress)

    // 2. Inject directly into your embedded tsnet stack
    // This is essentially just a function call in the same process
    emu.tsnetStack.InjectInboundPacket(frame) 
}

func (emu *Emulator) HandleStackResponse(frame []byte) {
    // 3. Inject the packet from Tailscale back into the Guest RAM
    emu.WriteGuestMemory(emu.VirtIOBufferAddress, frame)
    
    // 4. Trigger the interrupt to tell the Guest Kernel data is ready
    emu.TriggerPLICInterrupt(virtioIRQ)
}

```

### Key Considerations for "Fast"

* **Zero Copy:** Because the `tsnet` stack and your emulator share the same memory space, you can optimize the packet handling to avoid allocating new buffers. You can pass the underlying byte slice directly from the guest-emulated memory into the `netstack` without copying.
* **Memory Management:** Since you are in one process, keep your `VirtIO` ring buffers small and pinned in memory. This will minimize the time the CPU spends jumping between the emulator's fetch-decode-execute loop and the network stack's packet processing.
* **Identity:** Your emulator will automatically get a machine key and a Tailscale IP. You can authenticate it using an `AuthKey` provided as an environment variable or a configuration file, making the boot process completely automated.

### Why this beats "external" networking

* **Latency:** You avoid the OS network stack entirely. Packets travel from Guest memory -> Emulator -> WireGuard (in-process) -> Host Network.
* **Portability:** Your Go binary becomes a single, self-contained unit. You can drop it onto any machine (Mac, Linux, Windows), and it will immediately connect to your Tailscale network without any system-level configuration or "sudo" privileges.

3. Plan it now:

design
------

No macOS `/dev/tun`, no root, no host network interface.

Small naming nuance: Tailscale’s Go API may still call the in-process packet boundary a `tun.Device`, but we will implement that as a pure Go memory object. It is just channels/function calls inside `emu`, not a real OS TUN.

**Plan**

1. **Add virtio-net MMIO device**
   - Add FDT node at something like `0x10008000`:
     ```dts
     virtio_net@10008000 {
       compatible = "virtio,mmio";
       reg = <0x0 0x10008000 0x0 0x1000>;
       interrupts = <1>;
       interrupt-parent = <&plic>;
     };
     ```
   - Implement virtio-mmio v2 registers: magic, version, device id `1`, vendor id, feature select, driver feature select, queue select, queue size, queue addresses, queue ready, queue notify, interrupt status/ack, status, config generation, config space.
   - Advertise only minimal features first: `VIRTIO_F_VERSION_1`, `VIRTIO_NET_F_MAC`, `VIRTIO_NET_F_STATUS`, maybe `VIRTIO_NET_F_MTU`. No offloads, no control queue, no multiqueue.

2. **Implement split virtqueue core**
   - Generic helper for descriptor table, avail ring, used ring.
   - No indirect descriptors at first, because we will not advertise them.
   - Queues:
     - queue 0: RX, guest-provided writable buffers.
     - queue 1: TX, guest-submitted Ethernet frames.
   - On `QueueNotify(1)`: read TX descriptors, strip virtio-net header, extract Ethernet frame.
   - On inbound packet from host side: fill RX buffers, write used ring, raise PLIC interrupt.

3. **Add in-process tsnet packet bridge**
   - Create our own object with the conceptual API you described:
     ```go
     emu.tsnetStack.InjectInboundPacket(ethernetFrame)
     ```
   - Internally it will strip Ethernet headers and inject raw IP packets into Tailscale’s userspace stack through a pure-Go memory packet endpoint.
   - Reverse direction: packets coming back from tsnet get wrapped in Ethernet headers and delivered into virtio-net RX.
   - No real TUN. No root. No macOS network device.

4. **Handle the L2 shim**
   - Virtio-net gives Linux an Ethernet NIC; tsnet wants IP packets.
   - Implement:
     - IPv4/IPv6 EtherType pass-through by stripping/prepending Ethernet headers.
     - Minimal ARP proxy so Linux can send IPv4 packets over an Ethernet-looking link.
     - NDP can be second pass if we want IPv6 early.
   - Use stable fake MACs:
     - guest MAC from virtio config.
     - host-side MAC for ARP replies.

5. **Guest configuration path**
   - First milestone can use static config in the guest:
     ```sh
     ip link set eth0 up
     ip addr add <tailscale-ip>/32 dev eth0
     ip route add 100.64.0.0/10 dev eth0
     ```
   - Then add minimal DHCP later if we want “it just comes up” without manual guest commands.

6. **Tests**
   - Red unit tests first:
     - FDT advertises `virtio,mmio` net node only when enabled.
     - MMIO probe returns virtio magic/version/device id.
     - Linux-style queue setup writes are retained.
     - TX queue notify turns a guest Ethernet frame into an injected IP packet.
     - tsnet-side packet callback becomes a virtio RX used-buffer plus PLIC interrupt.
   - Boot smoke:
     - Kernel logs `virtio-mmio` and `virtio_net`.
     - `eth0` appears.
   - Integration smoke, skipped unless auth is present:
     - Start tsnet with `TS_AUTHKEY`.
     - Configure guest `eth0`.
     - Ping or TCP connect to a tailnet peer.

The important architecture is:

```text
guest Linux virtio_net
  -> Ethernet frame in virtqueue
  -> Go virtio-net backend
  -> emu.tsnetStack.InjectInboundPacket(frame)
  -> strip Ethernet, inject IP into in-process tsnet userspace stack
  -> Tailscale/WireGuard

Tailscale/WireGuard
  -> in-process tsnet packet callback
  -> wrap IP as Ethernet
  -> virtio RX queue
  -> PLIC interrupt
  -> guest Linux eth0
```

So yes: one process, no root, no OS TUN.
