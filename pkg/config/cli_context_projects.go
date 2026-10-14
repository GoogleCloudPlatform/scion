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

import "time"

// The known-project list is a cache of projects seen on hubs; the hubs are
// authoritative. These helpers keep it consistent: one entry per hub and
// project ID, refresh entries replaced per hub, and selection and link
// entries kept until a lookup on their hub proves them gone.

// MatchKnownProjects returns the known-project entries that ref names
// exactly, by ID, slug or name. When hubs is not nil, only entries on the
// hubs it contains are returned. Entries whose hub has no hub entry are
// skipped. Each hub and project ID appears once.
func (c *CLIContext) MatchKnownProjects(ref string, hubs map[string]bool) []KnownProject {
	if ref == "" {
		return nil
	}
	var out []KnownProject
	seen := map[[2]string]bool{}
	for _, p := range c.Projects {
		if p.ID == "" || c.Hubs[p.Hub] == nil || (hubs != nil && !hubs[p.Hub]) {
			continue
		}
		if p.ID != ref && p.Slug != ref && p.Name != ref {
			continue
		}
		key := [2]string{p.Hub, p.ID}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// DropKnownProject removes every known-project entry for the project id on
// hub, whatever its source. It reports whether anything was removed. The
// hub's current project is left alone.
func (c *CLIContext) DropKnownProject(hub, id string) bool {
	kept := c.Projects[:0]
	dropped := false
	for _, p := range c.Projects {
		if p.Hub == hub && p.ID == id {
			dropped = true
			continue
		}
		kept = append(kept, p)
	}
	c.Projects = kept
	if len(c.Projects) == 0 {
		c.Projects = nil
	}
	return dropped
}

// RememberProjects merges projects that a lookup found on hub into the
// known-project list. A project without an entry gets one with source
// "use". An existing entry gets the slug and name from the hub and keeps
// its last use; a refresh entry becomes a "use" entry, so a later refresh
// does not remove it, and a link entry stays a link entry.
func (c *CLIContext) RememberProjects(hub string, projects []KnownProject) {
	for _, p := range projects {
		if p.ID == "" {
			continue
		}
		if e := c.knownProject(hub, p.ID); e != nil {
			e.Slug, e.Name = p.Slug, p.Name
			if e.Source == KnownProjectSourceRefresh {
				e.Source = KnownProjectSourceUse
			}
			continue
		}
		c.Projects = append(c.Projects, KnownProject{
			Hub: hub, ID: p.ID, Slug: p.Slug, Name: p.Name, Source: KnownProjectSourceUse,
		})
	}
}

// ReplaceRefreshed stores a complete project listing of hub. The hub's
// earlier refresh entries are replaced by projects; a listed project that
// already has a "use" or "link" entry updates that entry's slug and name
// instead, and such entries stay even when the listing does not contain
// them. It records now as the hub's refresh time.
func (c *CLIContext) ReplaceRefreshed(hub string, projects []KnownProject, now time.Time) {
	kept := c.Projects[:0]
	for _, p := range c.Projects {
		if p.Hub == hub && p.Source == KnownProjectSourceRefresh {
			continue
		}
		kept = append(kept, p)
	}
	c.Projects = kept
	for _, p := range projects {
		if p.ID == "" {
			continue
		}
		if e := c.knownProject(hub, p.ID); e != nil {
			e.Slug, e.Name = p.Slug, p.Name
			continue
		}
		c.Projects = append(c.Projects, KnownProject{
			Hub: hub, ID: p.ID, Slug: p.Slug, Name: p.Name, Source: KnownProjectSourceRefresh,
		})
	}
	if c.Refreshed == nil {
		c.Refreshed = map[string]time.Time{}
	}
	c.Refreshed[hub] = now.UTC().Truncate(time.Second)
}

// knownProject returns the first known-project entry for id on hub, or nil.
func (c *CLIContext) knownProject(hub, id string) *KnownProject {
	for i := range c.Projects {
		if c.Projects[i].Hub == hub && c.Projects[i].ID == id {
			return &c.Projects[i]
		}
	}
	return nil
}
