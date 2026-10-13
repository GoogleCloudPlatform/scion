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

package config

import (
	"reflect"
	"testing"
	"time"
)

// knownTestContext has two hubs, each with a project slugged "foo", plus
// a project on a hub that has no entry any more.
func knownTestContext() *CLIContext {
	return &CLIContext{
		Version: CLIContextVersion,
		Hubs: map[string]*HubEntry{
			"prod":    {Endpoint: "https://hub.example.com"},
			"staging": {Endpoint: "https://staging.example.com"},
		},
		Projects: []KnownProject{
			{Hub: "prod", ID: "p-foo", Slug: "foo", Name: "Foo", Source: KnownProjectSourceUse},
			{Hub: "prod", ID: "p-web", Slug: "web", Name: "Web frontend", Source: KnownProjectSourceRefresh},
			{Hub: "staging", ID: "s-foo", Slug: "foo", Name: "Foo", Source: KnownProjectSourceLink, LocalPath: "/src/foo"},
			{Hub: "gone", ID: "g-foo", Slug: "foo", Source: KnownProjectSourceUse},
		},
	}
}

func knownIDs(ps []KnownProject) []string {
	var ids []string
	for _, p := range ps {
		ids = append(ids, p.Hub+"/"+p.ID)
	}
	return ids
}

func TestMatchKnownProjects(t *testing.T) {
	c := knownTestContext()
	tests := []struct {
		name string
		ref  string
		hubs map[string]bool
		want []string
	}{
		{"slug on two hubs, the hub without an entry skipped", "foo", nil, []string{"prod/p-foo", "staging/s-foo"}},
		{"restricted to one hub", "foo", map[string]bool{"staging": true}, []string{"staging/s-foo"}},
		{"by name", "Web frontend", nil, []string{"prod/p-web"}},
		{"by ID", "s-foo", nil, []string{"staging/s-foo"}},
		{"no partial match", "fo", nil, nil},
		{"empty ref", "", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := knownIDs(c.MatchKnownProjects(tt.ref, tt.hubs)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("MatchKnownProjects(%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

func TestMatchKnownProjects_DuplicateEntryOnce(t *testing.T) {
	c := knownTestContext()
	c.Projects = append(c.Projects, KnownProject{Hub: "prod", ID: "p-foo", Slug: "foo", Source: KnownProjectSourceLink})
	if got := knownIDs(c.MatchKnownProjects("foo", map[string]bool{"prod": true})); !reflect.DeepEqual(got, []string{"prod/p-foo"}) {
		t.Errorf("got %v, want one entry", got)
	}
}

func TestDropKnownProject(t *testing.T) {
	c := knownTestContext()
	c.Hubs["prod"].CurrentProject = &ProjectRef{ID: "p-foo", Slug: "foo"}
	if !c.DropKnownProject("prod", "p-foo") {
		t.Fatal("DropKnownProject reported nothing removed")
	}
	if got := knownIDs(c.Projects); !reflect.DeepEqual(got, []string{"prod/p-web", "staging/s-foo", "gone/g-foo"}) {
		t.Errorf("after drop: %v", got)
	}
	if c.Hubs["prod"].CurrentProject == nil {
		t.Error("the hub's current project must be left alone")
	}
	if c.DropKnownProject("prod", "p-foo") {
		t.Error("a second drop reported a removal")
	}
}

func TestRememberProjects(t *testing.T) {
	c := knownTestContext()
	used := time.Date(2026, 10, 11, 19, 2, 11, 0, time.UTC)
	c.Projects[0].LastUsed = used
	c.RememberProjects("prod", []KnownProject{
		{ID: "p-foo", Slug: "foo", Name: "Foo renamed"},
		{ID: "p-web", Slug: "web", Name: "Web"},
		{ID: "p-new", Slug: "new", Name: "New"},
		{ID: "", Slug: "no-id"},
	})
	c.RememberProjects("staging", []KnownProject{{ID: "s-foo", Slug: "foo", Name: "Foo 2"}})

	want := []KnownProject{
		{Hub: "prod", ID: "p-foo", Slug: "foo", Name: "Foo renamed", Source: KnownProjectSourceUse, LastUsed: used},
		{Hub: "prod", ID: "p-web", Slug: "web", Name: "Web", Source: KnownProjectSourceUse},
		{Hub: "staging", ID: "s-foo", Slug: "foo", Name: "Foo 2", Source: KnownProjectSourceLink, LocalPath: "/src/foo"},
		{Hub: "gone", ID: "g-foo", Slug: "foo", Source: KnownProjectSourceUse},
		{Hub: "prod", ID: "p-new", Slug: "new", Name: "New", Source: KnownProjectSourceUse},
	}
	if !reflect.DeepEqual(c.Projects, want) {
		t.Errorf("Projects =\n%+v\nwant\n%+v", c.Projects, want)
	}
}

func TestReplaceRefreshed(t *testing.T) {
	c := knownTestContext()
	c.Projects = append(c.Projects,
		KnownProject{Hub: "prod", ID: "p-old", Slug: "old", Source: KnownProjectSourceRefresh},
		KnownProject{Hub: "staging", ID: "s-ref", Slug: "ref", Source: KnownProjectSourceRefresh},
	)
	now := time.Date(2026, 10, 12, 9, 30, 15, 500, time.FixedZone("x", 3600))
	// prod no longer lists p-foo (a selection), p-web or p-old (refresh
	// entries); it lists p-api, and p-foo is gone from the listing.
	c.ReplaceRefreshed("prod", []KnownProject{
		{ID: "p-api", Slug: "api", Name: "API"},
		{ID: "", Slug: "no-id"},
	}, now)
	// staging lists its linked project under a new name.
	c.ReplaceRefreshed("staging", []KnownProject{{ID: "s-foo", Slug: "foo", Name: "Foo (staging)"}}, now)

	want := []KnownProject{
		{Hub: "prod", ID: "p-foo", Slug: "foo", Name: "Foo", Source: KnownProjectSourceUse},
		{Hub: "staging", ID: "s-foo", Slug: "foo", Name: "Foo (staging)", Source: KnownProjectSourceLink, LocalPath: "/src/foo"},
		{Hub: "gone", ID: "g-foo", Slug: "foo", Source: KnownProjectSourceUse},
		{Hub: "prod", ID: "p-api", Slug: "api", Name: "API", Source: KnownProjectSourceRefresh},
	}
	if !reflect.DeepEqual(c.Projects, want) {
		t.Errorf("Projects =\n%+v\nwant\n%+v", c.Projects, want)
	}
	wantTime := time.Date(2026, 10, 12, 8, 30, 15, 0, time.UTC)
	if got := c.Refreshed["prod"]; !got.Equal(wantTime) || got.Location() != time.UTC {
		t.Errorf("Refreshed[prod] = %v, want %v in UTC", got, wantTime)
	}
}

func TestReplaceRefreshed_SavedAndReloaded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	err := UpdateCLIContext(func(c *CLIContext) error {
		c.Hubs = map[string]*HubEntry{"prod": {Endpoint: "https://hub.example.com"}}
		c.ReplaceRefreshed("prod", []KnownProject{{ID: "p-1", Slug: "one", Name: "One"}}, now)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := LoadCLIContext()
	if err != nil {
		t.Fatal(err)
	}
	want := []KnownProject{{Hub: "prod", ID: "p-1", Slug: "one", Name: "One", Source: KnownProjectSourceRefresh}}
	if !reflect.DeepEqual(c.Projects, want) {
		t.Errorf("Projects = %+v, want %+v", c.Projects, want)
	}
	if !c.Refreshed["prod"].Equal(now) {
		t.Errorf("Refreshed[prod] = %v, want %v", c.Refreshed["prod"], now)
	}
}
