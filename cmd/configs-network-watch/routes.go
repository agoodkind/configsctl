//go:build linux

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

const (
	// rtaNexthopID is RTA_NH_ID from linux/rtnetlink.h, which x/sys/unix
	// does not define.
	rtaNexthopID      = 30
	routeDumpAttempts = 3
)

// kernelRoute is one IPv6 main table route from a netlink route dump.
type kernelRoute struct {
	gateway       netip.Addr
	linkIndex     int
	metric        uint32
	protocol      uint8
	routeType     uint8
	multipath     bool
	nexthopObject bool
}

// mainTableRoutes returns IPv6 main table routes for destination and retries
// interrupted dumps up to the total limit in routeDumpAttempts.
func mainTableRoutes(destination netip.Prefix) ([]kernelRoute, error) {
	var lastErr error
	for range routeDumpAttempts {
		routes, err := dumpMainTableRoutes(destination)
		if !errors.Is(err, nl.ErrDumpInterrupted) {
			return routes, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func dumpMainTableRoutes(destination netip.Prefix) ([]kernelRoute, error) {
	request := nl.NewNetlinkRequest(unix.RTM_GETROUTE, unix.NLM_F_DUMP)
	header := nl.NewRtMsg()
	header.Family = unix.AF_INET6
	request.AddData(header)
	messages, err := request.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWROUTE)
	if err != nil {
		slog.Error("network_watch.route.dump_failed", "destination", destination.String(), "err", err)
		return nil, fmt.Errorf("list IPv6 routes for %s: %w", destination, err)
	}
	routes := make([]kernelRoute, 0, len(messages))
	for _, message := range messages {
		route, matches, err := parseRouteMessage(message, destination)
		if err != nil {
			return nil, err
		}
		if matches {
			routes = append(routes, route)
		}
	}
	return routes, nil
}

// parseRouteMessage decodes one route message and reports whether it is a
// main table route for destination.
func parseRouteMessage(message []byte, destination netip.Prefix) (kernelRoute, bool, error) {
	var route kernelRoute
	if len(message) < unix.SizeofRtMsg {
		return route, false, nil
	}
	header := nl.DeserializeRtMsg(message)
	if header.Family != unix.AF_INET6 || int(header.Dst_len) != destination.Bits() {
		return route, false, nil
	}
	attributes, err := nl.ParseRouteAttr(message[header.Len():])
	if err != nil {
		slog.Error("network_watch.route.parse_failed", "destination", destination.String(), "err", err)
		return route, false, fmt.Errorf("parse IPv6 route attributes: %w", err)
	}
	route.protocol = header.Protocol
	route.routeType = header.Type
	table := uint32(header.Table)
	address := netip.IPv6Unspecified()
	for _, attribute := range attributes {
		switch attribute.Attr.Type {
		case unix.RTA_DST:
			if parsed, ok := netip.AddrFromSlice(attribute.Value); ok {
				address = parsed
			}
		case unix.RTA_TABLE:
			table = nl.NativeEndian().Uint32(attribute.Value)
		case unix.RTA_PRIORITY:
			route.metric = nl.NativeEndian().Uint32(attribute.Value)
		case unix.RTA_GATEWAY:
			if parsed, ok := netip.AddrFromSlice(attribute.Value); ok {
				route.gateway = parsed
			}
		case unix.RTA_OIF:
			route.linkIndex = int(nl.NativeEndian().Uint32(attribute.Value))
		case unix.RTA_MULTIPATH:
			route.multipath = true
		case rtaNexthopID:
			route.nexthopObject = true
		}
	}
	if table != unix.RT_TABLE_MAIN || netip.PrefixFrom(address, destination.Bits()) != destination {
		return route, false, nil
	}
	return route, true, nil
}
