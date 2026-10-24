package hostlock_test

import (
	"testing"

	"goodkind.io/configsctl/internal/hostlock"
)

func TestGuestLockRefusesUnsafeVMID(t *testing.T) {
	for _, vmid := range []string{"", "0", "-1", "00101", "../101", "101/102", "101.5"} {
		t.Run(vmid, func(t *testing.T) {
			host := localHost(t)
			if _, err := hostlock.ForGuest(host, "site", vmid); err == nil {
				t.Fatalf("the guest lock accepted unsafe VMID %q", vmid)
			}
		})
	}
}
