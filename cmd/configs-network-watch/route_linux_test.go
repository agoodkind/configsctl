//go:build linux

package main_test

import (
	"net"
	"os/exec"
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
	trackedMetric      = 100
	trackedMetricText  = "100"
	otherMetric        = 200
	foreignNexthopID   = "77"

	routeChangeWait = 10 * time.Second
	failureLogWait  = 20 * time.Second
	exitFailure     = 1
	exitUsage       = 2
	exitInterrupt   = 130

	installConflictLine = "msg=network_watch.route.conflict operation=install"
	deleteConflictLine  = "msg=network_watch.route.conflict operation=delete"
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
		"--metric", trackedMetricText)
}

// runIP runs the iproute2 ip command in the tracker namespace and returns
// its output.
func (lab routeLab) runIP(t *testing.T, args ...string) string {
	t.Helper()
	ipTool, err := exec.LookPath("ip")
	if err != nil {
		t.Fatalf("find the iproute2 ip command: %v", err)
	}
	var output []byte
	runErr := inNamespace(lab.trackerNamespace, func() error {
		command := exec.Command(ipTool, args...)
		combined, err := command.CombinedOutput()
		output = combined
		return err
	})
	if runErr != nil {
		t.Fatalf("ip %s: %v\n%s", strings.Join(args, " "), runErr, output)
	}
	return string(output)
}

// hasNexthopRoute reports whether the tracker namespace has the tracked
// destination through the foreign nexthop object.
func (lab routeLab) hasNexthopRoute(t *testing.T) bool {
	t.Helper()
	output := lab.runIP(t, "-6", "-o", "route", "show", trackedDestination)
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "nhid "+foreignNexthopID+" ") {
			return true
		}
	}
	return false
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

func TestRouteTrackerPreservesForeignRouteWithSameMetric(t *testing.T) {
	requireRoot(t)
	lab := newRouteLab(t)
	tracked := lab.route(t, gatewayAddress, trackedMetric, unix.RTPROT_STATIC)
	foreign := lab.route(t, otherGateway, trackedMetric, unix.RTPROT_BOOT)
	if err := lab.tracker.RouteAdd(foreign); err != nil {
		t.Fatalf("add the foreign route with the tracked metric: %v", err)
	}
	requireForeign := func(stage string) {
		t.Helper()
		if !lab.hasRoute(t, foreign) {
			t.Fatalf("the foreign route with the tracked metric is missing after %s", stage)
		}
		if lab.hasRoute(t, tracked) {
			t.Fatalf("the tracker installed its route beside the foreign route after %s", stage)
		}
	}

	process := lab.startTracker(t)
	process.waitForLine(t, routeChangeWait, installConflictLine)
	requireForeign("the first successful probe")
	process.waitForLine(t, routeChangeWait, installConflictLine)
	requireForeign("a repeated successful probe")

	deleteAddress(t, lab.gateway, gatewayLink, gatewayAddressCIDR)
	process.waitForLine(t, failureLogWait, "msg=network_watch.route.probe_failed attempt=3")
	process.waitForLine(t, failureLogWait, "msg=network_watch.route.probe_failed attempt=4")
	requireForeign("failed probes past the threshold")

	if status := process.stop(t, syscall.SIGTERM); status != 0 {
		t.Fatalf("exit status after SIGTERM = %d, want 0", status)
	}
	requireForeign("exit")
}

func TestRouteTrackerPreservesForeignNexthopRoute(t *testing.T) {
	requireRoot(t)
	lab := newRouteLab(t)
	tracked := lab.route(t, gatewayAddress, trackedMetric, unix.RTPROT_STATIC)

	process := lab.startTracker(t)
	waitFor(t, routeChangeWait, "the tracked route after a successful probe", func() bool {
		return lab.hasRoute(t, tracked)
	})
	lab.runIP(t, "-6", "nexthop", "add", "id", foreignNexthopID, "via", otherGateway, "dev", trackerLink)
	lab.runIP(t, "-6", "route", "append", trackedDestination,
		"nhid", foreignNexthopID, "metric", trackedMetricText, "proto", "static")
	if !lab.hasNexthopRoute(t) {
		t.Fatal("the foreign nexthop object route is missing after setup")
	}

	deleteAddress(t, lab.gateway, gatewayLink, gatewayAddressCIDR)
	process.waitForLine(t, failureLogWait, deleteConflictLine)
	if !lab.hasNexthopRoute(t) {
		t.Fatal("the tracker deleted the foreign nexthop object route after failed probes")
	}

	if status := process.stop(t, syscall.SIGTERM); status != exitFailure {
		t.Fatalf("exit status after a refused deletion = %d, want %d", status, exitFailure)
	}
	if !lab.hasNexthopRoute(t) {
		t.Fatal("the tracker deleted the foreign nexthop object route on exit")
	}
}

func TestRouteRejectsUnrepresentableNumbers(t *testing.T) {
	cases := []struct {
		name     string
		flag     string
		value    string
		response string
	}{
		{name: "metric above 32 bits", flag: "--metric", value: "4294967296",
			response: "--metric must be an integer from 1 through 4294967295"},
		{name: "zero metric", flag: "--metric", value: "0",
			response: "--metric must be an integer from 1 through 4294967295"},
		{name: "interval above the duration range", flag: "--interval-seconds", value: "9223372037",
			response: "--interval-seconds must be an integer from 1 through 9223372036"},
		{name: "zero threshold", flag: "--failure-threshold", value: "0",
			response: "--failure-threshold must be an integer from 1 through"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			values := map[string]string{
				"--destination":       trackedDestination,
				"--gateway":           gatewayAddress,
				"--interface":         trackerLink,
				"--interval-seconds":  "2",
				"--failure-threshold": "3",
				"--metric":            trackedMetricText,
			}
			values[testCase.flag] = testCase.value
			args := []string{"route"}
			for _, flagName := range []string{
				"--destination", "--gateway", "--interface", "--interval-seconds", "--failure-threshold", "--metric",
			} {
				args = append(args, flagName, values[flagName])
			}
			process := startCommand(t, netns.None(), commandBinary(t), args...)
			if status := process.wait(t); status != exitUsage {
				t.Fatalf("exit status = %d, want %d", status, exitUsage)
			}
			if output := process.capturedOutput(); !strings.Contains(output, testCase.response) {
				t.Fatalf("output does not contain %q:\n%s", testCase.response, output)
			}
		})
	}
}
