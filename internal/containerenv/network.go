// Package containerenv runs the Linux Cage environment without a nested VM.
// The host owns the network stack and its policy; a container only exchanges
// Ethernet frames over a process pipe and receives no host network interface.
package containerenv

import (
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"github.com/lima-vm/lima/v2/pkg/guestidentity"
	"github.com/sylvinus/agent-vm/internal/cage"
)

const (
	GuestIP    = "192.168.64.2"
	GatewayIP  = "192.168.64.1"
	GuestMAC   = "5a:94:ef:e4:0c:02"
	GatewayMAC = "5a:94:ef:e4:0c:01"
	TapName    = "cagetap0"
	NetworkMTU = 1500
)

// Network owns the only path between the container and the outside network.
// The caller must close the process endpoint when Run returns. A gate starts
// closed; connecting SSH does not authorize any guest outbound traffic.
type Network struct {
	Gate     *cage.EgressGate
	stack    *virtualnetwork.VirtualNetwork
	guestMAC net.HardwareAddr
}

func networkConfiguration(gate *cage.EgressGate, identity *guestidentity.Identity) types.Configuration {
	guestMAC, gatewayMAC := GuestMAC, GatewayMAC
	if identity != nil {
		guestMAC, gatewayMAC = identity.MACAddress, identity.GatewayMACAddress
	}
	return types.Configuration{
		MTU:    NetworkMTU,
		Subnet: "192.168.64.0/24", GatewayIP: GatewayIP,
		GatewayMacAddress: gatewayMAC,
		DHCPStaticLeases:  map[string]string{GuestIP: guestMAC},
		// No NAT-to-host entries and no automatically published ports.
		NAT: map[string]string{}, Forwards: map[string]string{},
		Ec2MetadataAccess: false,
		Outbound:          gate.Allows, DialContext: gate.DialContext,
		DNSExchange: gate.ExchangeDNS,
	}
}

func NewNetwork(cfg cage.Config) (*Network, error) { return newNetwork(cfg, nil) }

func newNetwork(cfg cage.Config, identity *guestidentity.Identity) (*Network, error) {
	if identity != nil {
		if err := identity.Validate(); err != nil {
			return nil, err
		}
	}
	gate, err := cage.NewEgressGate(cfg)
	if err != nil {
		return nil, err
	}
	configuration := networkConfiguration(gate, identity)
	stack, err := virtualnetwork.New(&configuration)
	if err != nil {
		gate.Close("容器网络初始化失败")
		return nil, err
	}
	guestMAC, _ := net.ParseMAC(configuration.DHCPStaticLeases[GuestIP])
	return &Network{Gate: gate, stack: stack, guestMAC: guestMAC}, nil
}

// Run consumes the documented four-byte big-endian frame length followed by
// an Ethernet frame. AcceptQemu names the wire format, not a VM dependency.
func (n *Network) Run(ctx context.Context, endpoint net.Conn) error {
	stop := context.AfterFunc(ctx, func() { _ = endpoint.Close() })
	defer stop()
	defer n.Gate.Close("容器网络连接已断开")
	defer endpoint.Close()
	return n.stack.AcceptQemu(ctx, &boundedFrameConnection{Conn: endpoint, sourceMAC: n.guestMAC})
}

// ServeSSH only accepts the listener supplied by the host. It never listens
// inside the virtual gateway, so a guest cannot use this as a reverse proxy.
func (n *Network) ServeSSH(ctx context.Context, listener net.Listener) error {
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.IsLoopback() {
		return errors.New("容器 SSH 入口必须监听本机回环地址")
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var active sync.WaitGroup
	defer func() { cancel(); _ = listener.Close(); active.Wait() }()
	for {
		local, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		active.Add(1)
		go func() {
			defer active.Done()
			defer local.Close()
			cancelLocal := context.AfterFunc(ctx, func() { _ = local.Close() })
			defer cancelLocal()
			dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			remote, err := n.stack.DialContextTCP(dialCtx, GuestIP+":22")
			cancel()
			if err != nil {
				return
			}
			defer remote.Close()
			cancelRemote := context.AfterFunc(ctx, func() { _ = remote.Close() })
			defer cancelRemote()
			done := make(chan struct{})
			go func() { _, _ = io.Copy(remote, local); _ = remote.Close(); _ = local.Close(); close(done) }()
			_, _ = io.Copy(local, remote)
			_ = local.Close()
			_ = remote.Close()
			<-done
		}()
	}
}

// ProcessEndpoint connects an exec -i process to the frame protocol. The
// process must emit only frames on stdout; diagnostics belong to stderr.
// Closing the endpoint terminates this process and releases both pipes.
func ProcessEndpoint(command *exec.Cmd) (net.Conn, func() error, error) {
	input, err := command.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, nil, err
	}
	if err = command.Start(); err != nil {
		input.Close()
		output.Close()
		return nil, nil, err
	}
	endpoint, bridge := net.Pipe()
	var closeOnce sync.Once
	closeProcess := func() {
		closeOnce.Do(func() { _ = input.Close(); _ = output.Close(); _ = bridge.Close(); _ = command.Process.Kill() })
	}
	var copies sync.WaitGroup
	copies.Add(2)
	go func() { defer copies.Done(); defer closeProcess(); _, _ = io.Copy(input, bridge) }()
	go func() { defer copies.Done(); defer closeProcess(); _, _ = io.Copy(bridge, output) }()
	var waitOnce sync.Once
	var waitErr error
	wait := func() error {
		waitOnce.Do(func() { _ = endpoint.Close(); closeProcess(); copies.Wait(); waitErr = command.Wait() })
		return waitErr
	}
	return &processConnection{Conn: endpoint, closeProcess: closeProcess}, wait, nil
}

type processConnection struct {
	net.Conn
	closeProcess func()
}

func (c *processConnection) Close() error { err := c.Conn.Close(); c.closeProcess(); return err }
