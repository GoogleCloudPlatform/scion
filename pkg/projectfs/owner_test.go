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

package projectfs

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// A shared Layout's UID and GID travel with every directory it resolves,
// as the owner a root creator gives the directories it creates.
func TestResolvedOwner(t *testing.T) {
	l := Layout{Backend: BackendNFS, HostBase: "/mnt/share-1", UID: 1000, GID: 1001}
	const id = "4f1d2c3b-5a69-4e70-8b91-a2b3c4d5e6f7"
	want := shareddirs.Owner{UID: 1000, GID: 1001}

	pr, err := l.ProjectRoot(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := pr.Owner(); got != want {
		t.Errorf("ProjectRoot owner = %+v, want %+v", got, want)
	}
	cfg, err := l.ProjectConfig(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Owner(); got != want {
		t.Errorf("ProjectConfig owner = %+v, want %+v", got, want)
	}
	sd, err := l.Resolve(Project{ID: id}, "", SharedDirMount(api.SharedDir{Name: "scratchpad"}, "/workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sd.Owner(); got != want {
		t.Errorf("shared dir owner = %+v, want %+v", got, want)
	}
	if got := (Layout{Backend: BackendNFS, HostBase: "/mnt/share-1"}).shared("projects/x", Mount{}).Owner(); got != (shareddirs.Owner{}) {
		t.Errorf("a Layout with no ids gives owner %+v, want the zero Owner", got)
	}
}
