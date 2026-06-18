package emunet

import (
	"context"
	"testing"
	"time"
)

func TestDNSServerLookup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ln, err := ListenRendezvous(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	want := EmunetDNS{
		LeaderURL:         "tcp://127.0.0.1:30001/emunet/leader",
		KnownFollowerURLs: []string{"tcp://127.0.0.1:30002/emunet/follower"},
	}
	srv := StartDNSServer(ctx, ln, func() EmunetDNS { return want })
	defer srv.Close()

	got, err := LookupDNS(ctx, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if got.LeaderURL != want.LeaderURL {
		t.Fatalf("LeaderURL: got %q want %q", got.LeaderURL, want.LeaderURL)
	}
	if len(got.KnownFollowerURLs) != 1 || got.KnownFollowerURLs[0] != want.KnownFollowerURLs[0] {
		t.Fatalf("KnownFollowerURLs: got %#v want %#v", got.KnownFollowerURLs, want.KnownFollowerURLs)
	}
}

func TestListenRendezvousOccupiedPortFailsCleanly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ln, err := ListenRendezvous(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ln2, err := ListenRendezvous(ctx, ln.Addr().String())
	if err == nil {
		ln2.Close()
		t.Fatalf("expected occupied rendezvous address to fail")
	}
}

func TestListenRendezvousRejectsNonLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if ln, err := ListenRendezvous(ctx, "0.0.0.0:0"); err == nil {
		ln.Close()
		t.Fatalf("expected non-loopback rendezvous address to be rejected")
	}
}
