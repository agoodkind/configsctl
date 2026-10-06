package tofulock

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

const (
	supportedFormatMajor = "1."
	guestTypePrefix      = "pveguest_"
)

// Decision selects declared hosts or every hypervisor when plan mapping fails.
type Decision struct {
	AllHosts bool
	Reason   string
	Hosts    []string
}

// Decide selects declared hosts for changes other than no-op and read.
// Decide requires every hypervisor when plan decoding, format validation, or
// host selection fails.
func Decide(plan []byte, targets Targets) Decision {
	var document planDocument
	if err := json.Unmarshal(plan, &document); err != nil {
		return allHosts("the plan JSON does not decode: " + err.Error())
	}
	if !strings.HasPrefix(document.FormatVersion, supportedFormatMajor) {
		return allHosts(fmt.Sprintf("the plan JSON format version %q is not 1.x", document.FormatVersion))
	}
	found := map[string]struct{}{}
	for _, resource := range document.ResourceChanges {
		if resource.Change.inert() {
			continue
		}
		hosts, reason := targets.changeHosts(resource)
		if reason != "" {
			return allHosts(reason)
		}
		for _, host := range hosts {
			found[host] = struct{}{}
		}
	}
	return Decision{AllHosts: false, Reason: "", Hosts: slices.Sorted(maps.Keys(found))}
}

func allHosts(reason string) Decision {
	return Decision{AllHosts: true, Reason: reason, Hosts: nil}
}

func (t Targets) changeHosts(resource resourceChange) ([]string, string) {
	states := resource.Change.states()
	if resource.Change.deletes() && states.nodeUnknown() {
		return nil, resource.Address + " deletes a resource with an unknown node"
	}
	hosts, reason := t.moduleHosts(resource.Address)
	if reason != "" {
		return nil, reason
	}
	if t.NodeHosts != nil && strings.HasPrefix(resource.Type, guestTypePrefix) {
		nodeHosts, nodeReason := t.nodeHosts(resource.Address, states)
		if nodeReason != "" {
			return nil, nodeReason
		}
		hosts = append(hosts, nodeHosts...)
	}
	if len(hosts) == 0 {
		return nil, resource.Address + " matches no lock target"
	}
	return hosts, ""
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
		if vmid, vmidKnown := state.text(vmidAttribute); vmidKnown {
			if guest, guestDeclared := t.GuestHosts[node+"/"+vmid]; guestDeclared {
				hosts = append(hosts, guest)
			}
		}
	}
	return hosts, ""
}
