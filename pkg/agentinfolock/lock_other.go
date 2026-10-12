//go:build !unix

/*
Copyright 2026 The Scion Authors.
*/

package agentinfolock

// acquire has no flock on this platform; updates run unlocked.
func acquire(lockPath string, opts Options) (func(), bool) {
	opts.logf("no file lock on this platform, updating %s unlocked", lockPath)
	return nil, false
}
