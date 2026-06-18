package emunet

//go:generate greenpack

const (
	ProtocolVersion = 1

	MessageKindHello         = "hello"
	MessageKindLeaderURL     = "leader-url"
	MessageKindEthernetFrame = "ethernet-frame"
	MessageKindError         = "error"

	FragmentSubject = "emunet"
	CircuitName     = "emunet"
	ServiceName     = "emunet"
)

// EmunetDNS is the one-shot payload served by the local rendezvous socket.
// Clients send "HELLO EMUNET\n", read one greenpack EmunetDNS value until EOF,
// and then close the socket.
type EmunetDNS struct {
	LeaderURL         string   `zid:"0"`
	KnownFollowerURLs []string `zid:"1"`
}

// Message is the versioned emunet payload carried inside rpc25519 fragments.
type Message struct {
	Version   int    `zid:"0"`
	Kind      string `zid:"1"`
	NodeID    string `zid:"2"`
	PeerURL   string `zid:"3"`
	MAC       []byte `zid:"4"`
	LeaderURL string `zid:"5"`
	Error     string `zid:"6"`
	Frame     []byte `zid:"7"`
}

func NewMessage(kind string) Message {
	return Message{
		Version: ProtocolVersion,
		Kind:    kind,
	}
}
