package emunet

import "fmt"

func MarshalDNS(dns EmunetDNS) ([]byte, error) {
	return dns.MarshalMsg(nil)
}

func UnmarshalDNS(b []byte) (EmunetDNS, error) {
	var dns EmunetDNS
	left, err := dns.UnmarshalMsg(b)
	if err != nil {
		return EmunetDNS{}, err
	}
	if len(left) != 0 {
		return EmunetDNS{}, fmt.Errorf("emunet dns decode left %d trailing bytes", len(left))
	}
	return dns, nil
}

func MarshalMessage(msg Message) ([]byte, error) {
	if msg.Version == 0 {
		msg.Version = ProtocolVersion
	}
	return msg.MarshalMsg(nil)
}

func UnmarshalMessage(b []byte) (Message, error) {
	var msg Message
	left, err := msg.UnmarshalMsg(b)
	if err != nil {
		return Message{}, err
	}
	if len(left) != 0 {
		return Message{}, fmt.Errorf("emunet message decode left %d trailing bytes", len(left))
	}
	return msg, nil
}
