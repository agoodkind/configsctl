//go:build linux

package main_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	systemddbus "github.com/coreos/go-systemd/v22/dbus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	watchedLink        = "wat0"
	watchedPeer        = "wat0p"
	watchedAddressCIDR = "10.231.0.1/24"
	dhcp4ListenAddress = "10.231.0.1:67"
	dhcp6ListenAddress = "[::]:547"

	runtimeUnitDirectory = "/run/systemd/system"
	helperTemplatePath   = "testdata/helper.service.tmpl"
	oneshotTemplatePath  = "testdata/oneshot.service.tmpl"
	jobResultDone        = "done"
	activeState          = "active"
	inactiveState        = "inactive"

	watcherStartWait  = 10 * time.Second
	unitChangeWait    = 15 * time.Second
	unchangedPolls    = 6 * time.Second
	missingSocketWait = 45 * time.Second
	beforeMissingWait = 20 * time.Second
)

// unitTemplate is the input to the test unit templates.
type unitTemplate struct {
	Mode              string
	Address           string
	Interface         string
	NamespacePath     string
	Executable        string
	ModeVariable      string
	AddressVariable   string
	InterfaceVariable string
}

// interfaceLab is a named namespace with the watched veth pair and three
// temporary systemd units.
type interfaceLab struct {
	namespace     netns.NsHandle
	namespacePath string
	handle        *netlink.Handle
	systemd       *systemddbus.Conn
	dhcp4Unit     string
	dhcp6Unit     string
	routeUnit     string
}

func requireSystemd(t *testing.T) {
	t.Helper()
	info, err := os.Stat(runtimeUnitDirectory)
	if err != nil || !info.IsDir() {
		t.Skip("this test requires a host that booted with systemd")
	}
}

func newInterfaceLab(t *testing.T, dhcp6Mode string) *interfaceLab {
	t.Helper()
	requireRoot(t)
	requireSystemd(t)
	name := fmt.Sprintf("cnw-test-%d", os.Getpid())
	namespace := newNamespace(t, name)
	systemd, err := systemddbus.NewSystemdConnectionContext(context.Background())
	if err != nil {
		t.Fatalf("connect to systemd: %v", err)
	}
	t.Cleanup(systemd.Close)
	prefix := fmt.Sprintf("configs-network-watch-test-%d-", os.Getpid())
	lab := &interfaceLab{
		namespace:     namespace,
		namespacePath: filepath.Join("/run/netns", name),
		handle:        namespaceHandle(t, namespace),
		systemd:       systemd,
		dhcp4Unit:     prefix + "dhcp4.service",
		dhcp6Unit:     prefix + "dhcp6.service",
		routeUnit:     prefix + "route.service",
	}
	lab.createWatchedLink(t, 0)

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test executable: %v", err)
	}
	lab.writeUnit(t, lab.dhcp4Unit, helperTemplatePath, lab.unitInput(executable, helperUDP4, dhcp4ListenAddress))
	lab.writeUnit(t, lab.dhcp6Unit, helperTemplatePath, lab.unitInput(executable, dhcp6Mode, dhcp6ListenAddress))
	lab.writeUnit(t, lab.routeUnit, oneshotTemplatePath, lab.unitInput(executable, helperExit, ""))
	if err := systemd.ReloadContext(context.Background()); err != nil {
		t.Fatalf("reload systemd: %v", err)
	}
	for _, unit := range []string{lab.dhcp4Unit, lab.dhcp6Unit, lab.routeUnit} {
		lab.runJob(t, unit, systemd.StartUnitContext)
	}
	return lab
}

func (lab *interfaceLab) unitInput(executable string, mode string, address string) unitTemplate {
	return unitTemplate{
		Mode:              mode,
		Address:           address,
		Interface:         watchedLink,
		NamespacePath:     lab.namespacePath,
		Executable:        executable,
		ModeVariable:      helperModeVariable,
		AddressVariable:   helperAddressVariable,
		InterfaceVariable: helperInterfaceVariable,
	}
}

func (lab *interfaceLab) writeUnit(t *testing.T, unit string, templatePath string, input unitTemplate) {
	t.Helper()
	source, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatalf("read %s: %v", templatePath, err)
	}
	parsed, err := template.New(unit).Option("missingkey=error").Parse(string(source))
	if err != nil {
		t.Fatalf("parse %s: %v", templatePath, err)
	}
	var rendered bytes.Buffer
	if err := parsed.Execute(&rendered, input); err != nil {
		t.Fatalf("render %s: %v", templatePath, err)
	}
	path := filepath.Join(runtimeUnitDirectory, unit)
	if err := os.WriteFile(path, rendered.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Cleanup(func() {
		lab.runJob(t, unit, lab.systemd.StopUnitContext)
		if err := os.Remove(path); err != nil {
			t.Errorf("remove %s: %v", path, err)
		}
		if err := lab.systemd.ReloadContext(context.Background()); err != nil {
			t.Errorf("reload systemd: %v", err)
		}
	})
}

// systemdJob is the signature of the go-systemd start and stop calls.
type systemdJob func(ctx context.Context, name string, mode string, results chan<- string) (int, error)

func (lab *interfaceLab) runJob(t *testing.T, unit string, job systemdJob) {
	t.Helper()
	results := make(chan string, 1)
	if _, err := job(context.Background(), unit, "replace", results); err != nil {
		t.Fatalf("queue a job for %s: %v", unit, err)
	}
	select {
	case result := <-results:
		if result != jobResultDone {
			t.Fatalf("job for %s finished with %s", unit, result)
		}
	case <-time.After(unitChangeWait):
		t.Fatalf("job for %s did not finish within %s", unit, unitChangeWait)
	}
}

func (lab *interfaceLab) property(t *testing.T, unit string, name string) string {
	t.Helper()
	property, err := lab.systemd.GetUnitPropertyContext(context.Background(), unit, name)
	if err != nil {
		t.Fatalf("read %s of %s: %v", name, unit, err)
	}
	switch value := property.Value.Value().(type) {
	case string:
		return value
	case []byte:
		return hex.EncodeToString(value)
	default:
		t.Fatalf("%s of %s has type %T", name, unit, value)
		return ""
	}
}

func (lab *interfaceLab) invocation(t *testing.T, unit string) string {
	t.Helper()
	return lab.property(t, unit, "InvocationID")
}

func (lab *interfaceLab) activeState(t *testing.T, unit string) string {
	t.Helper()
	return lab.property(t, unit, "ActiveState")
}

// createWatchedLink creates the watched veth pair with index, or with a
// kernel-chosen index when index is 0, and returns the new index.
func (lab *interfaceLab) createWatchedLink(t *testing.T, index int) int {
	t.Helper()
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: watchedLink, Index: index},
		PeerName:  watchedPeer,
	}
	if err := lab.handle.LinkAdd(veth); err != nil {
		t.Fatalf("add %s with index %d: %v", watchedLink, index, err)
	}
	setLinkUp(t, lab.handle, watchedLink)
	setLinkUp(t, lab.handle, watchedPeer)
	addAddress(t, lab.handle, watchedLink, watchedAddressCIDR)
	return linkByName(t, lab.handle, watchedLink).Attrs().Index
}

func (lab *interfaceLab) deleteWatchedLink(t *testing.T) {
	t.Helper()
	if err := lab.handle.LinkDel(linkByName(t, lab.handle, watchedLink)); err != nil {
		t.Fatalf("delete %s: %v", watchedLink, err)
	}
}

func (lab *interfaceLab) startWatcher(t *testing.T) *commandProcess {
	t.Helper()
	process := startCommand(t, lab.namespace, commandBinary(t), "interface",
		"--interface", watchedLink,
		"--dhcp4-unit", lab.dhcp4Unit,
		"--dhcp6-unit", lab.dhcp6Unit,
		"--route-unit", lab.routeUnit)
	process.waitForLine(t, watcherStartWait, "msg=network_watch.interface.watching")
	return process
}

// invocations returns the current InvocationID of each unit.
func (lab *interfaceLab) invocations(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		lab.dhcp4Unit: lab.invocation(t, lab.dhcp4Unit),
		lab.dhcp6Unit: lab.invocation(t, lab.dhcp6Unit),
		lab.routeUnit: lab.invocation(t, lab.routeUnit),
	}
}

func (lab *interfaceLab) requireUnchanged(t *testing.T, before map[string]string, reason string) {
	t.Helper()
	for unit, invocation := range lab.invocations(t) {
		if invocation != before[unit] {
			t.Fatalf("%s restarted after %s", unit, reason)
		}
	}
}

func TestInterfaceWatcherRestartsUnitsAfterIndexChange(t *testing.T) {
	lab := newInterfaceLab(t, helperUDP6Device)
	process := lab.startWatcher(t)
	before := lab.invocations(t)

	setLinkDown(t, lab.handle, watchedPeer)
	time.Sleep(unchangedPolls)
	setLinkUp(t, lab.handle, watchedPeer)
	time.Sleep(unchangedPolls)
	lab.requireUnchanged(t, before, "a carrier change with the same interface index")

	index := linkByName(t, lab.handle, watchedLink).Attrs().Index
	lab.deleteWatchedLink(t)
	time.Sleep(unchangedPolls)
	lab.requireUnchanged(t, before, "the interface disappeared")
	lab.createWatchedLink(t, index)
	time.Sleep(unchangedPolls)
	lab.requireUnchanged(t, before, "the interface returned with its previous index")

	lab.runJob(t, lab.dhcp4Unit, lab.systemd.StopUnitContext)
	lab.runJob(t, lab.routeUnit, lab.systemd.StopUnitContext)
	lab.deleteWatchedLink(t)
	newIndex := lab.createWatchedLink(t, 0)
	if newIndex == index {
		t.Fatalf("recreated %s kept index %d, want a new index", watchedLink, index)
	}
	process.waitForLine(t, unitChangeWait, "msg=network_watch.interface.index_changed")
	waitFor(t, unitChangeWait, "the running DHCPv6 unit to restart", func() bool {
		return lab.invocation(t, lab.dhcp6Unit) != before[lab.dhcp6Unit]
	})
	waitFor(t, unitChangeWait, "the stopped route unit to start", func() bool {
		return lab.activeState(t, lab.routeUnit) == activeState
	})
	if state := lab.activeState(t, lab.dhcp4Unit); state != inactiveState {
		t.Fatalf("stopped DHCPv4 unit state = %s, want %s after try-restart", state, inactiveState)
	}
}

func TestInterfaceWatcherRestartsKeaAfterMissingSocketPolls(t *testing.T) {
	lab := newInterfaceLab(t, helperIdle)
	process := lab.startWatcher(t)
	before := lab.invocations(t)

	time.Sleep(beforeMissingWait)
	lab.requireUnchanged(t, before, "fewer than fifteen polls without a socket")

	process.waitForLine(t, missingSocketWait, "msg=network_watch.interface.socket_missing unit="+lab.dhcp6Unit)
	waitFor(t, unitChangeWait, "the DHCPv6 unit without a socket to restart", func() bool {
		return lab.invocation(t, lab.dhcp6Unit) != before[lab.dhcp6Unit]
	})
	if lab.invocation(t, lab.dhcp4Unit) != before[lab.dhcp4Unit] {
		t.Fatal("the DHCPv4 unit with a socket restarted")
	}
}

func TestCommandRejectsMissingRequiredFlags(t *testing.T) {
	process := startCommand(t, netns.None(), commandBinary(t), "interface", "--interface", watchedLink)
	status := process.wait(t)
	if status != exitUsage {
		t.Fatalf("exit status = %d, want %d", status, exitUsage)
	}
	output := process.capturedOutput()
	if !strings.Contains(output, "missing required flags --dhcp4-unit, --dhcp6-unit, --route-unit") {
		t.Fatalf("output does not name the missing flags:\n%s", output)
	}
}
