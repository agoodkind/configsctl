package tofulock

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"goodkind.io/configsctl/hostlock"
)

const (
	supportedFormatMajor = "1."
	guestTypePrefix      = "pveguest_"
)

// Decision selects declared hosts or every hypervisor when plan mapping fails.
type Decision struct {
	AllHosts   bool
	Reason     string
	Hosts      []string
	GuestLocks []GuestLock
}

// GuestLock selects one guest's lock directory on its declared SSH host.
type GuestLock struct {
	Node string
	VMID string
	Host string
}

// Validate requires a known node, an SSH host, and a positive numeric VMID.
func (g GuestLock) Validate() error {
	if g.Host == "" {
		return fmt.Errorf("guest lock for node %s requires a declared host and a positive numeric VMID", g.Node)
	}
	if err := hostlock.ValidateGuestIdentity(g.Node, g.VMID); err != nil {
		slog.Error("tofu.lock.guest_identity_invalid", "node", g.Node, "err", err)
		return fmt.Errorf("validate guest lock identity: %w", err)
	}
	return nil
}

// Decide skips no-op and read changes and resource types in LockFreeTypes.
// Decide selects every hypervisor when plan decoding, format validation, or
// host selection fails.
func Decide(plan []byte, targets Targets) Decision {
	return decide(plan, targets, false)
}

// DecideIndependent selects exact guest owners without adding their hypervisors.
// AllHosts reports an ownership failure that independent workspaces must refuse.
func DecideIndependent(plan []byte, targets Targets) Decision {
	return decide(plan, targets, true)
}

func decide(plan []byte, targets Targets, independent bool) Decision {
	var document planDocument
	if err := json.Unmarshal(plan, &document); err != nil {
		return allHosts("the plan JSON does not decode: " + err.Error())
	}
	if !strings.HasPrefix(document.FormatVersion, supportedFormatMajor) {
		return allHosts(fmt.Sprintf("the plan JSON format version %q is not 1.x", document.FormatVersion))
	}
	found := map[string]struct{}{}
	guestLocks := map[GuestLock]struct{}{}
	for _, resource := range document.ResourceChanges {
		if resource.Change.inert() || slices.Contains(targets.LockFreeTypes, resource.Type) {
			continue
		}
		hosts, guests, reason := targets.changeHosts(resource, independent)
		if reason != "" {
			return allHosts(reason)
		}
		for _, host := range hosts {
			found[host] = struct{}{}
		}
		for _, guest := range guests {
			guestLocks[guest] = struct{}{}
		}
	}
	guests := slices.Collect(maps.Keys(guestLocks))
	slices.SortFunc(guests, func(first, second GuestLock) int {
		return strings.Compare(first.Host+"/"+first.VMID+"/"+first.Node, second.Host+"/"+second.VMID+"/"+second.Node)
	})
	return Decision{AllHosts: false, Reason: "", Hosts: slices.Sorted(maps.Keys(found)), GuestLocks: guests}
}

// PendingAddresses returns sorted addresses of pending resource changes from plan JSON.
// The function returns an error when decoding fails or the format version is outside 1.x.
func PendingAddresses(plan []byte) ([]string, error) {
	var document planDocument
	if err := json.Unmarshal(plan, &document); err != nil {
		slog.Error("tofu.plan.decode_failed", "err", err)
		return nil, fmt.Errorf("decode the plan JSON: %w", err)
	}
	if !strings.HasPrefix(document.FormatVersion, supportedFormatMajor) {
		return nil, fmt.Errorf("the plan JSON format version %q is not 1.x", document.FormatVersion)
	}
	var addresses []string
	for _, resource := range document.ResourceChanges {
		if !resource.Change.inert() {
			addresses = append(addresses, resource.Address)
		}
	}
	slices.Sort(addresses)
	return addresses, nil
}

func allHosts(reason string) Decision {
	return Decision{AllHosts: true, Reason: reason, Hosts: nil, GuestLocks: nil}
}

func (t Targets) changeHosts(resource resourceChange, independent bool) ([]string, []GuestLock, string) {
	if independent && strings.HasPrefix(resource.Type, guestTypePrefix) {
		return t.independentGuestHosts(resource)
	}
	states := resource.Change.states()
	if resource.Change.deletes() && states.nodeUnknown() {
		return nil, nil, resource.Address + " deletes a resource with an unknown node"
	}
	var nodeHosts []string
	if t.NodeHosts != nil && strings.HasPrefix(resource.Type, guestTypePrefix) {
		var reason string
		nodeHosts, reason = t.nodeHosts(resource.Address, states)
		if reason != "" {
			return nil, nil, reason
		}
	}
	hosts := nodeHosts
	for _, address := range resource.addresses() {
		if independent {
			if host, declared := t.ModuleHosts[address]; declared {
				hosts = append(hosts, host)
				continue
			}
		}
		addressHosts, reason := t.moduleHosts(address)
		if reason != "" {
			return nil, nil, reason
		}
		if len(addressHosts) == 0 && len(nodeHosts) == 0 {
			return nil, nil, address + " matches no lock target"
		}
		hosts = append(hosts, addressHosts...)
	}
	return hosts, nil, ""
}

func (t Targets) independentGuestHosts(resource resourceChange) ([]string, []GuestLock, string) {
	var explicit []string
	for _, address := range resource.addresses() {
		if host, declared := t.ModuleHosts[address]; declared {
			explicit = append(explicit, host)
		}
	}
	if len(explicit) == len(resource.addresses()) {
		return explicit, nil, ""
	}
	if resource.Change.deletes() && resource.Change.states().nodeUnknown() {
		return nil, nil, resource.Address + " deletes a resource with an unknown node"
	}
	hosts, guests, reason := t.independentNodeHosts(resource.Address, resource.Change.states())
	return append(hosts, explicit...), guests, reason
}

func (t Targets) independentNodeHosts(address string, states changeStates) ([]string, []GuestLock, string) {
	present := states.present()
	if len(present) == 0 {
		return nil, nil, address + " has no node"
	}
	guestResource := slices.ContainsFunc(present, func(state *resourceState) bool {
		return state.declares(vmidAttribute)
	})
	var hosts []string
	var guests []GuestLock
	for _, state := range present {
		node, known := state.text(nodeAttribute)
		if !known {
			return nil, nil, address + " has an unknown or absent node"
		}
		if guestResource {
			stateHosts, stateGuests, reason := t.independentGuestState(node, state)
			if reason != "" {
				return nil, nil, address + reason
			}
			hosts = append(hosts, stateHosts...)
			guests = append(guests, stateGuests...)
			continue
		}
		host, declared := t.NodeHosts[node]
		if !declared {
			return nil, nil, address + " has no host lock target for its node"
		}
		hosts = append(hosts, host)
	}
	return hosts, guests, ""
}

func (t Targets) independentGuestState(node string, state *resourceState) ([]string, []GuestLock, string) {
	vmid, known := state.text(vmidAttribute)
	if !known {
		return nil, nil, " has an unknown or null vmid"
	}
	host, enrolled := t.GuestHosts[node+"/"+vmid]
	if !enrolled {
		host = t.NodeHosts[node]
	}
	guest := GuestLock{Node: node, VMID: vmid, Host: host}
	if err := guest.Validate(); err != nil {
		return nil, nil, " has an unknown guest lock identity: " + err.Error()
	}
	if enrolled {
		return []string{host}, nil, ""
	}
	return nil, []GuestLock{guest}, ""
}

func (t Targets) moduleHosts(address string) ([]string, string) {
	var hosts []string
	for module, host := range t.ModuleHosts {
		if address == module || strings.HasPrefix(address, module+".") {
			hosts = append(hosts, host)
		}
	}
	for module, keyHosts := range t.ModuleKeyHosts {
		key, inside := moduleKey(address, module)
		if !inside {
			continue
		}
		host, declared := keyHosts[key]
		if !declared {
			return nil, fmt.Sprintf("%s has no host for the key %q of %s", address, key, module)
		}
		hosts = append(hosts, host)
	}
	return hosts, ""
}

func moduleKey(address, module string) (string, bool) {
	rest, inside := strings.CutPrefix(address, module+"[")
	if !inside {
		return "", false
	}
	index, _, found := strings.Cut(rest, "].")
	if !found {
		return "", true
	}
	var key string
	if err := json.Unmarshal([]byte(index), &key); err != nil {
		return "", true
	}
	return key, true
}

func (t Targets) nodeHosts(address string, states changeStates) ([]string, string) {
	present := states.present()
	if len(present) == 0 {
		return nil, address + " has no node"
	}
	guestVmid := t.GuestHosts != nil && slices.ContainsFunc(present, func(state *resourceState) bool {
		return state.declares(vmidAttribute)
	})
	var hosts []string
	for _, state := range present {
		node, known := state.text(nodeAttribute)
		if !known {
			return nil, address + " has an unknown or absent node"
		}
		host, declared := t.NodeHosts[node]
		if !declared {
			return nil, fmt.Sprintf("%s is on node %s, which has no lock target", address, node)
		}
		hosts = append(hosts, host)
		if !guestVmid {
			continue
		}
		vmid, vmidKnown := state.text(vmidAttribute)
		if !vmidKnown {
			return nil, address + " has an unknown or null vmid"
		}
		if guest, guestDeclared := t.GuestHosts[node+"/"+vmid]; guestDeclared {
			hosts = append(hosts, guest)
		}
	}
	return hosts, ""
}
