package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// CloneDepthFull is the clone_depth value that requests a full clone
// (no --depth flag).
const CloneDepthFull = "full"

// CloneDepth is the clone_depth setting on a profile or template: "full"
// for a full clone, or a positive integer N for a clone of depth N. The
// empty value means "not set", which keeps the default shallow clone.
//
// It decodes from either a JSON/YAML string ("full", "50") or a bare
// integer (50), so `clone_depth: 50` works in YAML and JSON alike.
type CloneDepth string

// UnmarshalJSON accepts a JSON string or a JSON integer.
func (d *CloneDepth) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) {
		*d = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*d = CloneDepth(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("clone_depth: want \"full\" or a positive integer, got %s", string(b))
	}
	*d = CloneDepth(n.String())
	return nil
}

// GitDepth converts the setting to a GitCloneConfig.Depth value. ok is
// false when the setting is empty (not set). "full" yields 0 (full clone);
// a positive integer N yields N. Any other value is an error: 0 and
// negative numbers are rejected so that "full" is the only way to ask for
// a full clone.
func (d CloneDepth) GitDepth() (depth int, ok bool, err error) {
	s := strings.TrimSpace(string(d))
	if s == "" {
		return 0, false, nil
	}
	if strings.EqualFold(s, CloneDepthFull) {
		return 0, true, nil
	}
	n, convErr := strconv.Atoi(s)
	if convErr != nil || n < 1 {
		return 0, false, fmt.Errorf("invalid clone_depth %q: want %q or a positive integer", string(d), CloneDepthFull)
	}
	return n, true, nil
}

// Validate reports whether the setting is empty, "full" or a positive
// integer.
func (d CloneDepth) Validate() error {
	_, _, err := d.GitDepth()
	return err
}
