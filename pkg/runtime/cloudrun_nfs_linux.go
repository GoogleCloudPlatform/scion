//go:build linux

package runtime

import (
	"fmt"
	"os"
	"syscall"
)

const nfsSuperMagic = 0x6969

func requireNFSFilesystem(hostBase string) error {
	info, err := os.Stat(hostBase)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("cloudrun: Hub cannot access NFS export at %s; "+
				"mount the Filestore export into the Hub Cloud Run service at this path or run an external provisioner", hostBase)
		}
		return fmt.Errorf("cloudrun: cannot inspect Hub NFS mount %s: %w", hostBase, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cloudrun: Hub NFS mount path %s is not a directory", hostBase)
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(hostBase, &stat); err != nil {
		return fmt.Errorf("cloudrun: cannot stat filesystem for Hub NFS mount %s: %w", hostBase, err)
	}
	if stat.Type != nfsSuperMagic {
		return fmt.Errorf("cloudrun: Hub path %s exists but is not an NFS filesystem (statfs type %#x); "+
			"mount the Filestore export into the Hub Cloud Run service or run an external provisioner",
			hostBase, stat.Type)
	}
	return nil
}
