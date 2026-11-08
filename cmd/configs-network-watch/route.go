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
		Priority: route.config.metric,
		Protocol: unix.RTPROT_STATIC,
		Table:    unix.RT_TABLE_MAIN,
		Family:   netlink.FAMILY_V6,
	}, true, nil
}

func (route trackedRoute) replace() error {
	const operation = "replace"
	request, found, err := route.netlinkRoute(operation)
	if err != nil {
		return err
	}
	if !found {
		return route.routeFailure(operation, errors.New("interface does not exist"))
	}
	if err := netlink.RouteReplace(request); err != nil {
		return route.routeFailure(operation, err)
	}
	return nil
}

// remove deletes only the route that matches the tracked destination,
// gateway, interface, metric, and protocol, and logs the deletion.
func (route trackedRoute) remove() error {
	const operation = "delete"
	request, found, err := route.netlinkRoute(operation)
	if err != nil {
		return err
	}
	if !found {
		return nil
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

// record applies one probe result to the tracked route.
func (tracker *routeTracker) record(probeErr error) error {
	if probeErr == nil {
		tracker.failureCount = 0
		return tracker.route.replace()
	}
	tracker.failureCount++
	slog.Warn("network_watch.route.probe_failed",
		"attempt", tracker.failureCount,
		"threshold", tracker.config.failureThreshold,
		"gateway", tracker.config.gateway.String(),
		"interface", tracker.config.interfaceName,
		"err", probeErr)
	if tracker.failureCount < tracker.config.failureThreshold {
		return nil
	}
	return tracker.route.remove()
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
		if errors.Is(probeErr, errProbeSocket) {
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
