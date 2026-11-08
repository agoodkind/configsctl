//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	pollInterval       = 2 * time.Second
	missingSocketPolls = 15
	dhcp4ServerPort    = 67
	dhcp6ServerPort    = 547
)

// listeningUDPStates lists the socket states that `ss -u -l` prints.
var listeningUDPStates = []uint8{netlink.TCP_CLOSE, netlink.TCP_LISTEN}

// inspectionFailure logs a failed interface or socket inspection and returns
// it as an error.
func inspectionFailure(subject string, operation string, err error) error {
	wrapped := fmt.Errorf("%s %s: %w", operation, subject, err)
	slog.Warn("network_watch.interface.inspection_failed", "subject", subject, "operation", operation, "err", err)
	return wrapped
}

// lookupLink returns the watched interface, or false when it does not exist.
func lookupLink(name string) (netlink.Link, bool, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil, false, nil
		}
		return nil, false, inspectionFailure(name, "look up interface", err)
	}
	return link, true, nil
}

// socketProbe reports whether a Kea unit has its UDP socket on link.
type socketProbe func(link netlink.Link) (bool, error)

// keaUnit is one Kea service and its missing socket count.
type keaUnit struct {
	name         string
	port         int
	probe        socketProbe
	missingPolls int
}

// interfaceWatcher restarts the interface units after the interface index
// changes and restarts a Kea unit after its socket stays missing.
type interfaceWatcher struct {
	config    interfaceConfig
	units     *unitManager
	kea       []*keaUnit
	lastIndex int
}

func runInterfaceWatcher(ctx context.Context, config interfaceConfig) error {
	units := &unitManager{connection: nil}
	defer units.close()
	watcher := &interfaceWatcher{
		config: config,
		units:  units,
		kea: []*keaUnit{
			{name: config.dhcp4Unit, port: dhcp4ServerPort, probe: dhcp4SocketPresent, missingPolls: 0},
			{name: config.dhcp6Unit, port: dhcp6ServerPort, probe: dhcp6SocketPresent, missingPolls: 0},
		},
		lastIndex: 0,
	}
	watcher.lastIndex = watcher.currentIndex()
	slog.Info("network_watch.interface.watching", "interface", config.interfaceName, "ifindex", watcher.lastIndex)

	for {
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		watcher.poll(ctx)
	}
}

// currentIndex returns the interface index, or 0 when the interface does
// not exist or the lookup fails.
func (watcher *interfaceWatcher) currentIndex() int {
	link, found, err := lookupLink(watcher.config.interfaceName)
	if err != nil || !found {
		return 0
	}
	return link.Attrs().Index
}

func (watcher *interfaceWatcher) poll(ctx context.Context) {
	index := watcher.currentIndex()
	if index != 0 && index != watcher.lastIndex {
		slog.Info("network_watch.interface.index_changed",
			"interface", watcher.config.interfaceName,
			"previous_ifindex", watcher.lastIndex,
			"ifindex", index)
		watcher.lastIndex = index
		watcher.restartInterfaceUnits(ctx)
	}
	for _, unit := range watcher.kea {
		watcher.checkSocket(ctx, unit)
	}
}

func (watcher *interfaceWatcher) restartInterfaceUnits(ctx context.Context) {
	for _, unit := range watcher.kea {
		watcher.restart(ctx, unit.name, restartIfRunning)
	}
	watcher.restart(ctx, watcher.config.routeUnit, restartAlways)
}

func (watcher *interfaceWatcher) restart(ctx context.Context, unit string, verb restartVerb) {
	if err := watcher.units.restart(ctx, unit, verb); err != nil {
		slog.Error("network_watch.interface.restart_failed", "unit", unit, "verb", string(verb), "err", err)
	}
}

// checkSocket counts a missing socket only while the unit is active and the
// interface is up. It keeps the count unchanged when a check fails.
func (watcher *interfaceWatcher) checkSocket(ctx context.Context, unit *keaUnit) {
	active, err := watcher.units.active(ctx, unit.name)
	if err != nil {
		return
	}
	if !active {
		unit.missingPolls = 0
		return
	}
	link, found, err := lookupLink(watcher.config.interfaceName)
	if err != nil {
		return
	}
	if !found || link.Attrs().OperState != netlink.OperUp {
		unit.missingPolls = 0
		return
	}
	present, err := unit.probe(link)
	if err != nil {
		return
	}
	if present {
		unit.missingPolls = 0
		return
	}
	unit.missingPolls++
	slog.Info("network_watch.interface.socket_missing_poll",
		"unit", unit.name, "port", unit.port, "polls", unit.missingPolls)
	if unit.missingPolls < missingSocketPolls {
		return
	}
	slog.Warn("network_watch.interface.socket_missing",
		"unit", unit.name,
		"port", unit.port,
		"interface", watcher.config.interfaceName,
		"polls", unit.missingPolls)
	unit.missingPolls = 0
	watcher.restart(ctx, unit.name, restartIfRunning)
}

// listeningUDPSockets returns the listening UDP sockets of family on port.
// An interrupted dump returns an error because its socket list can be
// incomplete.
func listeningUDPSockets(family uint8, port int) ([]*netlink.Socket, error) {
	sockets, err := netlink.SocketDiagUDP(family)
	if err != nil {
		return nil, inspectionFailure(fmt.Sprintf("UDP port %d", port), "list sockets on", err)
	}
	matches := make([]*netlink.Socket, 0, len(sockets))
	for _, socket := range sockets {
		if int(socket.ID.SourcePort) != port {
			continue
		}
		if !slices.Contains(listeningUDPStates, socket.State) {
			continue
		}
		matches = append(matches, socket)
	}
	return matches, nil
}

// dhcp4SocketPresent reports whether a UDP 67 socket listens on an IPv4
// address of link.
func dhcp4SocketPresent(link netlink.Link) (bool, error) {
	sockets, err := listeningUDPSockets(unix.AF_INET, dhcp4ServerPort)
	if err != nil {
		return false, err
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return false, inspectionFailure(link.Attrs().Name, "list IPv4 addresses of", err)
	}
	for _, socket := range sockets {
		source, ok := netip.AddrFromSlice(socket.ID.Source)
		if !ok {
			continue
		}
		for _, address := range addresses {
			interfaceAddress, ok := netip.AddrFromSlice(address.IP)
			if ok && interfaceAddress.Unmap() == source.Unmap() {
				return true, nil
			}
		}
	}
	return false, nil
}

// dhcp6SocketPresent reports whether a UDP 547 socket is bound to the index
// of link.
func dhcp6SocketPresent(link netlink.Link) (bool, error) {
	sockets, err := listeningUDPSockets(unix.AF_INET6, dhcp6ServerPort)
	if err != nil {
		return false, err
	}
	for _, socket := range sockets {
		if int(socket.ID.Interface) == link.Attrs().Index {
			return true, nil
		}
	}
	return false, nil
}
