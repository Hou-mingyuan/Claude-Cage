package forwarder

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/loopback"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

func TestGuestCancelsBeforeOutboundDialCompletes(t *testing.T) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	defer s.Close()
	addr := tcpip.AddrFrom4([4]byte{127, 0, 0, 1})
	if err := s.CreateNIC(1, loopback.New()); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: addr.WithPrefix()}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})

	upstream, peer := net.Pipe()
	defer upstream.Close()
	defer peer.Close()
	dialStarted := make(chan struct{})
	guestDone := make(chan struct{})
	f := TCP(s, nil, &sync.Mutex{}, false, nil, func(context.Context, string, string) (net.Conn, error) {
		close(dialStarted)
		<-guestDone
		return upstream, nil
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, f.HandlePacket)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		c, err := gonet.DialContextTCP(ctx, s, tcpip.FullAddress{NIC: 1, Addr: addr, Port: 443}, ipv4.ProtocolNumber)
		if c != nil {
			c.Close()
		}
		result <- err
	}()
	select {
	case <-dialStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("forwarder did not receive guest connection")
	}
	cancel()
	err := <-result
	close(guestDone)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("guest dial = %v", err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("abandoned upstream connection remained open: %v", err)
	}
}
