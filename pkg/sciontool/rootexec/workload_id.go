/*
Copyright 2026 The Scion Authors.
*/

package rootexec

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

// errWorkloadIDIsRoot and errWorkloadIDIsMaxUint32 are ValidWorkloadID's two
// refusal reasons, kept as package-level sentinels so a caller can
// distinguish them from an ordinary parse failure (via errors.Is) if it
// ever needs to, even though neither is itself exported.
var (
	errWorkloadIDIsRoot      = errors.New("rootexec: refusing uid/gid 0 (root) as a workload identity")
	errWorkloadIDIsMaxUint32 = errors.New("rootexec: refusing uid/gid 4294967295 (2^32-1) as a workload identity")
)

// ValidWorkloadID parses s (a SCION_HOST_UID/GID-shaped environment value,
// or any other string a caller receives as a workload uid or gid) as a
// uint32, and is the one place every root-context privilege-drop path
// should do so: strconv.ParseUint(s, 10, 32) itself already refuses
// anything wider than 32 bits or negative, unlike strconv.Atoi, which
// happily parses a value like "4294967296" (2^32) into Go's 64-bit int —
// a value that then passes every "uid > 0" guard downstream, right up until
// it is cast to uint32 for syscall.Credential, where it silently wraps
// around to 0 and drops "privilege" to root instead of the intended
// workload identity.
//
// On top of that width check, ValidWorkloadID always refuses
// math.MaxUint32 (4294967295, i.e. 2^32-1) — often the visible result of a
// signed-to-unsigned or -1-as-sentinel bug further upstream, not a real
// uid/gid any real system assigns — regardless of refuseZero.
//
// refuseZero additionally refuses 0 (root itself): a caller in a context
// where privilege drop is required sets this, since the whole point of a
// privilege drop is to leave uid/gid 0. A caller in a context where
// privilege drop is optional (e.g. setupHostUser outside RequirePrivilegeDrop)
// passes false, keeping the pre-existing behavior of accepting 0 and
// proceeding as root.
func ValidWorkloadID(s string, refuseZero bool) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("rootexec: %q is not a valid uid/gid: %w", s, err)
	}
	if v == 0 && refuseZero {
		return 0, errWorkloadIDIsRoot
	}
	if v == math.MaxUint32 {
		return 0, errWorkloadIDIsMaxUint32
	}
	return uint32(v), nil
}
