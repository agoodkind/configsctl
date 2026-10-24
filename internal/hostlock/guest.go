package hostlock

import (
	"fmt"
	"path/filepath"
	"strconv"
)

// ForGuest selects a guest-specific directory on an existing host lock target.
func ForGuest(host Host, node, vmid string) (Host, error) {
	identifier, err := strconv.ParseUint(vmid, 10, 32)
	if err != nil || identifier == 0 || strconv.FormatUint(identifier, 10) != vmid || node == "" {
		return Host{}, fmt.Errorf("guest lock on %s requires a known node and positive numeric VMID", host.Name)
	}
	dir := host.Dir
	if dir == "" {
		dir = DefaultDir
	}
	host.Name = "guest:" + node + "/" + vmid
	host.Dir = filepath.Join(dir, "guests", vmid)
	return host, nil
}
