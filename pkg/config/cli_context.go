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
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// The CLI's current context: the hub and project that 'scion project use'
// (alias 'scion use') selected. It lives in its own per-user file,
// ~/.scion/context.yaml, outside the settings hierarchy, so older binaries
// ignore it and the strict settings schema is untouched. Only the CLI reads
// it; agent containers and the server never do.

const (
	// CLIContextVersion is the context.yaml format version this binary
	// reads and writes.
	CLIContextVersion = 1

	cliContextFileName     = "context.yaml"
	cliContextLockFileName = "context.yaml.lock"
	cliContextFileMode     = os.FileMode(0o600)
)

// Sources of a KnownProject entry.
const (
	// KnownProjectSourceUse marks a project selected with 'scion use'.
	KnownProjectSourceUse = "use"
	// KnownProjectSourceLink marks a project recorded by 'scion hub link'.
	KnownProjectSourceLink = "link"
	// KnownProjectSourceRefresh marks a project from a hub listing.
	KnownProjectSourceRefresh = "refresh"
)

// ErrCLIContextCorrupt marks a context.yaml that cannot be read or parsed.
// Readers treat such a file as empty (after a warning); writers refuse to
// replace it unless forced.
var ErrCLIContextCorrupt = errors.New("context.yaml is not a valid context file")

// hubNameRe is the format of a hub nickname. It never contains ':' or '/',
// so a nickname can always be told apart from a URL.
var hubNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// CLIContext is the content of context.yaml.
type CLIContext struct {
	// Version is the format version. It is required.
	Version int `yaml:"version"`
	// CurrentHub is the name of the current hub entry ("" for none).
	CurrentHub string `yaml:"current_hub,omitempty"`
	// Hubs maps a hub nickname to its entry.
	Hubs map[string]*HubEntry `yaml:"hubs,omitempty"`
	// Projects is the known-project list, a cache of projects seen on hubs.
	Projects []KnownProject `yaml:"projects,omitempty"`
	// Refreshed records when each hub's project listing was last fetched.
	Refreshed map[string]time.Time `yaml:"refreshed,omitempty"`
	// Imported records one-time imports of legacy hub settings.
	Imported *CLIContextImports `yaml:"imported,omitempty"`
}

// CLIContextImports records when legacy hub settings were imported into the
// context, so each import runs once.
type CLIContextImports struct {
	// Global is when the global settings' hub configuration was imported.
	Global time.Time `yaml:"global,omitempty"`
}

// HubEntry is a named hub. It holds no credentials: logins stay in
// credentials.json, keyed by the endpoint.
type HubEntry struct {
	// Endpoint is the hub API URL.
	Endpoint string `yaml:"endpoint"`
	// CurrentProject is the project remembered for this hub.
	CurrentProject *ProjectRef `yaml:"current_project,omitempty"`
	// Transport holds the transport-layer auth settings (IAP, Cloud Run
	// invoker) for reaching this hub.
	Transport *HubTransportConfig `yaml:"transport,omitempty"`
}

// ProjectRef names a project on a hub.
type ProjectRef struct {
	// ID is the hub project UUID.
	ID string `json:"id" yaml:"id"`
	// Slug is the project slug, for display.
	Slug string `json:"slug,omitempty" yaml:"slug,omitempty"`
}

// KnownProject is one entry of the known-project list.
type KnownProject struct {
	// Hub is the nickname of the hub the project lives on.
	Hub string `yaml:"hub"`
	// ID is the hub project UUID.
	ID string `yaml:"id"`
	// Slug is the project slug.
	Slug string `yaml:"slug,omitempty"`
	// Name is the project display name.
	Name string `yaml:"name,omitempty"`
	// Source is how the entry was learned (KnownProjectSource*).
	Source string `yaml:"source,omitempty"`
	// LastUsed is when the project was last selected.
	LastUsed time.Time `yaml:"last_used,omitempty"`
	// LocalPath is the workspace directory of a linked project on this
	// machine, for link entries.
	LocalPath string `yaml:"local_path,omitempty"`
}

// CLIContextPath returns the path of context.yaml in the global directory.
func CLIContextPath() (string, error) {
	dir, err := GetGlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, cliContextFileName), nil
}

// newCLIContext returns an empty context of the current version.
func newCLIContext() *CLIContext {
	return &CLIContext{Version: CLIContextVersion}
}

// LoadCLIContext reads context.yaml without taking the lock. A missing file
// is an empty context. A file that cannot be read or parsed returns an empty
// context together with an error wrapping ErrCLIContextCorrupt: callers that
// only read the context warn and carry on. A file written by a newer version
// is returned with the fields this binary knows; UpdateCLIContext refuses to
// write it.
func LoadCLIContext() (*CLIContext, error) {
	path, err := CLIContextPath()
	if err != nil {
		return newCLIContext(), err
	}
	c, err := readCLIContextFile(path)
	if err != nil {
		return newCLIContext(), err
	}
	return c, nil
}

// readCLIContextFile reads and parses path. A missing file is an empty
// context.
func readCLIContextFile(path string) (*CLIContext, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return newCLIContext(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrCLIContextCorrupt, path, err)
	}
	c, err := parseCLIContext(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrCLIContextCorrupt, path, err)
	}
	return c, nil
}

// parseCLIContext decodes context.yaml content and checks it.
func parseCLIContext(data []byte) (*CLIContext, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if len(root.Content) == 0 {
		return nil, errors.New("file is empty")
	}
	if err := rejectHubSecrets(root.Content[0]); err != nil {
		return nil, err
	}
	c := &CLIContext{}
	if err := root.Content[0].Decode(c); err != nil {
		return nil, err
	}
	if c.Version <= 0 {
		return nil, errors.New("missing or invalid version")
	}
	for name, h := range c.Hubs {
		if h == nil {
			return nil, fmt.Errorf("hub %q has no entry", name)
		}
	}
	return c, nil
}

// rejectHubSecrets fails when a hub entry carries a key that looks like a
// credential. The context file never holds secrets: logins belong in
// credentials.json.
func rejectHubSecrets(doc *yaml.Node) error {
	if doc.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value != "hubs" || doc.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		hubs := doc.Content[i+1]
		for j := 0; j+1 < len(hubs.Content); j += 2 {
			entry := hubs.Content[j+1]
			if entry.Kind != yaml.MappingNode {
				continue
			}
			for k := 0; k < len(entry.Content); k += 2 {
				if isSecretKey(entry.Content[k].Value) {
					return fmt.Errorf("hub %q has a %q key; context.yaml must not hold credentials, log in with 'scion hub auth login' instead",
						hubs.Content[j].Value, entry.Content[k].Value)
				}
			}
		}
	}
	return nil
}

func isSecretKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range []string{"token", "secret", "password", "apikey", "api_key"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// UpdateCLIContext changes context.yaml with fn as one read-modify-write
// under an exclusive lock on context.yaml.lock. The file is re-read after
// the lock is taken, so concurrent updates from several processes are
// merged rather than lost. The new file replaces the old one atomically,
// with mode 0600. When fn returns an error nothing is written.
//
// It refuses a file written by a newer version, and a corrupt file (an
// error wrapping ErrCLIContextCorrupt); see UpdateCLIContextForce.
func UpdateCLIContext(fn func(*CLIContext) error) error {
	return updateCLIContext(fn, false)
}

// UpdateCLIContextForce is UpdateCLIContext, except that a corrupt file is
// replaced: fn starts from an empty context.
func UpdateCLIContextForce(fn func(*CLIContext) error) error {
	return updateCLIContext(fn, true)
}

func updateCLIContext(fn func(*CLIContext) error, force bool) error {
	dir, err := GetGlobalDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	unlock, err := lockCLIContext(filepath.Join(dir, cliContextLockFileName))
	if err != nil {
		return err
	}
	defer unlock()

	path := filepath.Join(dir, cliContextFileName)
	c, err := readCLIContextFile(path)
	if err != nil {
		if !force || !errors.Is(err, ErrCLIContextCorrupt) {
			return err
		}
		c = newCLIContext()
	}
	if c.Version > CLIContextVersion {
		return fmt.Errorf("context.yaml was written by a newer scion (version %d); upgrade, or remove the file", c.Version)
	}
	if err := fn(c); err != nil {
		return err
	}
	c.Version = CLIContextVersion
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode context.yaml: %w", err)
	}
	return writeFileAtomicMode(path, data, cliContextFileMode)
}

// lockCLIContext takes an exclusive advisory lock on path, which works
// across processes.
func lockCLIContext(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, cliContextFileMode)
	if err != nil {
		return nil, fmt.Errorf("open context lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock context file: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// ValidHubName reports whether name is a valid hub nickname.
func ValidHubName(name string) bool {
	return hubNameRe.MatchString(name)
}

// endpointKey returns endpoint in a form in which two spellings of one hub
// URL compare equal: spaces and trailing slashes trimmed, scheme and host
// lowercased.
func endpointKey(endpoint string) string {
	e := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	u, err := url.Parse(e)
	if err != nil || u.Host == "" {
		return e
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

// HubByEndpoint returns the name and entry of the hub with endpoint, or ""
// and nil.
func (c *CLIContext) HubByEndpoint(endpoint string) (string, *HubEntry) {
	if endpoint == "" {
		return "", nil
	}
	key := endpointKey(endpoint)
	for name, h := range c.Hubs {
		if h != nil && endpointKey(h.Endpoint) == key {
			return name, h
		}
	}
	return "", nil
}

// CurrentHubEntry returns the current hub entry, or nil.
func (c *CLIContext) CurrentHubEntry() *HubEntry {
	if c.CurrentHub == "" {
		return nil
	}
	return c.Hubs[c.CurrentHub]
}

// ProjectFor returns the project remembered for the hub with endpoint, or
// nil when that hub has no entry or no remembered project.
func (c *CLIContext) ProjectFor(endpoint string) *ProjectRef {
	_, h := c.HubByEndpoint(endpoint)
	if h == nil || h.CurrentProject == nil || h.CurrentProject.ID == "" {
		return nil
	}
	return h.CurrentProject
}

// EnsureHub returns the name of the entry for endpoint, adding one named
// baseName (with -2, -3, ... on a collision) when there is none.
func (c *CLIContext) EnsureHub(endpoint, baseName string) (name string, created bool) {
	if name, h := c.HubByEndpoint(endpoint); h != nil {
		return name, false
	}
	if !ValidHubName(baseName) {
		baseName = "hub"
	}
	name = baseName
	for i := 2; c.Hubs[name] != nil; i++ {
		suffix := "-" + strconv.Itoa(i)
		base := baseName
		if len(base)+len(suffix) > 63 {
			base = strings.TrimRight(base[:63-len(suffix)], "-")
		}
		name = base + suffix
	}
	if c.Hubs == nil {
		c.Hubs = map[string]*HubEntry{}
	}
	c.Hubs[name] = &HubEntry{Endpoint: strings.TrimSpace(endpoint)}
	return name, true
}

// DeriveHubName returns a nickname for endpoint: the first DNS label of its
// host (an IP address with '.' and ':' replaced by '-'), reduced to the
// nickname format. It returns "hub" when nothing usable is left. Callers
// name the local workstation hub "local" themselves.
func DeriveHubName(endpoint string) string {
	host := ""
	if u, err := url.Parse(strings.TrimSpace(endpoint)); err == nil {
		host = u.Hostname()
	}
	host = strings.ToLower(host)
	label := host
	if strings.ContainsAny(host, ":") || isDottedIPv4(host) {
		label = strings.NewReplacer(".", "-", ":", "-").Replace(host)
	} else if i := strings.IndexByte(host, '.'); i >= 0 {
		label = host[:i]
	}
	var b strings.Builder
	for _, r := range label {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	if !ValidHubName(name) {
		return "hub"
	}
	return name
}

func isDottedIPv4(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

// SelectProject makes hubName the current hub and p its current project,
// and records p in the known-project list with LastUsed set to now. An
// existing entry for the same hub and ID is updated in place.
func (c *CLIContext) SelectProject(hubName string, p KnownProject, now time.Time) error {
	h := c.Hubs[hubName]
	if h == nil {
		return fmt.Errorf("no hub named %q", hubName)
	}
	if p.ID == "" {
		return errors.New("project has no ID")
	}
	c.CurrentHub = hubName
	h.CurrentProject = &ProjectRef{ID: p.ID, Slug: p.Slug}

	p.Hub = hubName
	p.LastUsed = now.UTC().Truncate(time.Second)
	for i := range c.Projects {
		e := &c.Projects[i]
		if e.Hub != hubName || e.ID != p.ID {
			continue
		}
		// A linked project stays linked; anything else becomes a
		// selection, which a later refresh does not remove.
		if e.Source == KnownProjectSourceLink {
			p.Source = KnownProjectSourceLink
		} else {
			p.Source = KnownProjectSourceUse
		}
		*e = p
		return nil
	}
	p.Source = KnownProjectSourceUse
	c.Projects = append(c.Projects, p)
	return nil
}

// ClearCurrentProject forgets the current hub's project. The hub stays
// current. It returns the name of the current hub and the project that was
// cleared (nil when there was none).
func (c *CLIContext) ClearCurrentProject() (string, *ProjectRef) {
	h := c.CurrentHubEntry()
	if h == nil {
		return c.CurrentHub, nil
	}
	cleared := h.CurrentProject
	h.CurrentProject = nil
	return c.CurrentHub, cleared
}
