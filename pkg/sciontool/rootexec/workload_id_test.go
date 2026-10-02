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
// refuses it (and the 2^32-1 sentinel) outright, in both modes, while still
// accepting an ordinary workload uid.
func TestValidWorkloadID_RefusesUint32OverflowAndSentinels(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
		want    uint32
	}{
		{"ordinary workload uid", "1000", false, 1000},
		{"2^32 overflows uint32", "4294967296", true, 0},
		{"2^32-1 sentinel refused", "4294967295", true, 0},
		{"negative refused", "-1", true, 0},
		{"non-numeric refused", "not-a-number", true, 0},
		{"empty string refused", "", true, 0},
		{"largest valid uint32 minus one accepted", "4294967294", false, 4294967294},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, refuseZero := range []bool{true, false} {
				got, err := ValidWorkloadID(tt.in, refuseZero)
				if (err != nil) != tt.wantErr {
					t.Fatalf("ValidWorkloadID(%q, %v) error = %v, wantErr %v", tt.in, refuseZero, err, tt.wantErr)
				}
				if !tt.wantErr && got != tt.want {
					t.Errorf("ValidWorkloadID(%q, %v) = %d, want %d", tt.in, refuseZero, got, tt.want)
				}
			}
		})
	}
}

// TestValidWorkloadID_ZeroRefusalIsModeGated proves the one behavior that
// actually depends on refuseZero: "0" is refused when true (a
// RequirePrivilegeDrop context, where regaining root defeats the whole
// point of the mode) and accepted when false (an optional-drop context,
// where proceeding as root is the correct outcome).
func TestValidWorkloadID_ZeroRefusalIsModeGated(t *testing.T) {
	if _, err := ValidWorkloadID("0", true); err == nil {
		t.Error("ValidWorkloadID(\"0\", true) = nil error, want refusal (RequirePrivilegeDrop context)")
	}
	got, err := ValidWorkloadID("0", false)
	if err != nil {
		t.Errorf("ValidWorkloadID(\"0\", false) = %v, want nil (optional-drop context keeps base behavior)", err)
	}
	if got != 0 {
		t.Errorf("ValidWorkloadID(\"0\", false) = %d, want 0", got)
	}
}
