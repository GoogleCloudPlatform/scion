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

//go:build windows

package hubprofile

import (
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// ensurePrivateDir creates dir, which must be base or below it, and returns
// the private tree anchored at base. base is
// ~/.scion/hub-profiles. It and any missing parent are created with the
// private directory descriptor (TreeOptions.CreateAnchor); a link at or
// above base is followed, as for any tree anchor. Below base every
// component is opened relative to its parent's handle: a missing one is
// created private, an existing one is left as it is (never re-secured), and
// a reparse point (symlink, junction or other tag) is refused.
func ensurePrivateDir(base, dir string) (*privatefs.Tree, error) {
	rel, err := privateRel(base, dir)
	if err != nil {
		return nil, err
	}
	t, err := privatefs.OpenTree(base, privatefs.TreeOptions{CreateAnchor: true})
	if err != nil {
		return nil, err
	}
	if rel != "." {
		if err := t.MkdirAll(rel, privatefs.Private); err != nil {
			_ = t.Close()
			return nil, err
		}
	}
	return t, nil
}
