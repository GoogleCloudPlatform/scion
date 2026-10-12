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

package cmd

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Link entries and the local hub in the hub list (context.yaml). A linked
// project records its hub as a link entry instead of a hub.endpoint in its
// settings, and the workstation server registers itself as the hub
// "local" instead of writing the global hub.endpoint.

// localHubName is the nickname of the local workstation hub.
const localHubName = "local"

// recordLinkEntry records in the hub list that the project at projectDir
// is linked to the hub project hubProjectID on the hub at endpoint. The
// hub gets an entry when it has none, and the new entry is announced on
// out. A failure is a warning on stderr: the link itself succeeded. Inside
// an agent the hub list is not used, and nothing is recorded.
func recordLinkEntry(out, stderr io.Writer, endpoint, hubProjectID, slug, name, projectDir string) {
	if contextDisabled(resolveMode()) || endpoint == "" || hubProjectID == "" {
		return
	}
	localPath := config.LinkPathForProject(projectDir)
	var hubName string
	var created bool
	err := config.UpdateCLIContext(func(c *config.CLIContext) error {
		hubName, created = c.EnsureHub(endpoint, useHubBaseName(endpoint))
		return c.SetLinkEntry(hubName, config.KnownProject{ID: hubProjectID, Slug: slug, Name: name}, localPath, time.Now())
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "Warning: could not record the link in the hub list: %v\n", err)
		return
	}
	if created {
		printRegisteredHub(out, hubName, config.NormalizeEndpoint(endpoint))
	}
}

// errNoLinkEntry stops the context update of removeLinkEntry when there is
// no link entry to remove, so the file is not rewritten.
var errNoLinkEntry = errors.New("no link entry")

// removeLinkEntry removes the link entry of the project at projectDir from
// the hub list. The current context is not changed. A failure is a warning
// on stderr.
func removeLinkEntry(stderr io.Writer, projectDir string) {
	if contextDisabled(resolveMode()) {
		return
	}
	localPath := config.LinkPathForProject(projectDir)
	if c, err := config.LoadCLIContext(); err == nil && !hasLinkEntry(c, localPath) {
		return
	}
	err := config.UpdateCLIContext(func(c *config.CLIContext) error {
		if !c.RemoveLinkEntry(localPath) {
			return errNoLinkEntry
		}
		return nil
	})
	if err != nil && !errors.Is(err, errNoLinkEntry) {
		_, _ = fmt.Fprintf(stderr, "Warning: could not remove the link from the hub list: %v\n", err)
	}
}

// hasLinkEntry reports whether the known list has a link entry for
// localPath, also one whose hub has no entry any more.
func hasLinkEntry(c *config.CLIContext, localPath string) bool {
	for _, p := range c.Projects {
		if p.Source == config.KnownProjectSourceLink && p.LocalPath == localPath {
			return true
		}
	}
	return false
}

// contextLinkRecorder returns the hubsync RecordLink callback for this
// invocation: when the hub came from the current context, a project that
// EnsureHubReady registers or links there gets a link entry, so it keeps
// resolving to that hub after a hub switch. It returns nil otherwise.
func contextLinkRecorder(out, stderr io.Writer) func(endpoint, hubProjectID, projectPath string, isGlobal bool) {
	if hubSource != targetSourceContext {
		return nil
	}
	return func(endpoint, hubProjectID, projectPath string, _ bool) {
		recordLinkEntry(out, stderr, endpoint, hubProjectID, "", "", projectPath)
	}
}

// registerLocalHub adds the local workstation hub at endpoint to the hub
// list as "local" when no entry has that endpoint. It makes the hub at
// endpoint current only when no hub is current and the global settings
// name no other hub (hub.endpoint unset, or the local endpoint itself).
// While the CLI still reads hub.endpoint from the global settings, that
// keeps a user whose global hub.endpoint names a remote hub on that hub
// outside project directories, as before server start stopped writing
// hub.endpoint (it only ever wrote it when unset). A new entry is
// announced on out. A failure is a warning on stderr. Inside an agent the
// hub list is not used.
func registerLocalHub(out, stderr io.Writer, endpoint string) {
	if contextDisabled(resolveMode()) {
		return
	}
	globalEndpoint, _ := fileHubEndpoint("", true)
	mayBecomeCurrent := globalEndpoint == "" || config.NormalizeEndpoint(globalEndpoint) == config.NormalizeEndpoint(endpoint)
	var name string
	var created, changed bool
	err := config.UpdateCLIContext(func(c *config.CLIContext) error {
		name, created = c.EnsureHub(endpoint, localHubName)
		changed = created
		if c.CurrentHub == "" && mayBecomeCurrent {
			c.CurrentHub = name
			changed = true
		}
		if !changed {
			return errLocalHubKnown
		}
		return nil
	})
	switch {
	case errors.Is(err, errLocalHubKnown):
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "Warning: could not add the local hub to the hub list: %v\n", err)
	case created:
		printRegisteredHub(out, name, config.NormalizeEndpoint(endpoint))
	}
}

// errLocalHubKnown stops the context update of registerLocalHub when
// there is nothing to change, so the file is not rewritten.
var errLocalHubKnown = errors.New("local hub already registered")
