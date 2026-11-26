// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bundle

import "strings"

const (
	// StageInfix is the separator in sibling move-bundle staging directory
	// names: ".<base>.bundle-<32hex>".
	StageInfix = ".bundle-"
	// StageSuffixLen is the length in hex characters of the random suffix in a
	// sibling move-bundle staging directory name.
	StageSuffixLen = 32
)

// IsStageName reports whether name is a sibling staging directory for base
// (".<base>.bundle-<32hex>").
func IsStageName(name, base string) bool {
	suffix, ok := strings.CutPrefix(name, "."+base+StageInfix)
	if !ok {
		return false
	}
	return isLowerHexLen(suffix, StageSuffixLen)
}

// IsAnyStageName reports whether name is a sibling staging directory for any
// non-empty base (".<base>.bundle-<32hex>").
func IsAnyStageName(name string) bool {
	if !strings.HasPrefix(name, ".") {
		return false
	}
	idx := strings.LastIndex(name, StageInfix)
	if idx <= 1 {
		return false
	}
	return isLowerHexLen(name[idx+len(StageInfix):], StageSuffixLen)
}

func isLowerHexLen(s string, wantLen int) bool {
	if len(s) != wantLen {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
