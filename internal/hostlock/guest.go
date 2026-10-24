package hostlock

import (
	"errors"
	"path/filepath"
	"strconv"
)

// ValidateGuestIdentity requires a node and a canonical positive numeric VMID.
func ValidateGuestIdentity(node, vmid string) error {
	identifier, err := strconv.ParseUint(vmid, 10, 32)
	if err != nil || identifier == 0 || strconv.FormatUint(identifier, 10) != vmid || node == "" {
		return errors.New("guest lock requires a known node and positive numeric VMID")
	}
	return nil
}

// ForGuest selects a guest-specific directory on an existing host lock target.
func ForGuest(host Host, node, vmid string) (Host, error) {
	if err := ValidateGuestIdentity(node, vmid); err != nil {
		return Host{}, err
	}
	dir := host.Dir
	if dir == "" {
		dir = DefaultDir
	}
	host.Name = "guest:" + node + "/" + vmid
	host.Dir = filepath.Join(dir, "guests", vmid)
	return host, nil
}
