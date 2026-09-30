/*
Copyright 2026 The Scion Authors.
*/

package rootexec

import "testing"

// TestValidWorkloadID_RefusesUint32OverflowAndSentinels proves the numeric
// fail-open this guards against: strconv.Atoi would happily parse
// "4294967296" (2^32) into Go's 64-bit int, which then passes every
// "uid > 0" guard downstream and only fails once cast to uint32 for
// syscall.Credential — where it silently wraps to 0 (root). ValidWorkloadID
// must refuse it (and the 2^32-1 sentinel) outright, while still accepting
// an ordinary workload uid.
//
// MUTATION: widen the parse back to strconv.Atoi (64-bit) and drop the
// MaxUint32/zero checks — this test goes red on the overflow and sentinel
// cases (they'd start returning nil error).
func TestValidWorkloadID_RefusesUint32OverflowAndSentinels(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
		want    uint32
	}{
		{"ordinary workload uid", "1000", false, 1000},
		{"2^32 overflows uint32", "4294967296", true, 0},
		{"2^32 + 0's own uint32 truncation target", "4294967296", true, 0},
		{"2^32-1 sentinel refused", "4294967295", true, 0},
		{"zero (root) refused", "0", true, 0},
		{"negative refused", "-1", true, 0},
		{"non-numeric refused", "not-a-number", true, 0},
		{"largest valid uint32 minus one accepted", "4294967294", false, 4294967294},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidWorkloadID(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidWorkloadID(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ValidWorkloadID(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestValidWorkloadID_BothUIDAndGIDPaths proves the guard applies uniformly
// regardless of which of the two fields (uid or gid) a caller is validating
// — there is only one code path, but this pins that a caller validating
// each field independently (as setupHostUser does for SCION_HOST_UID and
// SCION_HOST_GID) gets the identical refusal for each.
func TestValidWorkloadID_BothUIDAndGIDPaths(t *testing.T) {
	for _, s := range []string{"4294967296", "4294967295", "0"} {
		if _, err := ValidWorkloadID(s); err == nil {
			t.Errorf("ValidWorkloadID(%q) = nil error, want refusal (uid path)", s)
		}
		if _, err := ValidWorkloadID(s); err == nil {
			t.Errorf("ValidWorkloadID(%q) = nil error, want refusal (gid path)", s)
		}
	}
}
