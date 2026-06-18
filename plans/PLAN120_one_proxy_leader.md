# PLAN120: Emunet Design Plan: One Local Tailscale Proxy Leader With Emunet NAT

## 1. Summary

Build **emunet**, the private guest-side emulator network, around one elected
local leader process. The leader binds `127.0.0.1:7557`, owns the single
persistent `tsnet.Server`, gives all local guest Linux instances emunet DHCP
addresses, NATs outbound IPv4 from emunet to the Tailscale tailnet, and drops
unsolicited inbound traffic. Every emulator process starts its own
`rpc25519.Server` and local emunet peer. Follower emulator processes use
`127.0.0.1:7557` as a tiny local DNS/rendezvous endpoint. The elected leader
replies to `HELLO EMUNET\n` with a greenpack-encoded `EmunetDNS` containing the
leader's canonical rpc25519 peer URL and known follower peer URLs. Followers
then exchange greenpack-encoded emunet messages over rpc25519
Peer/Fragment/Circuit circuits instead of registering new Tailscale nodes.

The security posture is intentionally outbound-only for v1. An emunet guest can
open connections to the tailnet or, if the selected Tailscale routing permits
it, to the internet. A tailnet peer cannot initiate arbitrary inbound
connections into an emunet guest unless a later plan adds explicit port
forwarding.

Default behavior:

- Emunet leader election address: `127.0.0.1:7557`.
- Test override: `RISCV_EMU_EMUNET_ADDR`.
- Emunet subnet: `10.77.0.0/24`.
- Emunet router and default gateway IP: `10.77.0.1`.
- Emunet guest lease pool: `10.77.0.2` through `10.77.0.254`.
- Emunet router MAC: existing host-side locally administered unicast MAC
  `02:72:69:73:ff:01`.
- Emunet guest MAC: invented per emulator node as a locally administered unicast MAC.
  The first octet must have the IEEE local bit set and multicast bit clear,
  the middle four octets must include the local process ID, and the remaining
  octets come from `crypto/rand`.
- DNS DHCP option: `100.100.100.100`, with the existing DNS env override still
  respected.
- Tailscale state: `$HOME/.tailemu/riscv-emu/tailscaled.state`.
- Operations log: each emulator node writes its own
  `$HOME/.local/state/emunet/oplog.${PID}`.
- The leader also writes their pid to $HOME/.tailemu/leader.${PID} file upon
winning the election, and deletes any other stale leader.${PID} files
at the same time. here ${PID} stands for the leader process's own process ID.

Important behavior change:

- Guest Linux no longer receives the Tailscale IPv4 directly by DHCP.
- Guest Linux receives an emunet address and routes through the emulator leader
  as a tiny emunet NAT router.

Current code facts this plan depends on:

- `cmd/emu/virtio_net.go` already exposes a guest-facing virtio-net MMIO
  device and calls `virtioNetPacketStack.InjectInboundPacket` for guest TX.
- `cmd/emu/virtio_net_stack_tsnet.go` already has a memory TUN for `tsnet`,
  DHCP and ARP helpers, persistent tsnet state, and the oplog.
- `tailsocks` is now a local package, but its existing data path is SOCKS5 over
  `tsnet`, not guest Ethernet.
- `rpc25519` provides the emunet loopback control/data plane. Every emulator
  process starts an rpc25519 server/local peer. The election port is only the
  local DNS/rendezvous. Canonical contact is always by rpc25519 peer URL.

## 2. Target Architecture

The final topology:

```text
guest Linux eth0
  -> virtio-net queue
  -> local emulator process
  -> leader if this process owns 127.0.0.1:7557
  -> follower rpc25519 circuit if another process owns 127.0.0.1:7557
  -> emunetRouter
  -> emunet NAT table
  -> virtioNetMemoryTUN
  -> tsnet.Server
  -> tailnet and optional exit-node internet
```

The leader is a local emunet router. It has one in-process emunet port for its
own guest, plus one emunet port per follower rpc25519 circuit. Each emulator node
must expose its own invented locally administered unicast virtio-net MAC. The
emunet port remains the unit of NAT ownership and return-path delivery, while
DHCP can associate a lease with the guest MAC learned on that emunet port.

Outbound packet path:

```text
guest frame
  -> emunet port
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
  -> owning emunet port only
  -> local virtio-net RX or follower rpc25519 frame
```

Unmatched inbound packets are dropped. That is not a missing route. It is the
v1 security model.

## 3. Public Interfaces And Package Shape

Keep emulator-specific virtio details in `cmd/emu`, but move generic loopback
peer bootstrap and message transport helpers into a root `emunet` package.
`tailsocks` stays separate as a SOCKS5-over-tsnet helper package.

Recommended package split:

- `emunet`: root package for loopback election, leader URL rendezvous,
  rpc25519 peer/circuit setup, and greenpack emunet message helpers.
- `tailsocks`: keep existing SOCKS5 code and auth/exit-node helpers.
- `cmd/emu`: owns `emunetRouter`, NAT, DHCP, ARP, virtio-net attachment, and the
  `tsnet.Server` integration.

New helper concepts:

- `DefaultEmunetAddr = "127.0.0.1:7557"`.
- `EmunetAddrFromEnv()` returns `RISCV_EMU_EMUNET_ADDR` or the default.
- Bootstrap DNS service: a tiny TCP server bound to the election address. A
  client sends exactly `HELLO EMUNET\n`, receives one greenpack-encoded
  `EmunetDNS` payload, and closes the socket.
- `EmunetDNS` begins as:
  - `LeaderURL string`;
  - `KnownFollowerURLs []string`.
- The leader's rpc25519 server is separate from the bootstrap DNS socket and may
  bind an ephemeral local port. Followers also start their own rpc25519 servers
  and advertise their own peer URLs to the leader over emunet circuits.
- Canonical peer URL: once learned, a follower uses the returned URL for emunet
  circuits. The bootstrap address can later move to a replacement leader
  without changing established rpc25519 circuit identity rules.
- Greenpack envelope: `emunet.Message` carries versioned `Kind`, `NodeID`,
  local guest `MAC`, optional `LeaderURL`, optional `Error`, and raw Ethernet
  `Frame` payload bytes.
- Message kinds begin with `hello`, `leader-url`, and `ethernet-frame`.
- No emunet transport-level Ethernet frame size cap. The loopback transport
  carries the greenpack payload it is given.

Leader/follower construction behavior:

- `newVirtioNetPacketStack` with the `tsnet` build tag first attempts to listen
  on the emunet address.
- Before election, every process starts an rpc25519 server and local emunet peer
  and obtains its own canonical peer URL.
- If listen succeeds, this process is leader:
  - start the `tsnet.Server`;
  - create the `emunetRouter`;
  - create the local in-process emunet port;
  - start the simple TCP bootstrap DNS server on the already-acquired election
    listener;
  - answer `HELLO EMUNET\n` with greenpack `EmunetDNS{LeaderURL: ownPeerURL,
    KnownFollowerURLs: ...}`.
- If listen fails because the address is in use, this process is follower:
  - dial the bootstrap service;
  - send `HELLO EMUNET\n`;
  - receive the greenpack `EmunetDNS`;
  - establish an emunet circuit to that peer URL;
  - create a follower stack that implements `virtioNetPacketStack`;
  - do not construct or start `tsnet.Server`.
- If listen fails for a non-address-in-use reason, try follower dial once. If
  dialing also fails, return a clear error.

Follower behavior:

- Guest TX frames are sent as greenpack `ethernet-frame` messages over the
  rpc25519 emunet circuit.
- Frames from leader are injected into the follower's local virtio-net RX queue.
- Frames received before `attachVirtioNet` are queued and flushed on attach.
- Followers keep a background emunet election loop that periodically tries to
  bind the emunet address. While the current leader owns the port, bind fails
  and the follower remains a client. If the port becomes available, exactly one
  follower wins the bind, promotes itself to leader, and starts `tsnet` using
  the shared persistent state directory.

Leader behavior:

- The local guest and each follower connection are separate emunet ports.
- The leader sends replies only to the port that owns the lease or NAT mapping.
- The leader closes and removes an emunet port when a follower circuit closes.
- Closing the leader closes rpc25519 server, tsnet server, memory TUN, and
  follower circuits.

## 4. Stage 1: Emunet Rendezvous And Circuit Transport

Goal: make every process start a canonical rpc25519 peer, then become either
leader or follower. Prove the local bootstrap DNS endpoint returns the canonical
leader peer URL plus known follower URLs, and prove Ethernet frames can move
over rpc25519 circuits without involving Tailscale, DHCP, or NAT yet.

Implementation details:

- Add greenpack `EmunetDNS` and message envelope helpers in the new root
  `emunet` package.
- Add an rpc25519 transport helper that accepts an address and either:
  - starts this node's rpc25519 server and local peer service;
  - wins the bootstrap DNS listener and serves `EmunetDNS`; or
  - connects to the bootstrap DNS listener, obtains the leader peer URL, and
    opens an emunet circuit to it.
- Use `net.ListenConfig` and `net.Dialer` so tests can use contexts and short
  timeouts for preflight bind/dial checks. This preflight is required because
  `rpc25519.Server.Start()` currently fatal-exits on bind failure.
- For production, refuse non-loopback addresses unless an explicit env override
  later added for tests permits it. The default must stay local-only.
- `127.0.0.1:7557` remains the election medium. Follower failover polling uses
  bind preflight; only the process that wins preflight may construct the
  simple bootstrap DNS server on that address.
- Add oplog events:
  - `emunet_election role=leader addr=...`
  - `emunet_election role=client addr=...`
  - `emunet_dns_lookup leader_url=... follower_count=...`
  - `emunet_leader_url url=...`
  - `emunet_circuit_connected remote=...`
  - `emunet_circuit_disconnected remote=... error=...`
  - `emunet_client_connected remote=...`
  - `emunet_client_disconnected remote=... error=...`

Stage 1 tests:

1. Greenpack `EmunetDNS` round-trips leader URL and known follower URLs.
2. Greenpack envelope round-trips a representative Ethernet frame byte-for-byte.
3. Greenpack envelope round-trips a payload larger than `virtioNetMaxFrameLen`
   to prove emunet does not impose an Ethernet frame cap.
4. Greenpack decode returns a clean error on truncated payload.
5. Unknown future greenpack fields are ignored by older readers.
6. First bind preflight on a free loopback address returns available.
7. Second bind preflight against an occupied address returns unavailable without
   starting rpc25519 and without exiting.
8. Every node startup returns a non-empty canonical peer URL.
9. Leader bootstrap DNS replies to `HELLO EMUNET\n` with exactly the leader's
   canonical peer URL.
10. Leader bootstrap DNS includes known follower URLs when present.
11. Follower sends an `ethernet-frame` message and the leader receives it with
   node ID and MAC metadata intact.
12. Leader sends an `ethernet-frame` reply and the follower receives it intact.
13. Follower election path does not call a tsnet construction/start hook.
14. Follower queues frames received before `attachVirtioNet`, then flushes them
    to the attached virtio-net device.

Acceptance for Stage 1:

- Existing virtio-net MMIO tests still pass.
- `go test -tags tsnet -count=1 ./cmd/emu` can compile and run focused tests
  without requiring real Tailscale authorization.

## 5. Stage 2: Automatic Emunet Leader Failover

Goal: make failover intrinsic. Followers do not rely on heartbeat messages.
They repeatedly try to bind the emunet address; the OS listen socket is the
election medium. If the leader process exits or otherwise releases
`127.0.0.1:7557`, exactly one follower wins the bind and becomes the new leader.

Implementation details:

- Add an `emunetElectionLoop` owned by follower stacks.
- The loop periodically attempts `net.Listen("tcp", EmunetAddrFromEnv())`.
- Default polling interval: 250ms.
- Test override or constructor injection must allow a much shorter interval or
  fake ticker without sleeping in unit tests.
- While the current leader owns the port, bind fails with address-in-use and the
  follower remains a client.
- When bind succeeds:
  - keep the listener open; this listener is the election victory token;
  - transition the process from follower mode to leader mode;
  - close the old follower rpc25519 client/circuit if it is still open;
  - create `emunetRouter` and the local emunet port;
  - start `tsnet.Server` only after the listener has been acquired;
  - start the simple bootstrap DNS service on the already-acquired listener;
  - write `$HOME/.tailemu/leader.${PID}` and delete stale leader pid files.
- Followers that do not win continue as clients:
  - if their old circuit/client closes, they dial the current bootstrap leader
    and request the current canonical peer URL again;
  - if dialing fails during the handoff window, they retry;
  - they keep polling for future leader loss.
- This stage handles leader process exit or any failure mode that releases the
  emunet listen port. It intentionally does not detect a wedged leader that
  keeps the rendezvous port bound.
- Add oplog events:
  - `emunet_failover_poll_start addr=... interval=...`
  - `emunet_failover_promote pid=... addr=...`
  - `emunet_failover_reconnect addr=...`
  - `emunet_failover_reconnect_error addr=... error=...`
  - `emunet_leader_pidfile path=...`

Stage 2 tests:

1. A follower polling while a leader listener is open never promotes.
2. When the leader listener closes, one polling follower promotes by acquiring
   the emunet address.
3. Three followers racing after leader loss produce exactly one promoted leader.
4. The promoted follower starts the tsnet hook exactly once and only after bind
   success.
5. Followers that lose the race do not start tsnet.
6. Losing followers reconnect to the newly promoted leader after the handoff
   window.
7. Promotion closes the old follower rpc25519 circuit and does not leak the old
   read/write goroutines.
8. Closing a follower stack stops its election loop and prevents later
   promotion.
9. Failover writes `leader.${PID}` for the promoted process and removes stale
   leader pid files in the tailemu directory.
10. Oplog contains poll start, promotion, reconnect, and reconnect-error events
    where applicable.
11. A transient dial failure after leader loss does not abort the follower; it
    continues polling and retrying.
12. If the old leader process is wedged but still holds the port, followers do
    not promote, documenting the bind-as-election contract.

Acceptance for Stage 2:

- The failover tests use fake tsnet start hooks and do not require Tailscale
  authorization.
- The implementation has one source of truth for leadership: ownership of the
  emunet listen socket.

## 6. Stage 3: Emunet DHCP, ARP, And Router Identity

Goal: make guest Linux see a normal emunet Ethernet LAN with a default gateway
at `10.77.0.1`, independent of whether Tailscale has authorized yet.

Implementation details:

- Replace the fixed guest virtio-net MAC in `newVirtioNetDevice` with generated
  MAC assignment:
  - read 6 bytes from `crypto/rand`;
  - force the first octet with `(b[0] | 0x02) & 0xfe`;
  - write `uint32(os.Getpid())` big-endian into `b[1:5]`;
  - leave `b[5]` as cryptographic random data;
  - reject all-zero, broadcast, and the emunet router MAC, then retry;
  - expose a package-level test hook or small helper that accepts an
    `io.Reader` and a PID value so unit tests can generate deterministic MACs;
  - keep the existing emunet router MAC separate and stable.
- Introduce `emunetRouter`.
- Introduce `emunetPort` or an equivalent opaque port ID:
  - local leader guest has one port;
  - each follower rpc25519 circuit has one port.
- Store per-port state:
  - lease IP;
  - guest MAC learned from the node's virtio-net config, DHCP, or ARP;
  - frame sink callback for sending Ethernet frames back to that guest.
- Allocate leases from `10.77.0.2` upward.
- Do not persist leases in v1. Guests can DHCP again after restart.
- Change DHCP from "Tailscale IP assignment" to "emunet LAN assignment":
  - `yiaddr`: port lease IP;
  - DHCP server: `10.77.0.1`;
  - router option: `10.77.0.1`;
  - subnet mask: `255.255.255.0`;
  - DNS: `100.100.100.100`;
  - MTU: current `virtioNetMTU`;
  - lease time: keep current 86400 seconds unless a test needs shorter.
- ARP:
  - reply only for `10.77.0.1`;
  - sender MAC is the emunet router MAC;
  - sender protocol address is `10.77.0.1`;
  - target fields mirror the guest request.
- Add local ICMP echo reply for the gateway IP:
  - `ping 10.77.0.1` should work without tsnet authorization;
  - recalculate IPv4 and ICMP checksums.
- DHCP, ARP, and gateway ping are handled before NAT.

Stage 3 tests:

1. Guest MAC generation sets the local bit, clears the multicast bit, and
   returns a non-broadcast unicast address.
2. Guest MAC generation embeds `uint32(pid)` big-endian in bytes `1:5`.
3. Guest MAC generation with identical random entropy but different fake PIDs
   yields distinct MACs.
4. Guest MAC generation with the same fake PID but different random entropy
   yields distinct MACs.
5. DHCP Discover on the first emunet port offers `10.77.0.2`.
6. DHCP Request on the same emunet port ACKs the same lease.
7. DHCP options contain router/server `10.77.0.1`, subnet `/24`, DNS
   `100.100.100.100`, and MTU.
8. A second emunet port with a different generated guest MAC receives
   `10.77.0.3`.
9. DHCP malformed BOOTP packet is consumed safely without panic or bogus reply.
10. ARP request for `10.77.0.1` returns emunet router MAC and correct sender/target
   fields.
11. ARP request for an unrelated private or public IP produces no reply.
12. ICMP echo to `10.77.0.1` returns an echo reply with correct checksum.
13. Non-IPv4 traffic is ignored or dropped consistently at this stage.
14. DHCP continues to answer when no Tailscale IPv4 is known yet.

Acceptance for Stage 3:

- Unit tests can instantiate the emunet router with fake frame sinks and no tsnet.
- Booted guest `netup` should be able to get an emunet IP once integrated in a
  later stage.

## 7. Stage 4: Pure Emunet IPv4 NAT Engine

Goal: build NAT as a deterministic packet translator before connecting it to
the live memory TUN or Tailscale.

Implementation details:

- Add a pure NAT core owned by `emunetRouter`.
- NAT outbound input:
  - owning emunet port;
  - raw IPv4 packet without Ethernet header;
  - current Tailscale IPv4 address.
- NAT outbound output:
  - rewritten IPv4 packet for memory TUN injection;
  - or a drop reason.
- NAT inbound input:
  - raw IPv4 packet from memory TUN;
  - current Tailscale IPv4 address.
- NAT inbound output:
  - owning emunet port;
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

Stage 4 tests:

1. UDP outbound rewrites source IP to the Tailscale IPv4, allocates a NAT source
   port, decrements TTL, and produces valid IPv4/UDP checksums.
2. UDP inbound reply maps back to the original guest IP/port and owning emunet
   port.
3. TCP SYN outbound rewrites source IP/port and produces valid TCP checksum.
4. TCP reply inbound maps back to the original guest tuple and owning emunet
   port.
5. ICMP echo request outbound rewrites identifier and checksum.
6. ICMP echo reply inbound restores guest identifier and checksum.
7. Two guests using the same source port to the same remote endpoint receive
   distinct NAT mappings.
8. Unmatched inbound UDP, TCP, and ICMP packets are dropped.
9. IPv4 fragments are dropped and counted as fragments.
10. Expired NAT mappings are removed by fake-clock cleanup and no longer accept
    inbound replies.

Acceptance for Stage 4:

- NAT tests are pure unit tests with no sockets, no emulator boot, and no
  Tailscale dependency.
- The NAT core has enough counters or drop reasons to make later debugging
  possible.

## 8. Stage 5: Integrate Emunet Router, Leader, Follower, And Existing tsnet TUN

Goal: connect the pure emunet router/NAT to virtio-net, the leader/follower
emunet rpc25519 circuits, and the existing `virtioNetMemoryTUN`.

Implementation details:

- Leader stack owns:
  - `tsnet.Server`;
  - `virtioNetMemoryTUN`;
  - `emunetRouter`;
  - local rpc25519 server/peer plus the separate bootstrap DNS listener;
  - local emunet port;
  - remote emunet port per follower circuit.
- Leader local guest TX:
  - `InjectInboundPacket(frame)` calls
    `emunetRouter.HandleGuestEthernet(localEmunetPort, frame)`.
- Follower TX:
  - follower sends an `ethernet-frame` greenpack message over its rpc25519
    circuit;
  - leader circuit goroutine reads the message and calls
    `emunetRouter.HandleGuestEthernet(remoteEmunetPort, frame)`.
- Emunet router outbound:
  - DHCP/ARP/gateway ICMP replies are Ethernet frames sent directly to the
    owning emunet port;
  - NAT-translated IPv4 packets are injected into `virtioNetMemoryTUN`.
- TUN inbound:
  - `handleTsnetPacket(pkt)` calls NAT inbound translation;
  - successful translation returns the owning emunet port and guest IP packet;
  - leader wraps that packet as Ethernet with guest destination MAC and emunet
    router source MAC;
  - leader sends only to the owning emunet port.
- Follower RX:
  - rpc25519 circuit reader receives `ethernet-frame` messages from leader;
  - if virtio-net is attached, call `InjectGuestFrame`;
  - otherwise queue until attachment.
- Preserve persistence:
  - only leader uses `$HOME/.tailemu/riscv-emu/tailscaled.state`;
  - followers never touch tsnet state.
- Preserve auth behavior:
  - `TS_AUTHKEY`, `RISCV_EMU_TSNET_EPHEMERAL`, and hostname env affect only the
    elected leader's `tsnet.Server`.
  - per-node emunet runtime oplogs are written by every emulator process to its
    own PID-suffixed file.
- Optional exit-node support:
  - do not require exit-node configuration for this stage;
  - keep `tailsocks` helper available for a later explicit env such as
    `RISCV_EMU_TSNET_EXIT_NODE`.

Stage 5 tests:

1. Leader local guest can complete DHCP and ARP without any followers.
2. Follower guest DHCP request is carried over an rpc25519 circuit and receives
   an emunet lease reply.
3. Leader accepts two followers and routes replies only to the NAT-owning
   follower.
4. Packet from TUN with valid NAT mapping is Ethernet-wrapped with guest
   destination MAC and emunet router source MAC.
5. Packet from TUN with no NAT mapping is dropped and never sent to any follower
   or local guest.
6. Follower circuit close removes the emunet port and does not affect other
   followers.
7. Leader close shuts down rpc25519 server, tsnet server, TUN, and follower
   circuits.
8. Oplog records leader start, follower connect, follower disconnect,
   authorization, and tail IPv4 readiness.
9. Existing `TestTsnetDir...`, `TestTsnetOpLog...`, DHCP helper tests, and
   virtio-net MMIO tests continue to pass.
10. `go test -tags tsnet -count=0 ./cmd/emu -run '^$'` compiles cleanly.

Acceptance for Stage 5:

- Starting a second emulator while a first one is running must not create a new
  Tailscale node.
- Unit tests should prove this by injecting a fake tsnet constructor/start hook
  and verifying followers do not call it.

## 9. Stage 6: Guest Linux Smoke Tests And Manual Acceptance

Goal: prove the booted guest sees normal Linux networking under the new emunet
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
  - verify emunet IP and default gateway.
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

Stage 6 tests:

1. Booted guest `netup` reports an `eth0` address in `10.77.0.0/24`.
2. Booted guest route table contains default gateway `10.77.0.1`.
3. Booted guest `/etc/resolv.conf` contains `100.100.100.100`.
4. Booted guest can `ping -c 1 10.77.0.1`.
5. Booted guest foreground ping can still be interrupted with Ctrl-C.
6. Two in-process emunet ports in a test harness get distinct leases and
   distinct NAT mappings.
7. Optional manual test confirms only one Tailscale node appears after two
   emulator processes start.
8. Optional manual test confirms no unsolicited inbound tailnet packet reaches a
   guest without NAT state.
9. Optional manual test confirms persistent tsnet state avoids repeated device
   authorization.
10. Optional manual test confirms exit-node internet routing if an exit node is
    configured.

Acceptance for Stage 6:

- The normal guest shell commands use the host-backed Tailscale route from
  emunet through the kernel's ordinary `eth0` path. No SOCKS configuration is
  required inside the guest.

## 10. Stage 7: Hardening And Observability

Goal: make the emunet router understandable when a packet path fails, without
filling the terminal with per-packet noise by default.

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
  - closed emunet port;
  - follower write error.
- Log high-level state transitions to oplog:
  - election result;
  - leader bootstrap DNS start;
  - follower connect/disconnect;
  - tsnet start;
  - authorization;
  - Tailscale IPv4 readiness;
  - emunet port allocation/removal.
- Do not log every packet by default.
- Add optional trace env:
  - `RISCV_EMU_EMUNET_TRACE=1`;
  - logs packet-level summaries and drop reasons to stderr or the oplog,
    whichever is least disruptive in the current code style.
- Counter reads must be race-safe.
- Closing leader during traffic must not deadlock.

Stage 7 tests:

1. Drop counter increments for unmatched inbound packet.
2. Drop counter increments for fragmented outbound packet.
3. Drop counter increments for unsupported protocol.
4. Drop counter increments for TTL-expired packet.
5. NAT counters increment for UDP, TCP, and ICMP success paths.
6. Oplog contains high-level state events but not per-packet logs by default.
7. Trace env enables packet-level debug output in a test logger.
8. Non-loopback emunet bind address is rejected unless an explicit test override
   is set.
9. Counter reads are race-safe under concurrent follower traffic.
10. Closing leader while packets are in flight does not deadlock.

Acceptance for Stage 7:

- A failed ping can be diagnosed from counters and oplog events without adding
  ad hoc prints.
- The default terminal remains quiet enough for normal guest Linux use.

## 11. Implementation Order And Merge Safety

Recommended implementation sequence:

1. Add greenpack `EmunetDNS`, greenpack message envelope, bind preflight,
   simple TCP bootstrap DNS, per-node rpc25519 server/peer, and circuit
   transport helpers with tests.
2. Add follower stack skeleton that can proxy frames over an emunet circuit but
   is not yet selected by default.
3. Add automatic failover where follower stacks poll-bind the emunet address
   and promote themselves only after acquiring the listen socket.
4. Add `emunetRouter` with fake ports, emunet DHCP, ARP, and gateway ping.
5. Add pure NAT with exhaustive unit tests.
6. Wire leader stack to emunet router and memory TUN.
7. Wire follower stack into `newVirtioNetPacketStack` election and failover.
8. Update boot smoke tests from Tailscale-IP expectation to emunet-IP
   expectation.
9. Add counters and trace logging.
10. Run focused tsnet-tag tests.
11. Run manual two-emulator acceptance including leader kill and follower
    promotion.

Keep changes small enough that each stage can stand on its tests. Do not jump
directly from current DHCP-to-Tailscale-IP behavior to full emunet leader NAT
without the pure NAT and fake emunet-port router tests first.

## 12. Out Of Scope For This Plan

- Inbound port forwarding from tailnet to a guest.
- IPv6 guest routing or NAT.
- Persistent DHCP leases.
- Bridging arbitrary L2 broadcast domains across emulator processes.
- Replacing `tsnet` with a host TUN device.
- Requiring guest commands to use SOCKS, HTTP proxy env vars, or custom user
  tools.

## 13. Assumptions And Defaults

- IPv4 NAT is enough for v1 because the current immediate debugging target is
  `ping`, DNS, and ordinary outbound Linux command networking.
- No-inbound is an intentional security feature, not a missing implementation.
- Public internet access through targets like `8.8.8.8` requires Tailscale
  routing that can reach the internet, usually an exit node. The emunet NAT
  makes that path possible but does not create an exit route by itself.
- The existing `tailsocks` package can be changed freely. Use it where it helps,
  especially for auth, exit-node prefs, and emunet helper organization, but do
  not force the guest data path through SOCKS.
- Lease persistence is not required. Runtime leases are enough.
- Followers may promote to leader and register `tsnet` only after acquiring the
  emunet listen socket. The socket bind is the election authority.
- Failover detects leader loss only when the OS releases the emunet listen
  socket. A wedged process that still owns `127.0.0.1:7557` remains the leader.
- Existing tsnet state persistence remains canonical:
  `$HOME/.tailemu/riscv-emu/tailscaled.state`. This is only ever
  written and read by the elected leader who holds the bind on port 7557.
- The operations log is per emulator process:
  `$HOME/.local/state/emunet/oplog.${PID}`, with timestamps using
  `2006-01-02T15:04:05.000Z07:00`.
  Every emulator process can write its own log file.

Q & A
-------

Q: 
Do the follower nodes a) bring up the netstack of gvisor.dev/gvisor/pkg/tcpip/ TCP/IP stack which is a part of the tailscale stack, and then re-initialize if they later become leader, or b) do they just start a tailscale stack immediately but without authentiation. (b) seems more stable, but I don't know how to "not authetnicate" but still run the tcp/stack.


A:
Short answer: neither quite. The best design is:

Followers should not start tsnet at all. They should start only their own emunet rpc25519 server/peer, lose or win the 127.0.0.1:7557 election, and if they lose, proxy guest Ethernet frames to the leader.

If a follower later wins the bind because the leader died, then it promotes itself and starts tsnet.Server using the persistent state dir.

Why: tsnet.Server.Start() does create the gVisor netstack, but it also starts the Tailscale LocalBackend with WantRunning=true. If there is no persisted auth or auth key, it goes to login/auth states and prints an auth URL. That is not really a clean “unauthenticated TCP/IP stack” mode. Up() waits for ipn.Running and a Tailscale IP, and our NAT path needs that tail IP anyway.

So option b would be noisier and more fragile: every follower would spin up Tailscale machinery, possibly emit auth URLs, own magicsock/backend state, and still not be useful for routing until promoted/authenticated.

The current code already follows the better shape: every node starts emunet.StartNode before election, but only the election winner calls newEmunetLeaderTsnetVirtioStack, which creates tsnet.Server.

Relevant spots:
[cmd/emu/virtio_net_stack_tsnet.go (line 141)](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/cmd/emu/virtio_net_stack_tsnet.go:141) starts the emunet node and election.

[cmd/emu/virtio_net_stack_tsnet.go (line 159)](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/cmd/emu/virtio_net_stack_tsnet.go:159) starts tsnet only for the leader.

[cmd/emu/virtio_net_stack_tsnet.go (line 171)](/Users/jaten/go/src/github.com/glycerine/riscv-emu-golang/cmd/emu/virtio_net_stack_tsnet.go:171) makes followers connect to the leader instead.

For failover, I’d keep that same model: followers poll/retry the rendezvous bind; the first one that succeeds becomes leader and lazily starts tsnet from the persistent state. That gives us stable follower behavior, no duplicate tailnet nodes, and fast promotion once auth state exists.

