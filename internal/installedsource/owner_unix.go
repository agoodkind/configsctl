//go:build unix

package installedsource

import (
	"fmt"
	"io/fs"
	"syscall"
)

func checkOwnerIDs(path string, info fs.FileInfo, uid, gid int64) error {
	stat, present := info.Sys().(*syscall.Stat_t)
	if !present || int64(stat.Uid) != uid || int64(stat.Gid) != gid {
		return fmt.Errorf("installed input %s does not match its observed owner and group", path)
	}
	return nil
}
