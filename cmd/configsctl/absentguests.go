package main

import (
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"goodkind.io/configsctl/internal/ansible"
)

const (
	serviceVMIDVariable = "service_vmid"
	proxmoxNodeVariable = "ansible_proxmox_host"
	proxmoxVMIDVariable = "ansible_proxmox_vmid"
	guestNodeLabelIndex = 1
	limitColonExclusion = ":!"
	limitCommaExclusion = ",!"
	limitCommaSeparator = ","
	limitAllHosts       = "all"
	limitFilePrefix     = "@"
)

type absentGuest struct {
	host string
	node string
	vmid int
}

// skipAbsentGuests removes play hosts without a matching Proxmox guest from the
// deploy targets and adds an exclusion for each skipped host to the Ansible
// limit.
func skipAbsentGuests(
	targets deployTargets, opts ansible.DeployOptions,
) (deployTargets, ansible.DeployOptions, error) {
	nodes := targets.settings.Deploy.AbsentGuestNodes
	absent, err := absentGuests(targets.inventory, targets.playHosts, nodes)
	if err != nil || len(absent) == 0 {
		return targets, opts, err
	}
	if err := requireUnenrolled(targets.settings.Tofu, absent); err != nil {
		return targets, opts, err
	}
	hosts := make([]string, 0, len(absent))
	for _, guest := range absent {
		hosts = append(hosts, guest.host)
	}
	if len(absent) == len(targets.playHosts) {
		err := fmt.Errorf("every play host has no Proxmox guest: %s", strings.Join(hosts, ", "))
		slog.Error("deploy.absent.all_hosts", "hosts", hosts, "err", err)
		return targets, opts, err
	}
	limit, err := excludeHostsLimit(opts.Limit, hosts)
	if err != nil {
		return targets, opts, err
	}
	logAbsentGuests(absent, opts.Output)
	targets.playHosts = slices.DeleteFunc(slices.Clone(targets.playHosts), func(host string) bool {
		return slices.Contains(hosts, host)
	})
	opts.Limit = limit
	return targets, opts, nil
}

func absentGuests(inv ansible.Inventory, playHosts []string, nodes map[string]string) ([]absentGuest, error) {
	var candidates []absentGuest
	for _, host := range playHosts {
		labels := strings.Split(host, ".")
		if !inv.HasVariable(host, serviceVMIDVariable) || len(labels) <= guestNodeLabelIndex {
			continue
		}
		node, mapped := nodes[labels[guestNodeLabelIndex]]
		if !mapped {
			continue
		}
		vmid, err := inv.HostInteger(host, serviceVMIDVariable)
		if err != nil {
			slog.Error("deploy.absent.vmid_read_failed", "host", host, "variable", serviceVMIDVariable, "err", err)
			return nil, fmt.Errorf("read the guest VMID of %s: %w", host, err)
		}
		candidates = append(candidates, absentGuest{host: host, node: node, vmid: vmid})
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	guests, err := nodeGuests(inv, candidates)
	if err != nil {
		return nil, err
	}
	var absent []absentGuest
	for _, candidate := range candidates {
		if !guests[candidate.node][candidate.vmid] {
			absent = append(absent, candidate)
		}
	}
	return absent, nil
}

// nodeGuests returns the VMIDs that the Proxmox inventory reports for each node
// represented by the candidates.
func nodeGuests(inv ansible.Inventory, candidates []absentGuest) (map[string]map[int]bool, error) {
	guests := map[string]map[int]bool{}
	for _, candidate := range candidates {
		guests[candidate.node] = map[int]bool{}
	}
	for _, host := range slices.Sorted(maps.Keys(inv.Hosts)) {
		if !inv.HasVariable(host, proxmoxNodeVariable) {
			continue
		}
		node, err := inv.HostString(host, proxmoxNodeVariable)
		if err != nil {
			slog.Error("deploy.absent.node_read_failed", "host", host, "variable", proxmoxNodeVariable, "err", err)
			return nil, fmt.Errorf("read the Proxmox node of %s: %w", host, err)
		}
		vmids, used := guests[node]
		if !used {
			continue
		}
		vmid, err := inv.HostInteger(host, proxmoxVMIDVariable)
		if err != nil {
			slog.Error("deploy.absent.vmid_read_failed", "host", host, "variable", proxmoxVMIDVariable, "err", err)
			return nil, fmt.Errorf("read the Proxmox VMID of %s: %w", host, err)
		}
		vmids[vmid] = true
	}
	for _, node := range slices.Sorted(maps.Keys(guests)) {
		if len(guests[node]) == 0 {
			err := fmt.Errorf("the Proxmox inventory returned no guests for node %s", node)
			slog.Error("deploy.absent.node_empty", "node", node, "err", err)
			return nil, err
		}
	}
	return guests, nil
}

func requireUnenrolled(tofu tofuSettings, absent []absentGuest) error {
	enrolledHosts := slices.Collect(maps.Values(tofu.guestHosts()))
	for _, guest := range absent {
		if slices.Contains(enrolledHosts, guest.host) {
			err := fmt.Errorf("host %s is a value of tofu.guest_hosts, and node %s reports no guest with VMID %d",
				guest.host, guest.node, guest.vmid)
			slog.Error("deploy.absent.enrolled_guest", "host", guest.host, "node", guest.node, "vmid", guest.vmid,
				"err", err)
			return err
		}
	}
	return nil
}

func excludeHostsLimit(limit string, hosts []string) (string, error) {
	if strings.HasPrefix(limit, limitFilePrefix) {
		err := fmt.Errorf("cannot exclude the hosts with no Proxmox guest from the host file limit %s: %s",
			limit, strings.Join(hosts, ", "))
		slog.Error("deploy.absent.limit_refused", "limit", limit, "hosts", hosts, "err", err)
		return "", err
	}
	if limit == "" {
		limit = limitAllHosts
	}
	// Ansible splits limits containing commas only at commas, not at colons.
	// excludeHostsLimit uses ,!host for those limits and :!host otherwise.
	exclusion := limitColonExclusion
	if strings.Contains(limit, limitCommaSeparator) {
		exclusion = limitCommaExclusion
	}
	return limit + exclusion + strings.Join(hosts, exclusion), nil
}

func logAbsentGuests(absent []absentGuest, output io.Writer) {
	hosts := make([]string, 0, len(absent))
	nodes := make([]string, 0, len(absent))
	vmids := make([]int, 0, len(absent))
	for _, guest := range absent {
		hosts = append(hosts, guest.host)
		nodes = append(nodes, guest.node)
		vmids = append(vmids, guest.vmid)
	}
	slog.Info("deploy.hosts_absent", "hosts", hosts, "nodes", nodes, "vmids", vmids)
	fmt.Fprintf(output, "Skipping hosts with no Proxmox guest: %s\n", strings.Join(hosts, ", "))
}
