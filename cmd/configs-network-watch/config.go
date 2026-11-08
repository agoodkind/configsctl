//go:build linux

package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const serviceUnitSuffix = ".service"

// usageError is an invalid command line.
type usageError string

func (message usageError) Error() string {
	return string(message)
}

// routeConfig is the validated input for the route subcommand.
type routeConfig struct {
	destination      netip.Prefix
	gateway          netip.Addr
	interfaceName    string
	interval         time.Duration
	failureThreshold int
	metric           uint32
}

const (
	maxIntervalSeconds = math.MaxInt64 / int64(time.Second)
	decimalBase        = 10
	intervalBits       = 64
	metricBits         = 32
)

// rangeError reports a numeric flag value outside 1 through maximum.
func rangeError(flagName string, maximum uint64) error {
	return usageError(fmt.Sprintf("--%s must be an integer from 1 through %d", flagName, maximum))
}

// parseInterval parses the --interval-seconds value into a duration that
// [time.Duration] can represent.
func parseInterval(text string) (time.Duration, error) {
	seconds, err := strconv.ParseInt(text, decimalBase, intervalBits)
	if err != nil || seconds < 1 || seconds > maxIntervalSeconds {
		return 0, rangeError("interval-seconds", uint64(maxIntervalSeconds))
	}
	return time.Duration(seconds) * time.Second, nil
}

// parseThreshold parses the --failure-threshold value.
func parseThreshold(text string) (int, error) {
	threshold, err := strconv.Atoi(text)
	if err != nil || threshold < 1 {
		return 0, rangeError("failure-threshold", math.MaxInt)
	}
	return threshold, nil
}

// parseMetric parses the --metric value into the 32-bit metric that the
// kernel stores. It rejects 0, which a route deletion treats as any metric.
func parseMetric(text string) (uint32, error) {
	metric, err := strconv.ParseUint(text, decimalBase, metricBits)
	if err != nil || metric < 1 {
		return 0, rangeError("metric", math.MaxUint32)
	}
	return uint32(metric), nil
}

// interfaceConfig is the validated input for the interface subcommand.
type interfaceConfig struct {
	interfaceName string
	dhcp4Unit     string
	dhcp6Unit     string
	routeUnit     string
}

// parseFlags parses args into flags and rejects positional arguments and
// omitted required flags.
func parseFlags(flags *flag.FlagSet, args []string, required []string) error {
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if flags.NArg() != 0 {
		return usageError(fmt.Sprintf("unexpected argument %q", flags.Arg(0)))
	}
	provided := make(map[string]bool, len(required))
	flags.Visit(func(set *flag.Flag) {
		provided[set.Name] = true
	})
	missing := make([]string, 0, len(required))
	for _, name := range required {
		if !provided[name] {
			missing = append(missing, "--"+name)
		}
	}
	if len(missing) != 0 {
		return usageError("missing required flags " + strings.Join(missing, ", "))
	}
	return nil
}

func parseRouteConfig(args []string) (routeConfig, error) {
	flags := flag.NewFlagSet(string(routeSubcommand), flag.ContinueOnError)
	destinationText := flags.String("destination", "", "destination IPv6 prefix of the tracked route")
	gatewayText := flags.String("gateway", "", "gateway IPv6 address to probe")
	interfaceName := flags.String("interface", "", "interface for the probes and the route")
	intervalText := flags.String("interval-seconds", "", "seconds to wait after each probe")
	thresholdText := flags.String("failure-threshold", "", "consecutive failed probes that remove the route")
	metricText := flags.String("metric", "", "route metric")
	required := []string{
		"destination", "gateway", "interface", "interval-seconds", "failure-threshold", "metric",
	}
	if err := parseFlags(flags, args, required); err != nil {
		return routeConfig{}, err
	}

	destination, err := netip.ParsePrefix(*destinationText)
	if err != nil {
		return routeConfig{}, usageError("--destination: " + err.Error())
	}
	if !destination.Addr().Is6() || destination.Addr().Is4In6() {
		return routeConfig{}, usageError(fmt.Sprintf("--destination %s is not an IPv6 prefix", destination))
	}
	if destination.Masked() != destination {
		return routeConfig{}, usageError(fmt.Sprintf(
			"--destination %s has host bits set; use %s", destination, destination.Masked()))
	}
	gateway, err := netip.ParseAddr(*gatewayText)
	if err != nil {
		return routeConfig{}, usageError("--gateway: " + err.Error())
	}
	if !gateway.Is6() || gateway.Is4In6() || gateway.Zone() != "" {
		return routeConfig{}, usageError(fmt.Sprintf(
			"--gateway %s is not an IPv6 address without a zone", gateway))
	}
	if *interfaceName == "" {
		return routeConfig{}, usageError("--interface is empty")
	}
	interval, err := parseInterval(*intervalText)
	if err != nil {
		return routeConfig{}, err
	}
	failureThreshold, err := parseThreshold(*thresholdText)
	if err != nil {
		return routeConfig{}, err
	}
	metric, err := parseMetric(*metricText)
	if err != nil {
		return routeConfig{}, err
	}
	return routeConfig{
		destination:      destination,
		gateway:          gateway,
		interfaceName:    *interfaceName,
		interval:         interval,
		failureThreshold: failureThreshold,
		metric:           metric,
	}, nil
}

func parseInterfaceConfig(args []string) (interfaceConfig, error) {
	flags := flag.NewFlagSet(string(interfaceSubcommand), flag.ContinueOnError)
	interfaceName := flags.String("interface", "", "interface the units bind to")
	dhcp4Unit := flags.String("dhcp4-unit", "", "service unit for Kea DHCPv4")
	dhcp6Unit := flags.String("dhcp6-unit", "", "service unit for Kea DHCPv6")
	routeUnit := flags.String("route-unit", "", "service unit to restart when the interface index changes")
	required := []string{"interface", "dhcp4-unit", "dhcp6-unit", "route-unit"}
	if err := parseFlags(flags, args, required); err != nil {
		return interfaceConfig{}, err
	}

	if *interfaceName == "" {
		return interfaceConfig{}, usageError("--interface is empty")
	}
	units := []struct {
		flagName string
		unit     string
	}{
		{flagName: "dhcp4-unit", unit: *dhcp4Unit},
		{flagName: "dhcp6-unit", unit: *dhcp6Unit},
		{flagName: "route-unit", unit: *routeUnit},
	}
	for _, entry := range units {
		if !strings.HasSuffix(entry.unit, serviceUnitSuffix) || entry.unit == serviceUnitSuffix {
			return interfaceConfig{}, usageError(fmt.Sprintf(
				"--%s %q is not a %s unit name", entry.flagName, entry.unit, serviceUnitSuffix))
		}
	}
	return interfaceConfig{
		interfaceName: *interfaceName,
		dhcp4Unit:     *dhcp4Unit,
		dhcp6Unit:     *dhcp6Unit,
		routeUnit:     *routeUnit,
	}, nil
}
