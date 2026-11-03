package tofulock_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"goodkind.io/configsctl/hostlock"
	"goodkind.io/configsctl/internal/tofulock"
)

func endpointTargets() tofulock.Targets {
	return tofulock.Targets{
		ModuleHosts:    map[string]string{"module.host": "hv_alpha"},
		ModuleKeyHosts: map[string]map[string]string{"module.base": {"guest_alpha": "guest-alpha"}},
		NodeHosts:      map[string]string{"site": "hv_alpha"},
		GuestHosts:     map[string]string{"site/101": "guest-alpha"},
		LockFreeTypes:  nil,
		LockEndpoints: map[string]tofulock.LockEndpoint{
			"hv_alpha":    {User: "", Address: "hv-alpha.example"},
			"guest-alpha": {User: "deploy", Address: "guest-alpha.example"},
		},
	}
}

func TestLockEndpointsValidate(t *testing.T) {
	cases := map[string]struct {
		change func(*tofulock.Targets)
		want   string
	}{
		"declared endpoints": {change: func(*tofulock.Targets) {}, want: ""},
		"empty address": {
			change: func(targets *tofulock.Targets) {
				targets.LockEndpoints["hv_alpha"] = tofulock.LockEndpoint{User: "", Address: ""}
			},
			want: "lock_endpoints.hv_alpha has an empty address",
		},
		"unknown name": {
			change: func(targets *tofulock.Targets) {
				targets.LockEndpoints["stray"] = tofulock.LockEndpoint{User: "", Address: "stray.example"}
			},
			want: "lock_endpoints.stray is not a host in",
		},
		"missing endpoint": {
			change: func(targets *tofulock.Targets) { delete(targets.LockEndpoints, "guest-alpha") },
			want:   "lock_endpoints does not declare host guest-alpha from",
		},
		"empty map": {
			change: func(targets *tofulock.Targets) { targets.LockEndpoints = map[string]tofulock.LockEndpoint{} },
			want:   "lock_endpoints is empty",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			targets := endpointTargets()
			test.change(&targets)
			err := targets.Validate()
			if test.want == "" {
				if err != nil {
					t.Fatalf("Validate rejected the declared endpoints: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate returned %v, want an error containing %q", err, test.want)
			}
		})
	}
}

func TestLockHostsBuildsTheDeclaredEndpoints(t *testing.T) {
	targets := endpointTargets()
	decision := tofulock.Decision{
		AllHosts: false, Reason: "", Hosts: []string{"guest-alpha", "hv_alpha"},
		GuestLocks: []tofulock.GuestLock{{Node: "site", VMID: "102", Host: "hv_alpha"}},
	}
	hosts, err := targets.LockHosts(decision)
	if err != nil {
		t.Fatal(err)
	}
	want := []hostlock.Host{
		{Name: "guest-alpha", User: "deploy", Address: "guest-alpha.example", Dir: hostlock.DefaultDir},
		{Name: "hv_alpha", User: "root", Address: "hv-alpha.example", Dir: hostlock.DefaultDir},
		{
			Name: "guest:site/102", User: "root", Address: "hv-alpha.example",
			Dir: filepath.Join(hostlock.DefaultDir, "guests", "102"),
		},
	}
	if !slices.Equal(hosts, want) {
		t.Fatalf("LockHosts returned %v, want %v", hosts, want)
	}
}

func TestLockHostsRefusesAHostWithoutAnEndpoint(t *testing.T) {
	targets := endpointTargets()
	delete(targets.LockEndpoints, "hv_alpha")
	_, err := targets.LockHosts(tofulock.Decision{AllHosts: false, Reason: "", Hosts: []string{"hv_alpha"}, GuestLocks: nil})
	if err == nil || !strings.Contains(err.Error(), "host hv_alpha has no declared lock endpoint") {
		t.Fatalf("LockHosts returned %v, want the missing endpoint error", err)
	}
}

func TestWorkspacePolicyValidate(t *testing.T) {
	one := 1
	zero := 0
	cases := map[string]struct {
		change func(*tofulock.Targets)
		want   string
	}{
		"declared policy": {
			change: func(targets *tofulock.Targets) {
				targets.RequiredHosts = []string{"hv_alpha"}
				targets.MaxParallelism = &one
			},
			want: "",
		},
		"empty required hosts": {
			change: func(targets *tofulock.Targets) { targets.RequiredHosts = []string{} },
			want:   "required_hosts is empty",
		},
		"empty required host name": {
			change: func(targets *tofulock.Targets) { targets.RequiredHosts = []string{""} },
			want:   "required_hosts has an empty host name",
		},
		"required host without an endpoint": {
			change: func(targets *tofulock.Targets) { targets.RequiredHosts = []string{"hv_missing"} },
			want:   "lock_endpoints does not declare host hv_missing from required_hosts",
		},
		"required hosts without endpoints": {
			change: func(targets *tofulock.Targets) {
				targets.LockEndpoints = nil
				targets.RequiredHosts = []string{"hv_alpha"}
			},
			want: "required_hosts requires lock_endpoints",
		},
		"zero parallelism": {
			change: func(targets *tofulock.Targets) { targets.MaxParallelism = &zero },
			want:   "max_parallelism is 0",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			targets := endpointTargets()
			test.change(&targets)
			err := targets.Validate()
			if test.want == "" {
				if err != nil {
					t.Fatalf("Validate rejected the policy: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate returned %v, want %q", err, test.want)
			}
		})
	}
}

func TestLockHostsAddsRequiredHostsToAPlanWithoutChanges(t *testing.T) {
	targets := endpointTargets()
	targets.RequiredHosts = []string{"hv_alpha", "guest-alpha"}
	decision := tofulock.Decision{
		AllHosts: false, Reason: "", Hosts: []string{"guest-alpha"},
		GuestLocks: []tofulock.GuestLock{{Node: "site", VMID: "102", Host: "hv_alpha"}},
	}
	hosts, err := targets.LockHosts(decision)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		names = append(names, host.Name)
	}
	want := []string{"guest-alpha", "hv_alpha", "guest:site/102"}
	if !slices.Equal(names, want) {
		t.Fatalf("LockHosts returned %v, want the deduplicated union %v", names, want)
	}
	empty, err := targets.LockHosts(tofulock.Decision{AllHosts: false, Reason: "", Hosts: nil, GuestLocks: nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 2 {
		t.Fatalf("LockHosts for a plan without changes returned %d hosts, want the 2 required hosts", len(empty))
	}
}
