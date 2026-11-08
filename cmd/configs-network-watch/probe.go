//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

const (
	icmpv6Protocol    = 58
	icmpReadBufferLen = 1500
	echoIDMask        = 0xffff
	echoSequenceMask  = 0xffff
)

var echoPayload = []byte("configs-network-watch")

// errProbePermission marks a permission error when opening a raw ICMPv6 socket.
var errProbePermission = errors.New("no permission to open a raw ICMPv6 socket")

// errProbeStopped marks a probe that ended because its context ended.
var errProbeStopped = errors.New("ICMPv6 probe stopped")

// echoProber sends one ICMPv6 echo request at a time from an interface.
type echoProber struct {
	interfaceName string
	identifier    int
	sequence      int
}

func newEchoProber(interfaceName string) *echoProber {
	return &echoProber{
		interfaceName: interfaceName,
		identifier:    os.Getpid() & echoIDMask,
		sequence:      0,
	}
}

// probeFailure logs a failed probe step and returns it as an error.
func probeFailure(target netip.Addr, interfaceName string, step string, err error) error {
	wrapped := fmt.Errorf("%s: %w", step, err)
	slog.Warn("network_watch.probe.step_failed",
		"target", target.String(), "interface", interfaceName, "step", step, "err", err)
	return wrapped
}

// openSocket opens a raw ICMPv6 socket bound to the prober interface.
func (prober *echoProber) openSocket(ctx context.Context, target netip.Addr) (net.PacketConn, error) {
	var bindErr error
	listenConfig := net.ListenConfig{
		Control: func(_, _ string, rawConn syscall.RawConn) error {
			controlErr := rawConn.Control(func(descriptor uintptr) {
				bindErr = unix.SetsockoptString(
					int(descriptor), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, prober.interfaceName)
			})
			if controlErr != nil {
				return fmt.Errorf("access ICMPv6 socket: %w", controlErr)
			}
			if bindErr != nil {
				return fmt.Errorf("set SO_BINDTODEVICE: %w", bindErr)
			}
			return nil
		},
	}
	packetConn, err := listenConfig.ListenPacket(ctx, "ip6:ipv6-icmp", "::")
	if bindErr != nil {
		return nil, probeFailure(target, prober.interfaceName, "bind ICMPv6 socket to "+prober.interfaceName, bindErr)
	}
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		slog.Error("network_watch.probe.socket_permission_denied", "interface", prober.interfaceName, "err", err)
		return nil, fmt.Errorf("%w: %w", errProbePermission, err)
	}
	if err != nil {
		return nil, probeFailure(target, prober.interfaceName, "open ICMPv6 socket", err)
	}
	return packetConn, nil
}

// probe sends one echo request to target on the prober interface and waits
// until timeout for the matching reply.
func (prober *echoProber) probe(ctx context.Context, target netip.Addr, timeout time.Duration) error {
	prober.sequence = (prober.sequence + 1) & echoSequenceMask
	sequence := prober.sequence

	packetConn, err := prober.openSocket(ctx, target)
	if err != nil {
		return err
	}
	defer func() {
		_ = packetConn.Close()
	}()
	stopClose := context.AfterFunc(ctx, func() {
		_ = packetConn.Close()
	})
	defer stopClose()

	request := icmp.Message{
		Type: ipv6.ICMPTypeEchoRequest,
		Code: 0,
		Body: &icmp.Echo{ID: prober.identifier, Seq: sequence, Data: echoPayload},
	}
	packet, err := request.Marshal(nil)
	if err != nil {
		return probeFailure(target, prober.interfaceName, "marshal ICMPv6 echo request", err)
	}
	if err := packetConn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return probeFailure(target, prober.interfaceName, "set ICMPv6 deadline", err)
	}
	destination := &net.IPAddr{IP: target.AsSlice(), Zone: ""}
	if target.IsLinkLocalUnicast() {
		destination.Zone = prober.interfaceName
	}
	if _, err := packetConn.WriteTo(packet, destination); err != nil {
		return probeFailure(target, prober.interfaceName, "send ICMPv6 echo request", err)
	}

	buffer := make([]byte, icmpReadBufferLen)
	for {
		length, peer, err := packetConn.ReadFrom(buffer)
		if err != nil {
			if ctx.Err() != nil {
				return errProbeStopped
			}
			return probeFailure(target, prober.interfaceName, "wait for ICMPv6 echo reply", err)
		}
		if isEchoReply(buffer[:length], peer, target, prober.identifier, sequence) {
			return nil
		}
	}
}

// isEchoReply reports whether packet is the echo reply from target for the
// request with identifier and sequence.
func isEchoReply(packet []byte, peer net.Addr, target netip.Addr, identifier int, sequence int) bool {
	peerAddress, ok := peer.(*net.IPAddr)
	if !ok {
		return false
	}
	source, ok := netip.AddrFromSlice(peerAddress.IP)
	if !ok || source.Unmap() != target {
		return false
	}
	message, err := icmp.ParseMessage(icmpv6Protocol, packet)
	if err != nil || message.Type != ipv6.ICMPTypeEchoReply {
		return false
	}
	echo, ok := message.Body.(*icmp.Echo)
	return ok && echo.ID == identifier && echo.Seq == sequence
}
