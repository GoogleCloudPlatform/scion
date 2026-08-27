//go:build !linux

package runtime

import (
	"fmt"
	"os"
)

// requireNFSFilesystem validates that hostBase exists and is a directory.
// On non-Linux platforms the NFS filesystem-type check is skipped because
// syscall.Statfs is Linux-specific.
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
	// NFS filesystem-type check skipped on this platform.
	return nil
}
