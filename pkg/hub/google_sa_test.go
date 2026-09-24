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

package hub

import "testing"

// S5: googleSAProject table test — every row of design §4.5, plus
// case-folding. Each case pins both the returned project ID and the ok
// bool, so a mutation that silently returns ("", true) or drops a rejection
// branch fails here rather than only at the (later) middleware level.
func TestGoogleSAProject(t *testing.T) {
	tests := []struct {
		name        string
		email       string
		wantProject string
		wantOK      bool
	}{
		{
			name:        "standard SA email",
			email:       "myworker@my-a2a-project.iam.gserviceaccount.com",
			wantProject: "my-a2a-project",
			wantOK:      true,
		},
		{
			name:        "standard SA email, project with hyphens and digits",
			email:       "svc@proj-123-abc.iam.gserviceaccount.com",
			wantProject: "proj-123-abc",
			wantOK:      true,
		},
		{
			name:        "standard SA email, mixed case folds to lower",
			email:       "MyWorker@My-A2A-Project.IAM.GSERVICEACCOUNT.COM",
			wantProject: "my-a2a-project",
			wantOK:      true,
		},
		{
			name:        "appspot default SA",
			email:       "my-a2a-project@appspot.gserviceaccount.com",
			wantProject: "my-a2a-project",
			wantOK:      true,
		},
		{
			name:        "appspot default SA, mixed case folds to lower",
			email:       "My-A2A-Project@Appspot.Gserviceaccount.com",
			wantProject: "my-a2a-project",
			wantOK:      true,
		},
		{
			name:   "Google service agent (gcp-sa-*) is rejected explicitly",
			email:  "service-123456789@gcp-sa-pubsub.iam.gserviceaccount.com",
			wantOK: false,
		},
		{
			name:   "Google service agent, mixed case, still rejected",
			email:  "service-123@GCP-SA-Something.IAM.Gserviceaccount.com",
			wantOK: false,
		},
		{
			name:   "compute default SA carries only project number",
			email:  "123456789012-compute@developer.gserviceaccount.com",
			wantOK: false,
		},
		{
			name:   "developer.gserviceaccount.com, mixed case",
			email:  "123456789012-compute@Developer.Gserviceaccount.com",
			wantOK: false,
		},
		{
			name:   "system.gserviceaccount.com",
			email:  "kube-system@system.gserviceaccount.com",
			wantOK: false,
		},
		{
			name:   "system.gserviceaccount.com, mixed case",
			email:  "kube-system@System.GServiceAccount.com",
			wantOK: false,
		},
		{
			name:   "not a service account email at all",
			email:  "alice@example.com",
			wantOK: false,
		},
		{
			name:   "gmail address",
			email:  "alice@gmail.com",
			wantOK: false,
		},
		{
			name:   "bare iam.gserviceaccount.com with no project label",
			email:  "name@iam.gserviceaccount.com",
			wantOK: false,
		},
		{
			// Unlike the "bare" case above (whose domain doesn't even end in
			// ".iam.gserviceaccount.com", so it takes the default arm), this
			// domain IS ".iam.gserviceaccount.com" exactly, so
			// strings.TrimSuffix leaves an empty project label — the
			// project == "" guard inside the .iam. branch itself (S5g, P3
			// fix round 1 Nit 4).
			name:   "empty project label before .iam.gserviceaccount.com",
			email:  "sa@.iam.gserviceaccount.com",
			wantOK: false,
		},
		{
			name:   "appspot with empty local part",
			email:  "@appspot.gserviceaccount.com",
			wantOK: false,
		},
		{
			name:   "no @ at all",
			email:  "not-an-email",
			wantOK: false,
		},
		{
			name:   "empty string",
			email:  "",
			wantOK: false,
		},
		{
			name:   "trailing @ with nothing after",
			email:  "name@",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotProject, gotOK := googleSAProject(tt.email)
			if gotOK != tt.wantOK {
				t.Fatalf("googleSAProject(%q) ok = %v, want %v (project = %q)", tt.email, gotOK, tt.wantOK, gotProject)
			}
			if gotOK && gotProject != tt.wantProject {
				t.Fatalf("googleSAProject(%q) project = %q, want %q", tt.email, gotProject, tt.wantProject)
			}
			if !gotOK && gotProject != "" {
				t.Fatalf("googleSAProject(%q) project = %q on a false result, want empty", tt.email, gotProject)
			}
		})
	}
}
