package emunet

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

func TestNodesExchangeFramesOverCircuit(t *testing.T) {
	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	leader, err := StartNode(ctx, NodeOptions{PeerName: "emunet-leader-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	follower, err := StartNode(ctx, NodeOptions{PeerName: "emunet-follower-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close()

	if leader.PeerURL() == "" || follower.PeerURL() == "" {
		t.Fatalf("peer URLs must be non-empty: leader=%q follower=%q", leader.PeerURL(), follower.PeerURL())
	}
	if leader.PeerURL() == follower.PeerURL() {
		t.Fatalf("peer URLs must be distinct: %q", leader.PeerURL())
	}

	hello := NewMessage(MessageKindHello)
	hello.NodeID = "follower-node"
	hello.PeerURL = follower.PeerURL()
	hello.MAC = []byte{0x02, 0, 0, 0, 1, 0x88}
	ckt, err := follower.Connect(ctx, leader.PeerURL(), &hello)
	if err != nil {
		t.Fatal(err)
	}
	defer ckt.Close(nil)

	leaderHello := recvEvent(t, ctx, leader)
	if leaderHello.Message.Kind != MessageKindHello {
		t.Fatalf("leader got message kind %q, want %q", leaderHello.Message.Kind, MessageKindHello)
	}
	if leaderHello.Message.PeerURL != follower.PeerURL() {
		t.Fatalf("leader saw follower URL %q, want %q", leaderHello.Message.PeerURL, follower.PeerURL())
	}
	if !bytes.Equal(leaderHello.Message.MAC, hello.MAC) {
		t.Fatalf("leader saw mac %x, want %x", leaderHello.Message.MAC, hello.MAC)
	}

	replyFrame := []byte{9, 8, 7, 6}
	reply := NewMessage(MessageKindEthernetFrame)
	reply.NodeID = "leader-node"
	reply.PeerURL = leader.PeerURL()
	if err := leader.SendFrame(leaderHello.Circuit, reply, replyFrame); err != nil {
		t.Fatal(err)
	}
	followerReply := recvEvent(t, ctx, follower)
	if followerReply.Message.Kind != MessageKindEthernetFrame {
		t.Fatalf("follower got message kind %q, want %q", followerReply.Message.Kind, MessageKindEthernetFrame)
	}
	if !bytes.Equal(followerReply.Message.Frame, replyFrame) {
		t.Fatalf("follower frame %v, want %v", followerReply.Message.Frame, replyFrame)
	}

	txFrame := []byte{1, 2, 3, 4, 5}
	tx := NewMessage(MessageKindEthernetFrame)
	tx.NodeID = "follower-node"
	tx.PeerURL = follower.PeerURL()
	if err := follower.SendFrame(ckt, tx, txFrame); err != nil {
		t.Fatal(err)
	}
	leaderFrame := recvEvent(t, ctx, leader)
	if !bytes.Equal(leaderFrame.Message.Frame, txFrame) {
		t.Fatalf("leader frame %v, want %v", leaderFrame.Message.Frame, txFrame)
	}
}

func TestDNSCanPublishKnownFollowerPeerURLs(t *testing.T) {
	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	leader, err := StartNode(ctx, NodeOptions{PeerName: "emunet-dns-leader-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer leader.Close()
	follower, err := StartNode(ctx, NodeOptions{PeerName: "emunet-dns-follower-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close()

	ln, err := ListenRendezvous(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dnsSrv := StartDNSServer(ctx, ln, func() EmunetDNS {
		return EmunetDNS{
			LeaderURL:         leader.PeerURL(),
			KnownFollowerURLs: []string{follower.PeerURL()},
		}
	})
	defer dnsSrv.Close()

	dns, err := LookupDNS(ctx, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if dns.LeaderURL != leader.PeerURL() {
		t.Fatalf("LeaderURL: got %q want %q", dns.LeaderURL, leader.PeerURL())
	}
	if len(dns.KnownFollowerURLs) != 1 || dns.KnownFollowerURLs[0] != follower.PeerURL() {
		t.Fatalf("KnownFollowerURLs: got %#v want [%q]", dns.KnownFollowerURLs, follower.PeerURL())
	}
}

func recvEvent(t *testing.T, ctx context.Context, n *Node) Event {
	t.Helper()
	select {
	case ev := <-n.Events():
		if ev.Err != nil {
			t.Fatalf("unexpected emunet event error: %v", ev.Err)
		}
		if ev.Circuit == nil {
			t.Fatalf("event circuit is nil: %#v", ev)
		}
		return ev
	case <-ctx.Done():
		t.Fatal(fmt.Errorf("timed out waiting for emunet event: %w", ctx.Err()))
		return Event{}
	}
}
