# PLAN120: One Local Tailscale Proxy Leader With Private Guest NAT

## 1. Summary

Build guest networking around one elected local leader process. The leader binds
`127.0.0.1:7557`, owns the single persistent `tsnet.Server`, gives all local
guest Linux instances private DHCP addresses, NATs outbound IPv4 through
Tailscale, and drops unsolicited inbound traffic. Follower emulator processes
connect to the leader over loopback and proxy raw virtio-net Ethernet frames
instead of registering new Tailscale nodes.

The security posture is intentionally outbound-only for v1. A guest can open
connections to the tailnet or, if the selected Tailscale routing permits it, to
the internet. A tailnet peer cannot initiate arbitrary inbound connections into
a guest unless a later plan adds explicit port forwarding.

Default behavior:

- Leader election address: `127.0.0.1:7557`.
- Test override: `RISCV_EMU_TAILPROXY_ADDR`.
- Private guest subnet: `10.77.0.0/24`.
- Router and default gateway IP: `10.77.0.1`.
- Guest lease pool: `10.77.0.2` through `10.77.0.254`.
- Router MAC: existing host-side MAC `02:72:69:73:ff:01`.
- DNS DHCP option: `100.100.100.100`, with the existing DNS env override still
  respected.
- Tailscale state: `$HOME/.tailemu/riscv-emu/tailscaled.state`.
- Operations log: `$HOME/.tailemu/oplog.txt`.

Important behavior change:

- Guest Linux no longer receives the Tailscale IPv4 directly by DHCP.
- Guest Linux receives a private address and routes through the emulator leader
  as a tiny NAT router.

Current code facts this plan depends on:

- `cmd/emu/virtio_net.go` already exposes a guest-facing virtio-net MMIO
  device and calls `virtioNetPacketStack.InjectInboundPacket` for guest TX.
- `cmd/emu/virtio_net_stack_tsnet.go` already has a memory TUN for `tsnet`,
  DHCP and ARP helpers, persistent tsnet state, and the oplog.
- `tailsocks` is now a local package, but its existing data path is SOCKS5 over
  `tsnet`, not guest Ethernet. It can be reshaped or used as a home for shared
  election/proxy helpers, but the emulator network path should stay raw
  Ethernet to preserve normal Linux commands inside the guest.

## 2. Target Architecture

The final topology:

```text
guest Linux eth0
  -> virtio-net queue
  -> local emulator process
  -> leader if this process owns 127.0.0.1:7557
  -> follower TCP connection if another process owns 127.0.0.1:7557
  -> tailRouter
  -> NAT table
  -> virtioNetMemoryTUN
  -> tsnet.Server
  -> tailnet and optional exit-node internet
```

The leader is a local router. It has one in-process router port for its own
guest, plus one router port per follower TCP connection. The router port is the
unit of DHCP leasing and NAT ownership.

This detail matters because every emulator currently exposes the same default
virtio-net MAC. If leases were keyed only by MAC, two local guest Linux
instances could collide. Keying by router port lets two guests with identical
MACs still receive separate addresses, such as `10.77.0.2` and `10.77.0.3`.

Outbound packet path:

```text
guest frame
  -> router port
  -> DHCP/ARP/local-gateway handling if applicable
  -> IPv4 parse
  -> NAT source rewrite
  -> TUN IP packet injection
  -> tsnet
```

Inbound reply path:

```text
tsnet IP packet
  -> memory TUN callback
  -> NAT destination lookup
  -> original guest tuple restore
  -> Ethernet wrapping
  -> owning router port only
  -> local virtio-net RX or follower TCP frame
```

Unmatched inbound packets are dropped. That is not a missing route. It is the
v1 security model.

## 3. Public Interfaces And Package Shape

Keep emulator-specific virtio details in `cmd/emu`, but move generic loopback
frame transport helpers into `tailsocks` or a small subpackage beneath it. Since
`tailsocks` is owned by this repo and malleable, do not preserve the imported
main-package API at the expense of clarity.

Recommended package split:

- `tailsocks`: keep existing SOCKS5 code and auth/exit-node helpers.
- `tailsocks/tailproxy` or `tailsocks/proxy`: new loopback election and frame
  transport helpers.
- `cmd/emu`: owns `tailRouter`, NAT, DHCP, ARP, virtio-net attachment, and the
  `tsnet.Server` integration.

New helper concepts:

- `DefaultEmuProxyAddr = "127.0.0.1:7557"`.
- `ProxyAddrFromEnv()` returns `RISCV_EMU_TAILPROXY_ADDR` or the default.
- TCP handshake magic: `risemu-tailproxy-v1\n`.
- Frame wire format: big-endian `uint32` payload length followed by one raw
  Ethernet frame.
- Maximum frame length: enforce `virtioNetMaxFrameLen` at the emulator boundary.
- `ReadFrame(io.Reader, max int) ([]byte, error)`.
- `WriteFrame(io.Writer, frame []byte, max int) error`.

Leader/follower construction behavior:

- `newVirtioNetPacketStack` with the `tsnet` build tag first attempts to listen
  on the proxy address.
- If listen succeeds, this process is leader:
  - start the `tsnet.Server`;
  - create the `tailRouter`;
  - create the local in-process router port;
  - accept follower TCP connections.
- If listen fails because the address is in use, this process is follower:
  - dial the leader;
  - perform the handshake;
  - create a follower stack that implements `virtioNetPacketStack`;
  - do not construct or start `tsnet.Server`.
- If listen fails for a non-address-in-use reason, try follower dial once. If
  dialing also fails, return a clear error.

Follower behavior:

- Guest TX frames are serialized over the loopback TCP connection.
- Frames from leader are injected into the follower's local virtio-net RX queue.
- Frames received before `attachVirtioNet` are queued and flushed on attach.
- If the leader connection dies, v1 logs and drops future outbound frames. It
  does not silently register its own tsnet node, because that would recreate the
  node-count and repeated-authorization problem.

Leader behavior:

- The local guest and each follower connection are separate router ports.
- The leader sends replies only to the port that owns the lease or NAT mapping.
- The leader closes and removes a router port when a follower disconnects.
- Closing the leader closes listener, tsnet server, memory TUN, and follower
  connections.

## 4. Stage 1: Local Election And Frame Transport

Goal: make a process become either leader or follower, and prove raw Ethernet
frames can move over localhost without involving Tailscale, DHCP, or NAT yet.

Implementation details:

- Add the frame codec and handshake helpers in the new `tailsocks` proxy helper
  package.
- Keep the codec intentionally tiny:
  - read exactly 4 bytes for the frame length;
  - reject lengths greater than the supplied max;
  - read exactly that many payload bytes;
  - write length and payload under one writer lock at call sites that share a
    connection.
- Add an election helper that accepts an address and returns either:
  - a listener for leader mode, or
  - a connected TCP connection for follower mode.
- Use `net.ListenConfig` and `net.Dialer` so tests can use contexts and short
  timeouts.
- For production, refuse non-loopback addresses unless an explicit env override
  later added for tests permits it. The default must stay local-only.
- Add oplog events:
  - `proxy_election role=leader addr=...`
  - `proxy_election role=client addr=...`
  - `proxy_client_connected remote=...`
  - `proxy_client_disconnected remote=... error=...`

Stage 1 tests:

1. Frame codec round-trips a representative Ethernet frame byte-for-byte.
2. Frame codec rejects frames larger than `virtioNetMaxFrameLen`.
3. Frame codec returns a clean error on truncated length prefix.
4. Frame codec returns a clean error on truncated payload.
5. Handshake accepts exact protocol magic.
6. Handshake rejects wrong magic.
7. First election on a free loopback address returns leader/listener.
8. Second election against an occupied address returns follower/connection.
9. Follower election path does not call a tsnet construction/start hook.
10. Follower queues frames received before `attachVirtioNet`, then flushes them
    to the attached virtio-net device.

Acceptance for Stage 1:

- Existing virtio-net MMIO tests still pass.
- `go test -tags tsnet -count=1 ./cmd/emu` can compile and run focused tests
  without requiring real Tailscale authorization.

## 5. Stage 2: Private Guest DHCP, ARP, And Router Identity

Goal: make guest Linux see a normal private Ethernet LAN with a default gateway
at `10.77.0.1`, independent of whether Tailscale has authorized yet.

Implementation details:

- Introduce `tailRouter`.
- Introduce `routerPort` or an equivalent opaque port ID:
  - local leader guest has one port;
  - each follower TCP connection has one port.
- Store per-port state:
  - lease IP;
  - guest MAC learned from DHCP or ARP;
  - frame sink callback for sending Ethernet frames back to that guest.
- Allocate leases from `10.77.0.2` upward.
- Do not persist leases in v1. Guests can DHCP again after restart.
- Change DHCP from "Tailscale IP assignment" to "private LAN assignment":
  - `yiaddr`: port lease IP;
  - DHCP server: `10.77.0.1`;
  - router option: `10.77.0.1`;
  - subnet mask: `255.255.255.0`;
  - DNS: `100.100.100.100`;
  - MTU: current `virtioNetMTU`;
  - lease time: keep current 86400 seconds unless a test needs shorter.
- ARP:
  - reply only for `10.77.0.1`;
  - sender MAC is router MAC;
  - sender protocol address is `10.77.0.1`;
  - target fields mirror the guest request.
- Add local ICMP echo reply for the gateway IP:
  - `ping 10.77.0.1` should work without tsnet authorization;
  - recalculate IPv4 and ICMP checksums.
- DHCP, ARP, and gateway ping are handled before NAT.

Stage 2 tests:

1. DHCP Discover on the first router port offers `10.77.0.2`.
2. DHCP Request on the same router port ACKs the same lease.
3. DHCP options contain router/server `10.77.0.1`, subnet `/24`, DNS
   `100.100.100.100`, and MTU.
4. A second router port with the same guest MAC receives `10.77.0.3`.
5. DHCP malformed BOOTP packet is consumed safely without panic or bogus reply.
6. ARP request for `10.77.0.1` returns router MAC and correct sender/target
   fields.
7. ARP request for an unrelated private or public IP produces no reply.
8. ICMP echo to `10.77.0.1` returns an echo reply with correct checksum.
9. Non-IPv4 traffic is ignored or dropped consistently at this stage.
10. DHCP continues to answer when no Tailscale IPv4 is known yet.

Acceptance for Stage 2:

- Unit tests can instantiate the router with fake frame sinks and no tsnet.
- Booted guest `netup` should be able to get a private IP once integrated in a
  later stage.

## 6. Stage 3: Pure IPv4 NAT Engine

Goal: build NAT as a deterministic packet translator before connecting it to
the live memory TUN or Tailscale.

Implementation details:

- Add a pure NAT core owned by `tailRouter`.
- NAT outbound input:
  - owning router port;
  - raw IPv4 packet without Ethernet header;
  - current Tailscale IPv4 address.
- NAT outbound output:
  - rewritten IPv4 packet for memory TUN injection;
  - or a drop reason.
- NAT inbound input:
  - raw IPv4 packet from memory TUN;
  - current Tailscale IPv4 address.
- NAT inbound output:
  - owning router port;
  - rewritten IPv4 packet addressed to the original guest;
  - or a drop reason.
- Support these protocols in v1:
  - UDP source-port NAT;
  - TCP source-port NAT;
  - ICMP echo identifier NAT.
- Drop these in v1:
  - IPv6;
  - IPv4 fragments;
  - unsupported IPv4 protocols;
  - packets with invalid header length;
  - packets with bad enough length fields that safe parsing is impossible;
  - inbound packets with no matching NAT state.
- Recompute checksums fully:
  - IPv4 header checksum;
  - UDP checksum, preserving checksum-zero semantics if needed;
  - TCP checksum;
  - ICMP checksum.
- Decrement TTL for routed outbound packets.
- Drop TTL-expired packets in v1 rather than generating ICMP time exceeded.
- NAT allocation:
  - deterministic range, for example `40000` through `60999`;
  - wrap around;
  - avoid collisions with existing active mappings;
  - one allocator namespace for UDP/TCP ports and one for ICMP echo IDs, or one
    shared allocator if that is simpler and tested.
- NAT mapping keys:
  - outbound key includes protocol, port ID, guest IP, guest port or ICMP ID,
    remote IP, and remote port or ICMP ID as applicable;
  - inbound key includes protocol, NAT external port or ICMP ID, remote IP, and
    remote port or ICMP ID as applicable.
- Timeouts with fake-clock testability:
  - ICMP idle timeout: 30 seconds;
  - UDP idle timeout: 2 minutes;
  - TCP idle timeout: 10 minutes.
- Refresh mapping last-used time on both outbound and inbound traffic.

Stage 3 tests:

1. UDP outbound rewrites source IP to the Tailscale IPv4, allocates a NAT source
   port, decrements TTL, and produces valid IPv4/UDP checksums.
2. UDP inbound reply maps back to the original guest IP/port and owning router
   port.
3. TCP SYN outbound rewrites source IP/port and produces valid TCP checksum.
4. TCP reply inbound maps back to the original guest tuple and owning router
   port.
5. ICMP echo request outbound rewrites identifier and checksum.
6. ICMP echo reply inbound restores guest identifier and checksum.
7. Two guests using the same source port to the same remote endpoint receive
   distinct NAT mappings.
8. Unmatched inbound UDP, TCP, and ICMP packets are dropped.
9. IPv4 fragments are dropped and counted as fragments.
10. Expired NAT mappings are removed by fake-clock cleanup and no longer accept
    inbound replies.

Acceptance for Stage 3:

- NAT tests are pure unit tests with no sockets, no emulator boot, and no
  Tailscale dependency.
- The NAT core has enough counters or drop reasons to make later debugging
  possible.

## 7. Stage 4: Integrate Router, Leader, Follower, And Existing tsnet TUN

Goal: connect the pure router/NAT to virtio-net, the leader/follower TCP proxy,
and the existing `virtioNetMemoryTUN`.

Implementation details:

- Leader stack owns:
  - `tsnet.Server`;
  - `virtioNetMemoryTUN`;
  - `tailRouter`;
  - loopback listener;
  - local router port;
  - remote router port per follower connection.
- Leader local guest TX:
  - `InjectInboundPacket(frame)` calls
    `tailRouter.HandleGuestEthernet(localPort, frame)`.
- Follower TX:
  - follower writes Ethernet frame over TCP;
  - leader connection goroutine reads frame and calls
    `tailRouter.HandleGuestEthernet(remotePort, frame)`.
- Router outbound:
  - DHCP/ARP/gateway ICMP replies are Ethernet frames sent directly to the
    owning router port;
  - NAT-translated IPv4 packets are injected into `virtioNetMemoryTUN`.
- TUN inbound:
  - `handleTsnetPacket(pkt)` calls NAT inbound translation;
  - successful translation returns the owning router port and guest IP packet;
  - leader wraps that packet as Ethernet with guest destination MAC and router
    source MAC;
  - leader sends only to the owning router port.
- Follower RX:
  - TCP reader receives Ethernet frames from leader;
  - if virtio-net is attached, call `InjectGuestFrame`;
  - otherwise queue until attachment.
- Preserve persistence:
  - only leader uses `$HOME/.tailemu/riscv-emu/tailscaled.state`;
  - followers never touch tsnet state.
- Preserve auth behavior:
  - `TS_AUTHKEY`, `RISCV_EMU_TSNET_EPHEMERAL`, hostname env, and oplog are
    leader-only.
- Optional exit-node support:
  - do not require exit-node configuration for this stage;
  - keep `tailsocks` helper available for a later explicit env such as
    `RISCV_EMU_TSNET_EXIT_NODE`.

Stage 4 tests:

1. Leader local guest can complete DHCP and ARP without any TCP followers.
2. Follower guest DHCP request is carried over TCP and receives private lease
   reply.
3. Leader accepts two followers and routes replies only to the NAT-owning
   follower.
4. Packet from TUN with valid NAT mapping is Ethernet-wrapped with guest
   destination MAC and router source MAC.
5. Packet from TUN with no NAT mapping is dropped and never sent to any follower
   or local guest.
6. Follower connection close removes the router port and does not affect other
   followers.
7. Leader close shuts down listener, tsnet server, TUN, and follower
   connections.
8. Oplog records leader start, follower connect, follower disconnect,
   authorization, and tail IPv4 readiness.
9. Existing `TestTsnetDir...`, `TestTsnetOpLog...`, DHCP helper tests, and
   virtio-net MMIO tests continue to pass.
10. `go test -tags tsnet -count=0 ./cmd/emu -run '^$'` compiles cleanly.

Acceptance for Stage 4:

- Starting a second emulator while a first one is running must not create a new
  Tailscale node.
- Unit tests should prove this by injecting a fake tsnet constructor/start hook
  and verifying followers do not call it.

## 8. Stage 5: Guest Linux Smoke Tests And Manual Acceptance

Goal: prove the booted guest sees normal Linux networking under the new private
subnet model, while keeping real tailnet tests opt-in.

Implementation details:

- Update any boot smoke expectations that currently assume the guest IP is a
  `100.x` Tailscale address.
- `netup` should result in:
  - `eth0` address in `10.77.0.0/24`;
  - default route through `10.77.0.1`;
  - `/etc/resolv.conf` using `100.100.100.100`.
- Add a smoke test for:
  - boot;
  - run `netup`;
  - inspect `ifconfig`, `route`, or BusyBox equivalents;
  - verify private IP and default gateway.
- Add a smoke test for gateway ping:
  - `ping -c 1 10.77.0.1`.
- Keep Ctrl-C resilience test from the prior networking work.
- Real tailnet connectivity tests should be manual or opt-in because they
  require authorization, tailnet routes, and possibly an exit node.

Manual acceptance flow:

1. Start one emulator with `make linux`.
2. Confirm the Tailscale node authorization persists across emulator restarts.
3. In guest, run `netup`.
4. Confirm guest IP is `10.77.0.2`.
5. Confirm default route is `10.77.0.1`.
6. Confirm `ping -c 1 10.77.0.1` succeeds.
7. Confirm `ping` to a tailnet peer IP succeeds.
8. If an exit node or route to the public internet exists, confirm
   `ping -c 1 8.8.8.8` succeeds.
9. Start a second emulator.
10. Confirm it receives `10.77.0.3` and no new Tailscale node appears.

Stage 5 tests:

1. Booted guest `netup` reports an `eth0` address in `10.77.0.0/24`.
2. Booted guest route table contains default gateway `10.77.0.1`.
3. Booted guest `/etc/resolv.conf` contains `100.100.100.100`.
4. Booted guest can `ping -c 1 10.77.0.1`.
5. Booted guest foreground ping can still be interrupted with Ctrl-C.
6. Two in-process router ports in a test harness get distinct leases and
   distinct NAT mappings.
7. Optional manual test confirms only one Tailscale node appears after two
   emulator processes start.
8. Optional manual test confirms no unsolicited inbound tailnet packet reaches a
   guest without NAT state.
9. Optional manual test confirms persistent tsnet state avoids repeated device
   authorization.
10. Optional manual test confirms exit-node internet routing if an exit node is
    configured.

Acceptance for Stage 5:

- The normal guest shell commands use the host-backed Tailscale route through
  the kernel's ordinary `eth0` path. No SOCKS configuration is required inside
  the guest.

## 9. Stage 6: Hardening And Observability

Goal: make the router understandable when a packet path fails, without filling
the terminal with per-packet noise by default.

Implementation details:

- Add leader counters:
  - DHCP offers;
  - DHCP ACKs;
  - ARP replies;
  - gateway ICMP replies;
  - outbound NAT packets by protocol;
  - inbound NAT packets by protocol;
  - drops by reason.
- Drop reasons:
  - no Tailscale IPv4 yet;
  - no NAT mapping;
  - IPv4 fragment;
  - unsupported protocol;
  - bad packet length;
  - bad header length;
  - TTL expired;
  - closed router port;
  - follower write error.
- Log high-level state transitions to oplog:
  - election result;
  - leader listener start;
  - follower connect/disconnect;
  - tsnet start;
  - authorization;
  - Tailscale IPv4 readiness;
  - router port allocation/removal.
- Do not log every packet by default.
- Add optional trace env:
  - `RISCV_EMU_TAILPROXY_TRACE=1`;
  - logs packet-level summaries and drop reasons to stderr or the oplog,
    whichever is least disruptive in the current code style.
- Counter reads must be race-safe.
- Closing leader during traffic must not deadlock.

Stage 6 tests:

1. Drop counter increments for unmatched inbound packet.
2. Drop counter increments for fragmented outbound packet.
3. Drop counter increments for unsupported protocol.
4. Drop counter increments for TTL-expired packet.
5. NAT counters increment for UDP, TCP, and ICMP success paths.
6. Oplog contains high-level state events but not per-packet logs by default.
7. Trace env enables packet-level debug output in a test logger.
8. Non-loopback proxy bind address is rejected unless an explicit test override
   is set.
9. Counter reads are race-safe under concurrent follower traffic.
10. Closing leader while packets are in flight does not deadlock.

Acceptance for Stage 6:

- A failed ping can be diagnosed from counters and oplog events without adding
  ad hoc prints.
- The default terminal remains quiet enough for normal guest Linux use.

## 10. Implementation Order And Merge Safety

Recommended implementation sequence:

1. Add frame codec, handshake, and election helpers with tests.
2. Add follower stack skeleton that can proxy frames but is not yet selected by
   default.
3. Add `tailRouter` with fake ports, private DHCP, ARP, and gateway ping.
4. Add pure NAT with exhaustive unit tests.
5. Wire leader stack to router and memory TUN.
6. Wire follower stack into `newVirtioNetPacketStack` election.
7. Update boot smoke tests from Tailscale-IP expectation to private-IP
   expectation.
8. Add counters and trace logging.
9. Run focused tsnet-tag tests.
10. Run manual two-emulator acceptance.

Keep changes small enough that each stage can stand on its tests. Do not jump
directly from current DHCP-to-Tailscale-IP behavior to full leader NAT without
the pure NAT and fake-port router tests first.

## 11. Out Of Scope For This Plan

- Inbound port forwarding from tailnet to a guest.
- IPv6 guest routing or NAT.
- Persistent DHCP leases.
- Automatic leader failover where followers re-elect and one registers tsnet.
- Bridging arbitrary L2 broadcast domains across emulator processes.
- Replacing `tsnet` with a host TUN device.
- Requiring guest commands to use SOCKS, HTTP proxy env vars, or custom user
  tools.

## 12. Assumptions And Defaults

- IPv4 NAT is enough for v1 because the current immediate debugging target is
  `ping`, DNS, and ordinary outbound Linux command networking.
- No-inbound is an intentional security feature, not a missing implementation.
- Public internet access through targets like `8.8.8.8` requires Tailscale
  routing that can reach the internet, usually an exit node. The emulator NAT
  makes that path possible but does not create an exit route by itself.
- The existing `tailsocks` package can be changed freely. Use it where it helps,
  especially for auth, exit-node prefs, and proxy helper organization, but do
  not force the guest data path through SOCKS.
- Lease persistence is not required. Runtime leases are enough.
- Followers do not fall back to direct `tsnet` registration after leader loss.
  This avoids surprise device creation and repeated authorization.
- Existing tsnet state persistence remains canonical:
  `$HOME/.tailemu/riscv-emu/tailscaled.state`.
- The operations log remains canonical:
  `$HOME/.tailemu/oplog.txt`, with timestamps using
  `2006-01-02T15:04:05.000Z07:00`.

