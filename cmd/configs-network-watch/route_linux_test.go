//go:build linux

package main_test

import (
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	trackerLink        = "trk0"
	gatewayLink        = "gw0"
	trackerAddressCIDR = "fd7a::1/64"
	gatewayAddressCIDR = "fd7a::2/64"
	gatewayAddress     = "fd7a::2"
	otherGateway       = "fd7a::3"
	trackedDestination = "fd7b::/64"
	trackedMetric      = 77
	otherMetric        = 200

	routeChangeWait = 10 * time.Second
	failureLogWait  = 20 * time.Second
	exitUsage       = 2
	exitInterrupt   = 130
)

// routeLab is a tracker namespace and a gateway namespace joined by a veth
// pair.
type routeLab struct {
	trackerNamespace netns.NsHandle
	tracker          *netlink.Handle
	gateway          *netlink.Handle
}

func newRouteLab(t *testing.T) routeLab {
	t.Helper()
	trackerNamespace := newNamespace(t, "")
	gatewayNamespace := newNamespace(t, "")
	lab := routeLab{
		trackerNamespace: trackerNamespace,
		tracker:          namespaceHandle(t, trackerNamespace),
		gateway:          namespaceHandle(t, gatewayNamespace),
	}
	veth := &netlink.Veth{
		LinkAttrs:     netlink.LinkAttrs{Name: trackerLink},
		PeerName:      gatewayLink,
		PeerNamespace: netlink.NsFd(int(gatewayNamespace)),
	}
	if err := lab.tracker.LinkAdd(veth); err != nil {
		t.Fatalf("add veth pair: %v", err)
	}
	setLinkUp(t, lab.tracker, trackerLink)
	setLinkUp(t, lab.gateway, gatewayLink)
	addAddress(t, lab.tracker, trackerLink, trackerAddressCIDR)
	addAddress(t, lab.gateway, gatewayLink, gatewayAddressCIDR)
	otherRoute := lab.route(t, otherGateway, otherMetric, unix.RTPROT_BOOT)
	if err := lab.tracker.RouteAdd(otherRoute); err != nil {
		t.Fatalf("add the route that the tracker does not own: %v", err)
	}
	return lab
}

func (lab routeLab) route(t *testing.T, gateway string, metric int, protocol netlink.RouteProtocol) *netlink.Route {
	t.Helper()
	_, destination, err := net.ParseCIDR(trackedDestination)
	if err != nil {
		t.Fatalf("parse %s: %v", trackedDestination, err)
	}
	return &netlink.Route{
		LinkIndex: linkByName(t, lab.tracker, trackerLink).Attrs().Index,
		Dst:       destination,
		Gw:        net.ParseIP(gateway),
		Priority:  metric,
		Protocol:  protocol,
		Table:     unix.RT_TABLE_MAIN,
	}
}

// hasRoute reports whether the tracker namespace has a route that matches
// want on destination, gateway, interface, metric, and protocol.
func (lab routeLab) hasRoute(t *testing.T, want *netlink.Route) bool {
	t.Helper()
	routes, err := lab.tracker.RouteListFiltered(netlink.FAMILY_V6, want, netlink.RT_FILTER_DST|netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	for _, route := range routes {
		if route.Gw.Equal(want.Gw) && route.LinkIndex == want.LinkIndex &&
			route.Priority == want.Priority && route.Protocol == want.Protocol {
			return true
		}
	}
	return false
}

func (lab routeLab) startTracker(t *testing.T) *commandProcess {
	t.Helper()
	return startCommand(t, lab.trackerNamespace, commandBinary(t), "route",
		"--destination", trackedDestination,
		"--gateway", gatewayAddress,
		"--interface", trackerLink,
		"--interval-seconds", "2",
		"--failure-threshold", "3",
		"--metric", "77")
}

func TestRouteTrackerFollowsGatewayReachability(t *testing.T) {
	requireRoot(t)
	lab := newRouteLab(t)
	tracked := lab.route(t, gatewayAddress, trackedMetric, unix.RTPROT_STATIC)
	other := lab.route(t, otherGateway, otherMetric, unix.RTPROT_BOOT)

	process := lab.startTracker(t)
	waitFor(t, routeChangeWait, "the tracked route after a successful probe", func() bool {
		return lab.hasRoute(t, tracked)
	})

	deleteAddress(t, lab.gateway, gatewayLink, gatewayAddressCIDR)
	process.waitForLine(t, failureLogWait, "msg=network_watch.route.probe_failed attempt=2")
	if !lab.hasRoute(t, tracked) {
		t.Fatal("the tracker removed the route after two failed probes, want removal after three")
	}
	process.waitForLine(t, failureLogWait, "msg=network_watch.route.probe_failed attempt=3")
	waitFor(t, routeChangeWait, "route removal after three failed probes", func() bool {
		return !lab.hasRoute(t, tracked)
	})
	if !lab.hasRoute(t, other) {
		t.Fatal("the tracker removed a route with a different gateway, metric, and protocol")
	}

	addAddress(t, lab.gateway, gatewayLink, gatewayAddressCIDR)
	waitFor(t, routeChangeWait, "the tracked route after the gateway recovers", func() bool {
		return lab.hasRoute(t, tracked)
	})

	if status := process.stop(t, syscall.SIGTERM); status != 0 {
		t.Fatalf("exit status after SIGTERM = %d, want 0", status)
	}
	if lab.hasRoute(t, tracked) {
		t.Fatal("the tracked route remains after SIGTERM")
	}
	if !lab.hasRoute(t, other) {
		t.Fatal("the route that the tracker does not own is missing after SIGTERM")
	}
}

func TestRouteTrackerRemovesRouteOnInterrupt(t *testing.T) {
	requireRoot(t)
	lab := newRouteLab(t)
	tracked := lab.route(t, gatewayAddress, trackedMetric, unix.RTPROT_STATIC)

	process := lab.startTracker(t)
	waitFor(t, routeChangeWait, "the tracked route after a successful probe", func() bool {
		return lab.hasRoute(t, tracked)
	})
	if status := process.stop(t, syscall.SIGINT); status != exitInterrupt {
		t.Fatalf("exit status after SIGINT = %d, want %d", status, exitInterrupt)
	}
	if lab.hasRoute(t, tracked) {
		t.Fatal("the tracked route remains after SIGINT")
	}
	if !strings.Contains(process.capturedOutput(), "msg=network_watch.route.removed") {
		t.Fatalf("output does not report the route removal:\n%s", process.capturedOutput())
	}
}
