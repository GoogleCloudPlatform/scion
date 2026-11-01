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
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestRenderLayoutIssues is the renderer table for every view.
func TestRenderLayoutIssues(t *testing.T) {
	items := []LayoutIssue{
		mismatch("nfs server", `"10.0.0.2"`, `"10.0.0.9"`, "10.0.0.2", "10.0.0.9"),
		NewFieldIssue("server.project_storage.nfs.shares[0].id", "must be a single path element", "../x"),
		reasonIssue("nfs", "broker reports no usable share", "nfs: broker reports no usable share"),
		NewMismatchIssue("export identity marker", "broker a", "id-a", "broker b", "id-b"),
	}
	for _, tc := range []struct {
		view IssueView
		want []string
	}{
		{ViewPublic, []string{
			"nfs server: differs",
			"server.project_storage.nfs.shares[0].id: invalid",
			"nfs: broker reports no usable share",
			"export identity marker: differs",
		}},
		{ViewAdmin, []string{
			`nfs server: hub "10.0.0.2", broker "10.0.0.9"`,
			`server.project_storage.nfs.shares[0].id: must be a single path element (got "../x")`,
			"nfs: broker reports no usable share",
			`export identity marker: broker a "id-a", broker b "id-b"`,
		}},
		{ViewLog, []string{
			`nfs server: hub "10.0.0.2", broker "10.0.0.9"`,
			`server.project_storage.nfs.shares[0].id: must be a single path element (got "../x")`,
			"nfs: broker reports no usable share",
			`export identity marker: broker a "id-a", broker b "id-b"`,
		}},
	} {
		if got := RenderLayoutIssues(items, tc.view); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("view %d: got %q, want %q", tc.view, got, tc.want)
		}
	}
	if RenderLayoutIssues(nil, ViewPublic) != nil {
		t.Error("no issues should render nil")
	}

	summary := LayoutIssueSummary(items[:1], ViewPublic)
	if summary != "nfs server: differs; "+LayoutIssuesRemediation {
		t.Errorf("public summary = %q", summary)
	}
	d := LayoutIssueDetails(items, ViewPublic)
	if got := d["fields"]; !reflect.DeepEqual(got, []string{"nfs server", "server.project_storage.nfs.shares[0].id", "nfs", "export identity marker"}) {
		t.Errorf("details fields = %v", got)
	}
	if d["remediation"] != LayoutIssuesRemediation {
		t.Errorf("details remediation = %v", d["remediation"])
	}
	if _, ok := LayoutIssueDetails(items, ViewAdmin)["remediation"]; ok {
		t.Error("the admin details carry the public remediation")
	}
	for _, v := range []string{"10.0.0.2", "10.0.0.9", "../x", "id-a", "id-b"} {
		if s := fmt.Sprintf("%v %s", d, summary); strings.Contains(s, v) {
			t.Errorf("public view shows the value %q: %s", v, s)
		}
	}
}

// TestDescriptorIssues_PublicViewHasNoValues: every DescriptorDiff line
// has a public form without the values.
func TestDescriptorIssues_PublicViewHasNoValues(t *testing.T) {
	hub := Layout{Backend: BackendNFS, HostBase: "/mnt/nfs/a", SubPathRoot: "projects", Homes: BackendNFS,
		Share: Share{ID: "a", Server: "10.0.0.2", Export: "/share/a"}}
	d := &api.BrokerWorkspaceStorage{Backend: "nfs", Homes: "local", SharedDirs: "local", NFS: &api.BrokerNFSWorkspaceStorage{
		Server: "10.0.0.9", Export: "/share/b", SubPathRoot: "team", ExportID: "e1"}}
	issues := hub.DescriptorIssues(d, "e2")
	if len(issues) != 6 {
		t.Fatalf("issues = %+v", issues)
	}
	if got, want := RenderLayoutIssues(issues, ViewLog), hub.DescriptorDiff(d, "e2"); !reflect.DeepEqual(got, want) {
		t.Errorf("log view %q differs from DescriptorDiff %q", got, want)
	}
	public := strings.Join(RenderLayoutIssues(issues, ViewPublic), "; ")
	for _, v := range []string{"10.0.0.2", "10.0.0.9", "/share/a", "/share/b", "projects", "team", "e1", "e2"} {
		if strings.Contains(public, v) {
			t.Errorf("public view shows %q: %s", v, public)
		}
	}
}

func TestExportIssues(t *testing.T) {
	nfs := func(server, export, root string) *api.BrokerWorkspaceStorage {
		return &api.BrokerWorkspaceStorage{Backend: "nfs", NFS: &api.BrokerNFSWorkspaceStorage{Server: server, Export: export, SubPathRoot: root}}
	}
	if got := ExportIssues("a", nfs("s", "/e/", "p"), "b", nfs("s", "/e", "p")); got != nil {
		t.Errorf("same export: %+v", got)
	}
	got := ExportIssues("a", nfs("s1", "/e1", "p"), "b", nfs("s2", "/e2", "q"))
	if f := LayoutIssueFields(got); !reflect.DeepEqual(f, []string{"nfs server", "nfs export", "subpath_root"}) {
		t.Errorf("fields = %v", f)
	}
	if f := LayoutIssueFields(ExportIssues("a", nfs("s", "/e", "p"), "b", &api.BrokerWorkspaceStorage{Backend: "local"})); !reflect.DeepEqual(f, []string{"backend"}) {
		t.Errorf("local target fields = %v", f)
	}
	if f := LayoutIssueFields(ExportIssues("a", nfs("", "/e", "p"), "b", nfs("", "/e", "p"))); !reflect.DeepEqual(f, []string{"nfs"}) {
		t.Errorf("incomplete share fields = %v", f)
	}
}
