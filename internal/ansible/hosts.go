package ansible

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// HostVars is the part of a host's inventory variables that a connection
// needs.
type HostVars struct {
	AnsibleHost       string `json:"ansible_host"`
	AnsibleUser       string `json:"ansible_user"`
	AnsibleConnection string `json:"ansible_connection"`
}

// Inventory is the resolved inventory: host variables and group membership.
type Inventory struct {
	Hosts     map[string]HostVars
	groups    map[string]inventoryGroup
	variables map[string]map[string]json.RawMessage
}

type inventoryGroup struct {
	Hosts    []string `json:"hosts"`
	Children []string `json:"children"`
}

type inventoryList struct {
	Meta struct {
		HostVars map[string]HostVars `json:"hostvars"`
	} `json:"_meta"`
}

type inventoryVariables struct {
	Meta struct {
		HostVars map[string]map[string]json.RawMessage `json:"hostvars"`
	} `json:"_meta"`
}

// LoadInventory runs ansible-inventory --list in the ansible directory of
// repoRoot and decodes the result. An empty repoRoot is the working directory.
func LoadInventory(ctx context.Context, repoRoot string) (Inventory, error) {
	out, err := captureAnsible(ctx, repoRoot, "ansible-inventory", "--list", "--vault-password-file", vaultPassPath())
	if err != nil {
		return Inventory{}, err
	}
	return DecodeInventory(out)
}

// DecodeInventory reads connection settings, raw host variables, and group
// membership from ansible-inventory --list JSON.
func DecodeInventory(out []byte) (Inventory, error) {
	var list inventoryList
	if err := json.Unmarshal(out, &list); err != nil {
		slog.Error("ansible.inventory.decode_failed", "err", err)
		return Inventory{}, fmt.Errorf("decode ansible-inventory hostvars: %w", err)
	}
	var variables inventoryVariables
	if err := json.Unmarshal(out, &variables); err != nil {
		slog.Error("ansible.inventory.decode_failed", "err", err)
		return Inventory{}, fmt.Errorf("decode ansible-inventory host variables: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		slog.Error("ansible.inventory.decode_failed", "err", err)
		return Inventory{}, fmt.Errorf("decode ansible-inventory groups: %w", err)
	}
	groups := map[string]inventoryGroup{}
	for name, body := range raw {
		if name == "_meta" {
			continue
		}
		var group inventoryGroup
		if err := json.Unmarshal(body, &group); err == nil {
			groups[name] = group
		}
	}
	return Inventory{Hosts: list.Meta.HostVars, groups: groups, variables: variables.Meta.HostVars}, nil
}

// GroupHosts returns the hosts of group and of its child groups, sorted.
func (inv Inventory) GroupHosts(group string) []string {
	found := map[string]struct{}{}
	seen := map[string]bool{}
	worklist := []string{group}
	for len(worklist) > 0 {
		name := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]
		if seen[name] {
			continue
		}
		seen[name] = true
		entry := inv.groups[name]
		for _, host := range entry.Hosts {
			found[host] = struct{}{}
		}
		worklist = append(worklist, entry.Children...)
	}
	return sortedKeys(found)
}

// PlaybookName removes directories and .yml or .yaml suffixes for
// declaration lookup.
func PlaybookName(playbook string) string {
	base := filepath.Base(playbook)
	return strings.TrimSuffix(strings.TrimSuffix(base, ".yml"), ".yaml")
}

var listHostsHeader = regexp.MustCompile(`^(\s*)hosts \(\d+\):$`)

// PlayHosts returns the hosts that `opts` targets, from `ansible-playbook
// --list-hosts`. A static import path can use an extra var. The listing
// receives the extra vars of the deploy.
func PlayHosts(ctx context.Context, opts DeployOptions) ([]string, error) {
	args := []string{"--list-hosts", "--vault-password-file", vaultPassPath(), playbookArg(opts.Playbook)}
	if opts.Limit != "" {
		args = append(args, "--limit", opts.Limit)
	}
	for _, extra := range opts.ExtraVars {
		args = append(args, "--extra-vars", extra)
	}
	out, err := captureAnsible(ctx, "", "ansible-playbook", args...)
	if err != nil {
		return nil, err
	}
	return parseListHosts(out), nil
}

// parseListHosts reads the host lines under each "hosts (N):" header.
func parseListHosts(out []byte) []string {
	found := map[string]struct{}{}
	headerIndent := -1
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if match := listHostsHeader.FindStringSubmatch(line); match != nil {
			headerIndent = len(match[1])
			continue
		}
		if headerIndent < 0 {
			continue
		}
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if trimmed == "" || indent <= headerIndent {
			headerIndent = -1
			continue
		}
		found[trimmed] = struct{}{}
	}
	return sortedKeys(found)
}

func captureAnsible(ctx context.Context, repoRoot, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = filepath.Join(repoRoot, ansibleDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.Error("ansible.capture_failed", "command", name, "stderr", strings.TrimSpace(stderr.String()), "err", err)
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
