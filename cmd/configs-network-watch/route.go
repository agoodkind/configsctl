//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const probeTimeout = time.Second

// errRouteConflict marks a route change that the tracker refused because
// another route uses the tracked destination and metric.
var errRouteConflict = errors.New("another route uses the tracked destination and metric")

// trackedRoute is the one IPv6 static route the tracker owns.
type trackedRoute struct {
	config routeConfig
}

// routeFailure logs a failed route operation and returns it as an error.
func (route trackedRoute) routeFailure(operation string, err error) error {
	wrapped := fmt.Errorf("%s route %s via %s dev %s metric %d proto static: %w",
		operation, route.config.destination, route.config.gateway, route.config.interfaceName,
		route.config.metric, err)
	slog.Error("network_watch.route.operation_failed",
		"operation", operation,
		"destination", route.config.destination.String(),
		"gateway", route.config.gateway.String(),
		"interface", route.config.interfaceName,
		"metric", route.config.metric,
		"err", err)
	return wrapped
}

// routeConflict logs a refused route change and returns errRouteConflict.
func (route trackedRoute) routeConflict(operation string, foreign []kernelRoute) error {
	for _, other := range foreign {
		slog.Error("network_watch.route.conflict",
			"operation", operation,
			"destination", route.config.destination.String(),
			"metric", route.config.metric,
			"foreign_gateway", other.gateway.String(),
			"foreign_ifindex", other.linkIndex,
			"foreign_protocol", other.protocol,
			"foreign_multipath", other.multipath,
			"foreign_nexthop_object", other.nexthopObject,
			"err", errRouteConflict)
	}
	if len(foreign) == 0 {
		slog.Error("network_watch.route.conflict",
			"operation", operation,
			"destination", route.config.destination.String(),
			"metric", route.config.metric,
			"err", errRouteConflict)
	}
	return fmt.Errorf("%s route %s metric %d: %w",
		operation, route.config.destination, route.config.metric, errRouteConflict)
}

// netlinkRoute builds the route for the current index of the configured
// interface. It returns false when the interface does not exist.
func (route trackedRoute) netlinkRoute(operation string) (*netlink.Route, bool, error) {
	link, err := netlink.LinkByName(route.config.interfaceName)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil, false, nil
		}
		return nil, false, route.routeFailure(operation, err)
	}
	destination := route.config.destination
	return &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst: &net.IPNet{
			IP:   destination.Addr().AsSlice(),
			Mask: net.CIDRMask(destination.Bits(), net.IPv6len*8),
		},
		Gw:       route.config.gateway.AsSlice(),
		Priority: int(route.config.metric),
		Protocol: unix.RTPROT_STATIC,
		Table:    unix.RT_TABLE_MAIN,
		Family:   netlink.FAMILY_V6,
		Type:     unix.RTN_UNICAST,
	}, true, nil
}

// owns reports whether candidate is the tracked route on the interface with
// linkIndex, as an ordinary unicast route without a nexthop object.
func (route trackedRoute) owns(candidate kernelRoute, linkIndex int) bool {
	return candidate.metric == route.config.metric &&
		candidate.protocol == unix.RTPROT_STATIC &&
		candidate.routeType == unix.RTN_UNICAST &&
		!candidate.multipath &&
		!candidate.nexthopObject &&
		candidate.linkIndex == linkIndex &&
		candidate.gateway == route.config.gateway
}

// metricSlot lists the routes to the tracked destination with the tracked
// metric and reports whether the tracked route is among them.
func (route trackedRoute) metricSlot(operation string, linkIndex int) (bool, []kernelRoute, error) {
	routes, err := mainTableRoutes(route.config.destination)
	if err != nil {
		return false, nil, route.routeFailure(operation, err)
	}
	owned := false
	foreign := make([]kernelRoute, 0, len(routes))
	for _, candidate := range routes {
		if candidate.metric != route.config.metric {
			continue
		}
		if route.owns(candidate, linkIndex) {
			owned = true
			continue
		}
		foreign = append(foreign, candidate)
	}
	return owned, foreign, nil
}

// install adds the tracked route if it is absent and its destination and
// metric are available.
func (route trackedRoute) install() error {
	const operation = "install"
	request, found, err := route.netlinkRoute(operation)
	if err != nil {
		return err
	}
	if !found {
		return route.routeFailure(operation, errors.New("interface does not exist"))
	}
	owned, foreign, err := route.metricSlot(operation, request.LinkIndex)
	if err != nil {
		return err
	}
	if owned {
		return nil
	}
	if len(foreign) != 0 {
		return route.routeConflict(operation, foreign)
	}
	if err := netlink.RouteAdd(request); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return route.routeConflict(operation, nil)
		}
		return route.routeFailure(operation, err)
	}
	slog.Info("network_watch.route.installed",
		"destination", route.config.destination.String(),
		"gateway", route.config.gateway.String(),
		"interface", route.config.interfaceName,
		"metric", route.config.metric)
	return nil
}

// remove deletes the tracked route only after a route dump confirms that it
// exists. It refuses when a nexthop object route uses the same destination,
// metric, and protocol, because the kernel deletes such a route without
// comparing its gateway or interface.
func (route trackedRoute) remove() error {
	const operation = "delete"
	request, found, err := route.netlinkRoute(operation)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	owned, foreign, err := route.metricSlot(operation, request.LinkIndex)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	blocking := make([]kernelRoute, 0, len(foreign))
	for _, other := range foreign {
		if other.nexthopObject && other.protocol == unix.RTPROT_STATIC {
			blocking = append(blocking, other)
		}
	}
	if len(blocking) != 0 {
		return route.routeConflict(operation, blocking)
	}
	if err := netlink.RouteDel(request); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		return route.routeFailure(operation, err)
	}
	slog.Info("network_watch.route.removed",
		"destination", route.config.destination.String(),
		"gateway", route.config.gateway.String(),
		"interface", route.config.interfaceName,
		"metric", route.config.metric)
	return nil
}

// routeTracker probes the gateway and installs or removes the tracked route.
type routeTracker struct {
	config       routeConfig
	route        trackedRoute
	prober       *echoProber
	failureCount int
}

// record updates the tracked route for one probe result and ignores conflicts.
func (tracker *routeTracker) record(probeErr error) error {
	var changeErr error
	if probeErr == nil {
		tracker.failureCount = 0
		changeErr = tracker.route.install()
	} else {
		tracker.failureCount++
		slog.Warn("network_watch.route.probe_failed",
			"attempt", tracker.failureCount,
			"threshold", tracker.config.failureThreshold,
			"gateway", tracker.config.gateway.String(),
			"interface", tracker.config.interfaceName,
			"err", probeErr)
		if tracker.failureCount >= tracker.config.failureThreshold {
			changeErr = tracker.route.remove()
		}
	}
	if errors.Is(changeErr, errRouteConflict) {
		return nil
	}
	return changeErr
}

// runRouteTracker probes the gateway until ctx ends or a route change fails.
// It removes the tracked route before it returns.
func runRouteTracker(ctx context.Context, config routeConfig) (err error) {
	tracker := &routeTracker{
		config:       config,
		route:        trackedRoute{config: config},
		prober:       newEchoProber(config.interfaceName),
		failureCount: 0,
	}
	defer func() {
		if cleanupErr := tracker.route.remove(); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()

	for {
		probeErr := tracker.prober.probe(ctx, config.gateway, probeTimeout)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(probeErr, errProbePermission) {
			return probeErr
		}
		if err := tracker.record(probeErr); err != nil {
			return err
		}
		timer := time.NewTimer(config.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
