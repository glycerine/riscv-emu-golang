package emunet

import (
	"bytes"
	"testing"

	"github.com/glycerine/greenpack/msgp"
)

func TestEmunetDNSRoundTrip(t *testing.T) {
	want := EmunetDNS{
		LeaderURL:         "tcp://127.0.0.1:30001/emunet/leader",
		KnownFollowerURLs: []string{"tcp://127.0.0.1:30002/emunet/follower-a", "tcp://127.0.0.1:30003/emunet/follower-b"},
	}
	b, err := MarshalDNS(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalDNS(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.LeaderURL != want.LeaderURL {
		t.Fatalf("LeaderURL mismatch: got %q want %q", got.LeaderURL, want.LeaderURL)
	}
	if len(got.KnownFollowerURLs) != len(want.KnownFollowerURLs) {
		t.Fatalf("KnownFollowerURLs len: got %d want %d", len(got.KnownFollowerURLs), len(want.KnownFollowerURLs))
	}
	for i := range want.KnownFollowerURLs {
		if got.KnownFollowerURLs[i] != want.KnownFollowerURLs[i] {
			t.Fatalf("KnownFollowerURLs[%d]: got %q want %q", i, got.KnownFollowerURLs[i], want.KnownFollowerURLs[i])
		}
	}
}

func TestMessageRoundTripLargeFrame(t *testing.T) {
	frame := bytes.Repeat([]byte{0xab}, 128*1024)
	want := NewMessage(MessageKindEthernetFrame)
	want.NodeID = "node-a"
	want.PeerURL = "tcp://127.0.0.1:30002/emunet/node-a"
	want.MAC = []byte{0x02, 0, 0, 0, 1, 0xaa}
	want.Frame = frame

	b, err := MarshalMessage(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != ProtocolVersion {
		t.Fatalf("version: got %d want %d", got.Version, ProtocolVersion)
	}
	if got.Kind != MessageKindEthernetFrame || got.NodeID != want.NodeID || got.PeerURL != want.PeerURL {
		t.Fatalf("metadata mismatch: got %#v want %#v", got, want)
	}
	if !bytes.Equal(got.MAC, want.MAC) {
		t.Fatalf("mac mismatch: got %x want %x", got.MAC, want.MAC)
	}
	if !bytes.Equal(got.Frame, frame) {
		t.Fatalf("frame mismatch after round trip")
	}
}

func TestUnmarshalMessageTruncated(t *testing.T) {
	msg := NewMessage(MessageKindEthernetFrame)
	msg.Frame = []byte{1, 2, 3, 4, 5}
	b, err := MarshalMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalMessage(b[:len(b)/2]); err == nil {
		t.Fatalf("expected truncated greenpack payload to fail")
	}
}

func TestEmunetDNSIgnoresFutureField(t *testing.T) {
	var b []byte
	b = msgp.AppendMapHeader(b, 3)
	b = msgp.AppendString(b, "LeaderURL_zid00_str")
	b = msgp.AppendString(b, "tcp://127.0.0.1:30001/emunet/leader")
	b = msgp.AppendString(b, "KnownFollowerURLs_zid01_slc")
	b = msgp.AppendArrayHeader(b, 1)
	b = msgp.AppendString(b, "tcp://127.0.0.1:30002/emunet/follower")
	b = msgp.AppendString(b, "FutureField_zid99_str")
	b = msgp.AppendString(b, "future value")

	got, err := UnmarshalDNS(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.LeaderURL == "" || len(got.KnownFollowerURLs) != 1 {
		t.Fatalf("failed to decode known fields with future field present: %#v", got)
	}
}
