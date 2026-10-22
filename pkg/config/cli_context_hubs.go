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
	"regexp"
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
		return fmt.Errorf("invalid hub URL %q: use an http:// or https:// URL", RedactURL(endpoint))
	}
	return nil
}

// userInfoRe matches the user info of a URL that url.Parse rejects, up to
// the last '@' before the host.
// pkg/apiclient keeps a copy of these rules (redactURLCredentials), since it
// imports no scion package; change both together.
var userInfoRe = regexp.MustCompile("^(" + urlSchemePrefix + ")?[^/?#]*@")

// textURLRe matches the scheme and authority of each http(s) URL in a
// text. The authority ends at the path, query or fragment, at white space,
// or at a character that delimits a URL in a message and cannot appear in
// user info.
var textURLRe = regexp.MustCompile("(?i)\\bhttps?://(?:[^\\s/?#\"<>`\\\\]|\\\\.)*")

// textFullURLRe matches each http(s) URL in a text: textURLRe's scheme and
// authority, then its path, query and fragment, which end at white space,
// a quote, an angle bracket, a backtick or a backslash. A JSON escape
// inside them (\u0026 for "&", \/ for "/") does not end the URL, so a URL
// quoted in JSON text is redacted whole; any other backslash (\" in a Go
// %q string, for example) ends it. The path, query and fragment start at a
// literal "/", "?" or "#" (encoding/json never escapes those).
var textFullURLRe = regexp.MustCompile(textURLRe.String() + "(?:[/?#](?:[^\\s\"<>`\\\\]|\\\\u[0-9a-fA-F]{4}|\\\\/)*)?")

// RedactURL returns raw for display with any credential in it replaced:
// in its user info, the password (see url.URL.Redacted), or a user name
// given without a password, which may itself be a token; in its query and
// fragment, the value of every parameter with a credential-like name (see
// isURLCredentialParam). A URL that does not parse has its whole user info
// replaced. A URL with nothing to mask is returned as given.
func RedactURL(raw string) string {
	s := raw
	if u, err := url.Parse(raw); err == nil {
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				s = u.Redacted()
			} else {
				u.User = url.User("xxxxx")
				s = u.String()
			}
		}
	} else {
		s = userInfoRe.ReplaceAllString(raw, "${1}xxxxx@")
	}
	return redactURLParams(s)
}

// RedactURLsIn returns text (for example an error message) with every
// http(s) URL in it redacted as RedactURL does: its user info, and the
// credential parameters of its query and fragment.
func RedactURLsIn(text string) string {
	return textFullURLRe.ReplaceAllStringFunc(text, func(m string) string {
		authority := textURLRe.FindString(m)
		rest := m[len(authority):]
		// Punctuation that ends a sentence is not part of the URL.
		trimmed := strings.TrimRight(rest, ".,;:!?)]}'")
		tail := rest[len(trimmed):]
		if strings.Contains(authority, "@") {
			authority = RedactURL(authority)
		}
		return authority + redactURLParams(trimmed) + tail
	})
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
// afterwards. It returns the removed entry and whether it was current. The
// path of each link entry it drops is marked as imported (see
// LinkEntryForProject), so the one-time import of that project's settings
// does not link it again.
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
	var dropped []string
	kept := c.Projects[:0]
	for _, p := range c.Projects {
		if p.Hub != name {
			kept = append(kept, p)
		} else if p.Source == KnownProjectSourceLink && p.LocalPath != "" {
			dropped = append(dropped, p.LocalPath)
		}
	}
	for _, path := range dropped {
		c.markProjectImported(path, time.Now())
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
// records the import time. It returns the entry's name. It does not make
// the entry the current hub: ImportCurrentHub does that, once.
func (c *CLIContext) ImportGlobalHub(endpoint string, transport *HubTransportConfig, baseName string, now time.Time) string {
	name, _ := c.EnsureHub(endpoint, baseName)
	setTransportIfUnset(c.Hubs[name], transport)
	if c.Imported == nil {
		c.Imported = &CLIContextImports{}
	}
	c.Imported.Global = now.UTC().Truncate(time.Second)
	return name
}

// setTransportIfUnset copies transport onto h when h has no transport and
// transport sets a mode or an audience.
func setTransportIfUnset(h *HubEntry, transport *HubTransportConfig) {
	if h == nil || h.Transport != nil || transport == nil || (transport.Mode == "" && transport.Audience == "") {
		return
	}
	t := *transport
	h.Transport = &t
}

// CurrentImported reports whether the current hub was already imported
// from the global settings (see ImportCurrentHub).
func (c *CLIContext) CurrentImported() bool {
	return c.Imported != nil && !c.Imported.Current.IsZero()
}

// ImportCurrentHub makes the global settings' hub.endpoint the current hub
// when no hub is current, adding an entry for it (named baseName,
// de-duplicated) when none has that endpoint, copies the global
// hub.transport onto that entry when it has none, and records that this
// ran.
// Until this step the CLI followed the global hub.endpoint outside
// projects, so it keeps the same target: also when an earlier binary
// imported another endpoint, or the user removed the imported entry. It
// runs once, so a current hub that is cleared or removed later is not set
// again.
func (c *CLIContext) ImportCurrentHub(endpoint string, transport *HubTransportConfig, baseName string, now time.Time) {
	if c.CurrentHub == "" {
		c.CurrentHub, _ = c.EnsureHub(endpoint, baseName)
		setTransportIfUnset(c.Hubs[c.CurrentHub], transport)
	}
	if c.Imported == nil {
		c.Imported = &CLIContextImports{}
	}
	c.Imported.Current = now.UTC().Truncate(time.Second)
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
// global settings' hub configuration into the hub list. The CLI reads no
// hub setting itself; this import is how an existing hub.endpoint carries
// over. It runs when the global settings file has an endpoint that is a
// valid hub URL (ValidateHubURL) and one of its two steps has not run yet:
//
//   - the global import: hub.endpoint (and hub.transport) become a hub
//     entry; baseName names a new entry (callers name the local
//     workstation hub "local");
//   - the current-hub import: the global hub.endpoint becomes the current
//     hub when no hub is current, with an entry added for it if none has
//     that endpoint (ImportCurrentHub).
//
// Both are written together under the lock, once. Without a usable
// endpoint nothing is written, and the import stays pending. No settings
// file is changed.
//
// A file that cannot be read returns the same error as LoadCLIContext, and
// a file written by a newer version is returned without the import. When
// the import cannot be saved, the returned context holds it anyway, with an
// error wrapping ErrCLIContextImport.
func LoadCLIContextImported(baseName func(endpoint string) string) (*CLIContext, error) {
	return loadCLIContextImported(baseName, true)
}

// LoadCLIContextImportedInMemory is LoadCLIContextImported without the
// save: the returned context holds the pending one-time import, and
// nothing is written (no context.yaml, no lock). Read-only commands use
// it, so they see the same hub as the next command that saves the import
// (ptone/scion#5483).
func LoadCLIContextImportedInMemory(baseName func(endpoint string) string) (*CLIContext, error) {
	return loadCLIContextImported(baseName, false)
}

func loadCLIContextImported(baseName func(endpoint string) string, save bool) (*CLIContext, error) {
	c, err := LoadCLIContext()
	if err != nil || c.Version > CLIContextVersion || (c.GlobalImported() && c.CurrentImported()) {
		return c, err
	}
	endpoint, transport := globalHubSettings()
	// An endpoint that is not a valid hub URL (for example one carrying
	// user info) is not imported, and the import stays pending.
	if endpoint == "" || ValidateHubURL(endpoint) != nil {
		return c, nil
	}
	importInto := func(target *CLIContext, now time.Time) {
		if !target.GlobalImported() {
			target.ImportGlobalHub(endpoint, transport, baseName(endpoint), now)
		}
		if !target.CurrentImported() {
			target.ImportCurrentHub(endpoint, transport, baseName(endpoint), now)
		}
	}
	if !save {
		importInto(c, time.Now())
		return c, nil
	}
	var imported *CLIContext
	err = UpdateCLIContext(func(fresh *CLIContext) error {
		importInto(fresh, time.Now())
		imported = fresh
		return nil
	})
	if err != nil {
		importInto(c, time.Now())
		return c, fmt.Errorf("%w: %w", ErrCLIContextImport, err)
	}
	return imported, nil
}
