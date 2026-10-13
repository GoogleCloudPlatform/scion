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
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The hub registry: the named hub entries of context.yaml, managed with
// 'scion hub add|list|rename|remove'. There is one entry per normalised
// endpoint, because the endpoint is how an entry finds its login.

// ErrCLIContextImport marks a failure to save the one-time import of the
// global settings' hub configuration. The context returned with it already
// holds the import, so callers can warn and carry on.
var ErrCLIContextImport = errors.New("could not save the imported hub settings to context.yaml")

// HubNames returns the names of the hub entries, sorted.
func (c *CLIContext) HubNames() []string {
	names := make([]string, 0, len(c.Hubs))
	for name := range c.Hubs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ValidateHubName returns an error when name is not a valid hub nickname.
func ValidateHubName(name string) error {
	if !ValidHubName(name) {
		return fmt.Errorf("invalid hub name %q: use lowercase letters, digits and '-', starting with a letter or digit, at most 63 characters", name)
	}
	return nil
}

// ValidateHubURL returns an error when endpoint is not an http or https URL
// with a host. User info, a query and a fragment are refused too: the hub
// list holds no credentials, and a hub URL needs neither.
func ValidateHubURL(endpoint string) error {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(endpoint, "#") {
		return fmt.Errorf("invalid hub URL %q: use an http:// or https:// URL", endpoint)
	}
	return nil
}

// AddHub adds a hub entry named name for endpoint. The name and the URL
// must be valid, the name must be new, and no other entry may have the
// same normalised endpoint.
func (c *CLIContext) AddHub(name, endpoint string) error {
	if err := ValidateHubName(name); err != nil {
		return err
	}
	if err := ValidateHubURL(endpoint); err != nil {
		return err
	}
	if h := c.Hubs[name]; h != nil {
		return fmt.Errorf("a hub named %q already exists (%s)", name, h.Endpoint)
	}
	if existing, _ := c.HubByEndpoint(endpoint); existing != "" {
		return fmt.Errorf("%s is already registered as hub %q; rename it with 'scion hub rename %s <name>'", NormalizeEndpoint(endpoint), existing, existing)
	}
	if c.Hubs == nil {
		c.Hubs = map[string]*HubEntry{}
	}
	c.Hubs[name] = &HubEntry{Endpoint: NormalizeEndpoint(endpoint)}
	return nil
}

// RenameHub renames the hub entry oldName to newName and rewrites every
// reference to it: the current hub, the known projects and the refresh
// times.
func (c *CLIContext) RenameHub(oldName, newName string) error {
	h := c.Hubs[oldName]
	if h == nil {
		return fmt.Errorf("no hub named %q", oldName)
	}
	if err := ValidateHubName(newName); err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}
	if other := c.Hubs[newName]; other != nil {
		return fmt.Errorf("a hub named %q already exists (%s)", newName, other.Endpoint)
	}
	delete(c.Hubs, oldName)
	c.Hubs[newName] = h
	if c.CurrentHub == oldName {
		c.CurrentHub = newName
	}
	for i := range c.Projects {
		if c.Projects[i].Hub == oldName {
			c.Projects[i].Hub = newName
		}
	}
	if t, ok := c.Refreshed[oldName]; ok {
		delete(c.Refreshed, oldName)
		c.Refreshed[newName] = t
	}
	return nil
}

// RemoveHub removes the hub entry name together with its known projects
// and refresh time. When it was the current hub, there is no current hub
// afterwards. It returns the removed entry and whether it was current.
func (c *CLIContext) RemoveHub(name string) (*HubEntry, bool, error) {
	h := c.Hubs[name]
	if h == nil {
		return nil, false, fmt.Errorf("no hub named %q", name)
	}
	delete(c.Hubs, name)
	wasCurrent := c.CurrentHub == name
	if wasCurrent {
		c.CurrentHub = ""
	}
	kept := c.Projects[:0]
	for _, p := range c.Projects {
		if p.Hub != name {
			kept = append(kept, p)
		}
	}
	c.Projects = kept
	if len(c.Projects) == 0 {
		c.Projects = nil
	}
	delete(c.Refreshed, name)
	return h, wasCurrent, nil
}

// LookupHub returns the entry for nameOrURL: by name for a nickname, by
// normalised endpoint for a URL. It returns "" and nil when there is none.
func (c *CLIContext) LookupHub(nameOrURL string) (string, *HubEntry) {
	if strings.Contains(nameOrURL, "://") {
		return c.HubByEndpoint(nameOrURL)
	}
	if h := c.Hubs[nameOrURL]; h != nil {
		return nameOrURL, h
	}
	return "", nil
}

// GlobalImported reports whether the global settings' hub configuration
// was already imported.
func (c *CLIContext) GlobalImported() bool {
	return c.Imported != nil && !c.Imported.Global.IsZero()
}

// ImportGlobalHub records the global settings' hub endpoint as a hub entry
// (named baseName, de-duplicated, unless an entry already has that
// endpoint), copies transport onto the entry when the entry has none, and
// records the import time. It returns the entry's name.
//
// It does not make the entry the current hub. While the CLI still reads
// hub.endpoint from settings, a user who never selected a hub keeps
// following the global hub.endpoint outside projects, including later
// changes to it. Only an explicit selection sets the current hub.
func (c *CLIContext) ImportGlobalHub(endpoint string, transport *HubTransportConfig, baseName string, now time.Time) string {
	name, _ := c.EnsureHub(endpoint, baseName)
	if h := c.Hubs[name]; transport != nil && h.Transport == nil && (transport.Mode != "" || transport.Audience != "") {
		t := *transport
		h.Transport = &t
	}
	if c.Imported == nil {
		c.Imported = &CLIContextImports{}
	}
	c.Imported.Global = now.UTC().Truncate(time.Second)
	return name
}

// globalHubSettings returns hub.endpoint and hub.transport from the global
// settings file. Environment variables are not applied: only what is
// written in the file is imported.
var globalHubSettings = func() (string, *HubTransportConfig) {
	dir, err := GetGlobalDir()
	if err != nil {
		return "", nil
	}
	s, err := LoadSettingsFromDir(dir)
	if err != nil || s == nil || s.Hub == nil {
		return "", nil
	}
	return strings.TrimSpace(s.Hub.Endpoint), s.Hub.Transport
}

// LoadCLIContextImported is LoadCLIContext plus the one-time import of the
// global settings' hub.endpoint (and hub.transport) into the hub list. The
// import runs when it has not run before and the global settings file has
// an endpoint that is a valid hub URL (ValidateHubURL); it is written under
// the lock, once. baseName names the new
// entry (callers name the local workstation hub "local"). The import does
// not set the current hub (see ImportGlobalHub). No settings file is
// changed.
//
// A file that cannot be read returns the same error as LoadCLIContext, and
// a file written by a newer version is returned without the import. When
// the import cannot be saved, the returned context holds it anyway, with an
// error wrapping ErrCLIContextImport.
func LoadCLIContextImported(baseName func(endpoint string) string) (*CLIContext, error) {
	c, err := LoadCLIContext()
	if err != nil || c.GlobalImported() || c.Version > CLIContextVersion {
		return c, err
	}
	endpoint, transport := globalHubSettings()
	// An endpoint that is not a valid hub URL (for example one carrying
	// user info) is not imported, and the import stays pending.
	if endpoint == "" || ValidateHubURL(endpoint) != nil {
		return c, nil
	}
	var imported *CLIContext
	err = UpdateCLIContext(func(fresh *CLIContext) error {
		if !fresh.GlobalImported() {
			fresh.ImportGlobalHub(endpoint, transport, baseName(endpoint), time.Now())
		}
		imported = fresh
		return nil
	})
	if err != nil {
		c.ImportGlobalHub(endpoint, transport, baseName(endpoint), time.Now())
		return c, fmt.Errorf("%w: %w", ErrCLIContextImport, err)
	}
	return imported, nil
}
