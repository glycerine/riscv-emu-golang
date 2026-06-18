package emunet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	rpc25519 "github.com/glycerine/rpc25519"
)

type Circuit = rpc25519.Circuit

type NodeOptions struct {
	ServerAddr  string
	ServiceName string
	PeerName    string
	EventBuffer int
}

type Event struct {
	Circuit *Circuit
	Message Message
	Err     error
}

type Node struct {
	srv *rpc25519.Server
	lpb *rpc25519.LocalPeer

	serviceName string
	peerURL     string
	events      chan Event
	closeOnce   sync.Once
}

func StartNode(ctx context.Context, opt NodeOptions) (*Node, error) {
	if opt.ServerAddr == "" {
		opt.ServerAddr = "127.0.0.1:0"
	}
	if opt.ServiceName == "" {
		opt.ServiceName = ServiceName
	}
	if opt.PeerName == "" {
		opt.PeerName = fmt.Sprintf("emunet-%d", os.Getpid())
	}
	if opt.EventBuffer <= 0 {
		opt.EventBuffer = 64
	}

	cfg := rpc25519.NewConfig()
	cfg.TCPonly_no_TLS = true
	cfg.QuietTestMode = true
	cfg.ServerAddr = opt.ServerAddr
	cfg.ServerAutoCreateClientsToDialOtherServers = true

	srv := rpc25519.NewServer("srv_"+opt.PeerName, cfg)
	if _, err := srv.Start(); err != nil {
		return nil, err
	}

	n := &Node{
		srv:         srv,
		serviceName: opt.ServiceName,
		events:      make(chan Event, opt.EventBuffer),
	}
	service := &peerService{node: n}
	if err := srv.PeerAPI.RegisterPeerServiceFunc(opt.ServiceName, service.Start); err != nil {
		_ = srv.Close()
		return nil, err
	}

	lpb, err := srv.PeerAPI.StartLocalPeer(ctx, opt.ServiceName, "", nil, opt.PeerName, false)
	if err != nil {
		_ = srv.Close()
		return nil, err
	}
	n.lpb = lpb
	n.peerURL = lpb.URL()
	return n, nil
}

func (n *Node) PeerURL() string {
	if n == nil {
		return ""
	}
	return n.peerURL
}

func (n *Node) Events() <-chan Event {
	return n.events
}

func (n *Node) Connect(ctx context.Context, peerURL string, first *Message) (*Circuit, error) {
	if n == nil || n.lpb == nil {
		return nil, errors.New("emunet node is not started")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	frag, err := n.fragment(first)
	if err != nil {
		return nil, err
	}
	ckt, _, _, err := n.lpb.NewCircuitToPeerURL(CircuitName, peerURL, frag, 0, "", "")
	if err != nil {
		return nil, err
	}
	go (&peerService{node: n}).readCircuit(n.lpb.Ctx, ckt)
	return ckt, nil
}

func (n *Node) SendMessage(ckt *Circuit, msg Message) error {
	if n == nil || n.lpb == nil {
		return errors.New("emunet node is not started")
	}
	if ckt == nil {
		return errors.New("emunet circuit is nil")
	}
	frag, err := n.fragment(&msg)
	if err != nil {
		return err
	}
	_, err = ckt.SendOneWay(frag, 0, 0)
	return err
}

func (n *Node) SendFrame(ckt *Circuit, msg Message, frame []byte) error {
	msg.Kind = MessageKindEthernetFrame
	msg.Frame = frame
	return n.SendMessage(ckt, msg)
}

func (n *Node) Close() error {
	if n == nil {
		return nil
	}
	var err error
	n.closeOnce.Do(func() {
		if n.lpb != nil {
			n.lpb.Close()
		}
		if n.srv != nil {
			err = n.srv.Close()
		}
	})
	return err
}

func (n *Node) fragment(msg *Message) (*rpc25519.Fragment, error) {
	frag := n.lpb.NewFragment()
	frag.FragSubject = FragmentSubject
	if msg != nil {
		payload, err := MarshalMessage(*msg)
		if err != nil {
			return nil, err
		}
		frag.Payload = payload
	}
	return frag, nil
}

type peerService struct {
	node *Node
}

func (s *peerService) Start(myPeer *rpc25519.LocalPeer, ctx context.Context, newCircuitCh <-chan *rpc25519.Circuit) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case ckt, ok := <-newCircuitCh:
			if !ok {
				return nil
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.readCircuit(ctx, ckt)
			}()
		case <-ctx.Done():
			return ctx.Err()
		case <-myPeer.Halt.ReqStop.Chan:
			return nil
		}
	}
}

func (s *peerService) readCircuit(ctx context.Context, ckt *rpc25519.Circuit) {
	defer ckt.Close(nil)
	for {
		select {
		case frag, ok := <-ckt.Reads:
			if !ok {
				return
			}
			if frag == nil || frag.FragSubject != FragmentSubject {
				continue
			}
			msg, err := UnmarshalMessage(frag.Payload)
			s.emit(Event{Circuit: ckt, Message: msg, Err: err})
		case fragErr, ok := <-ckt.Errors:
			if !ok {
				return
			}
			if fragErr == nil {
				continue
			}
			errText := fragErr.Err
			if errText == "" {
				errText = string(fragErr.Payload)
			}
			if errText == "" {
				errText = "emunet circuit error"
			}
			s.emit(Event{Circuit: ckt, Err: errors.New(errText)})
		case <-ckt.Context.Done():
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *peerService) emit(ev Event) {
	if s == nil || s.node == nil {
		return
	}
	select {
	case s.node.events <- ev:
	case <-ev.Circuit.Context.Done():
	}
}
