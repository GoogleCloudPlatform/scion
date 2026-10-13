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

// Package hubprofile implements CLI target-hub profiles (ptone/scion#4240):
// named, non-secret descriptions of another test hub, used to issue and
// use test identities on that hub from an agent or a workstation.
//
// A profile is stored in ~/.scion/hub-profiles/<name>.yaml and holds only
// non-secret fields: the endpoint, the transport (IAP or none), the IAP
// audience, the target's issuer and IAP service account emails, and the
// ids of the issuer delegations created on the target. Tokens are minted
// in-process (see transportauth.IAMIDTokenSource) and never written to the
// profile, the environment, standard output or logs. Test identity tokens
// issued through a profile are kept in a per-run cache next to the profile
// (cache.go).
//
// When a profile is selected, the CLI's bearer comes only from the profile:
// OAuth credentials, ~/.scion/scion-token, SCION_AUTH_TOKEN and
// SCION_HUB_TOKEN are never consulted, and a command with no applicable
// profile credential fails.
package hubprofile

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

const (
	// EnvHubProfile selects a profile when --hub-profile is not given.
	EnvHubProfile = "SCION_HUB_PROFILE"

	// EnvHubRun names the run label when --run is not given.
	EnvHubRun = "SCION_HUB_RUN"

	// TransportIAP sends an ID token for the target's IAP service account
	// in Proxy-Authorization.
	TransportIAP = "iap"
	// TransportNone sends no transport credential.
	TransportNone = "none"

	// TierMember is the only delegation tier a profile supports: member
	// and viewer test identities.
	TierMember = "member"

	profilesDirName = "hub-profiles"
	profileFileExt  = ".yaml"
)

// adminTiers are the admin delegation tiers, refused by name with a clear
// message until the admin tiers support workload delegation.
var adminTiers = map[string]bool{"hub-admin": true, "super-admin": true, "admin": true}

// nameRE bounds profile names, run labels and aliases: they become path
// components, so they are restricted to a safe alphabet.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidateName checks a profile name, run label or alias.
func ValidateName(kind, s string) error {
	if !nameRE.MatchString(s) {
		return fmt.Errorf("invalid %s %q: use 1-63 lowercase letters, digits, '-' or '_', starting with a letter or digit", kind, s)
	}
	return nil
}

// Profile is a target-hub profile. Every field is non-secret.
type Profile struct {
	// Name is the profile name (the file name without .yaml).
	Name string `yaml:"name" json:"name"`
	// Endpoint is the target hub URL.
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// Transport is "iap" or "none".
	Transport string `yaml:"transport" json:"transport"`
	// IAPAudience is the IAP OAuth client id (transport iap only).
	IAPAudience string `yaml:"iapAudience,omitempty" json:"iapAudience,omitempty"`
	// IssuerServiceAccount is the target's issuer service account email.
	// Issuer ID tokens are minted as this account.
	IssuerServiceAccount string `yaml:"issuerServiceAccount,omitempty" json:"issuerServiceAccount,omitempty"`
	// UATServiceAccount is the target's IAP service account email. IAP
	// transport tokens are minted as this account.
	UATServiceAccount string `yaml:"uatServiceAccount,omitempty" json:"uatServiceAccount,omitempty"`
	// IssuerDelegations maps a tier ("member") to a delegation id on the
	// target hub. The ids are not secrets.
	IssuerDelegations map[string]string `yaml:"issuerDelegations,omitempty" json:"issuerDelegations,omitempty"`
}

// Validate checks every field. It never contacts the target.
func (p *Profile) Validate() error {
	if err := ValidateName("profile name", p.Name); err != nil {
		return err
	}
	if _, err := NormalizeEndpoint(p.Endpoint); err != nil {
		return err
	}
	switch p.Transport {
	case TransportIAP:
		if strings.TrimSpace(p.IAPAudience) == "" {
			return errors.New("transport iap needs an IAP audience (--iap-audience)")
		}
		if p.UATServiceAccount == "" {
			return errors.New("transport iap needs the target's IAP service account (--uat-sa)")
		}
	case TransportNone:
		if p.IAPAudience != "" || p.UATServiceAccount != "" {
			return errors.New("an IAP audience or IAP service account needs transport iap")
		}
	default:
		return fmt.Errorf("invalid transport %q: must be %q or %q", p.Transport, TransportIAP, TransportNone)
	}
	for _, sa := range []struct{ flag, v string }{{"--issuer-sa", p.IssuerServiceAccount}, {"--uat-sa", p.UATServiceAccount}} {
		if sa.v != "" && !validServiceAccountEmail(sa.v) {
			return fmt.Errorf("invalid %s %q: must be a service account email", sa.flag, sa.v)
		}
	}
	for tier, id := range p.IssuerDelegations {
		if adminTiers[tier] {
			return fmt.Errorf("issuer delegation tier %q is not supported: profiles support the member tier only", tier)
		}
		if tier != TierMember {
			return fmt.Errorf("invalid issuer delegation tier %q: must be %q", tier, TierMember)
		}
		if !validDelegationID(id) {
			return fmt.Errorf("invalid issuer delegation id %q", id)
		}
	}
	if len(p.IssuerDelegations) > 0 && p.IssuerServiceAccount == "" {
		return errors.New("an issuer delegation needs the target's issuer service account (--issuer-sa)")
	}
	return nil
}

// MemberDelegation returns the member delegation id, or "".
func (p *Profile) MemberDelegation() string {
	return p.IssuerDelegations[TierMember]
}

var delegationIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func validDelegationID(id string) bool { return delegationIDRE.MatchString(id) }

var saEmailRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}@[a-z0-9][a-z0-9.-]{0,252}\.gserviceaccount\.com$`)

func validServiceAccountEmail(s string) bool { return saEmailRE.MatchString(s) }

// ParseDelegations parses "tier=id" flag values.
func ParseDelegations(values []string) (map[string]string, error) {
	out := map[string]string{}
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			tier, id, ok := strings.Cut(part, "=")
			if !ok || tier == "" || id == "" {
				return nil, fmt.Errorf("invalid --issuer-delegation %q: use tier=id, for example member=<id>", part)
			}
			if _, dup := out[tier]; dup {
				return nil, fmt.Errorf("duplicate --issuer-delegation tier %q", tier)
			}
			out[tier] = id
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// NormalizeEndpoint parses an endpoint and returns its canonical form
// (lowercase scheme and host, no default port, no trailing slash, no
// query, fragment or user info). https is required except for a loopback
// host.
func NormalizeEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("an endpoint is required (--endpoint)")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid endpoint %q: must be an absolute URL", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid endpoint %q: no user info, query or fragment", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	switch scheme {
	case "https":
		if port == "443" {
			port = ""
		}
	case "http":
		if !isLoopbackHost(host) {
			return "", fmt.Errorf("invalid endpoint %q: https is required", raw)
		}
		if port == "80" {
			port = ""
		}
	default:
		return "", fmt.Errorf("invalid endpoint %q: https is required", raw)
	}
	hostport := host
	if strings.Contains(host, ":") {
		hostport = "[" + host + "]"
	}
	if port != "" {
		hostport += ":" + port
	}
	return scheme + "://" + hostport + strings.TrimRight(u.EscapedPath(), "/"), nil
}

func isLoopbackHost(h string) bool {
	return transportauth.IsLoopbackHost(h)
}

// ErrOwnHub is returned when a profile's endpoint is the agent's own hub.
var ErrOwnHub = errors.New("the profile's endpoint is this agent's own hub; a target-hub profile must name another hub")

// ErrOwnHubUnknown is returned when SCION_HUB_ENDPOINT or SCION_HUB_URL is
// set but is not an absolute URL with a host, so the agent's own hub cannot
// be compared (the check fails closed).
var ErrOwnHubUnknown = errors.New("cannot determine this agent's own hub: SCION_HUB_ENDPOINT or SCION_HUB_URL is not an absolute URL")

// bridgeHostNames are the container bridge host names that reach the
// agent's host (mirrors pkg/runtimebroker bridgeHostnames). For the own-hub
// comparison they count as loopback: an agent's SCION_HUB_ENDPOINT is often
// rewritten to one of them.
var bridgeHostNames = map[string]bool{
	"host.docker.internal":     true,
	"host.containers.internal": true,
}

// hubAddr is a parsed hub endpoint reduced to what the own-hub comparison
// uses: the canonical host name and the effective port.
type hubAddr struct {
	host     string // CanonicalHost of the parsed host name
	port     string // explicit port, or the scheme's default
	loopback bool   // a loopback or container bridge host
}

// parseHubAddr parses an absolute http(s) URL with a host. User info is
// refused outright.
func parseHubAddr(raw string) (hubAddr, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Opaque != "" {
		return hubAddr{}, errors.New("not an absolute URL with a host")
	}
	if u.User != nil {
		return hubAddr{}, errors.New("user info is not allowed")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return hubAddr{}, errors.New("not an http or https URL")
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if scheme == "http" {
			port = "80"
		}
	}
	// Canonicalize first (case, trailing dot, IP form), then classify the
	// parsed host.
	h := transportauth.CanonicalHost(u.Hostname())
	if h == "" {
		return hubAddr{}, errors.New("not an absolute URL with a host")
	}
	return hubAddr{host: h, port: port, loopback: transportauth.IsLoopbackHost(h) || bridgeHostNames[h]}, nil
}

// ownHubAddrs parses the agent's own hub endpoints from SCION_HUB_ENDPOINT
// and SCION_HUB_URL. A set but unparsable value is an error (fail closed).
func ownHubAddrs(getenv func(string) string) ([]hubAddr, error) {
	var out []hubAddr
	for _, k := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
		v := strings.TrimSpace(getenv(k))
		if v == "" {
			continue
		}
		a, err := parseHubAddr(v)
		if err != nil {
			return nil, fmt.Errorf("%w (%s: %v)", ErrOwnHubUnknown, k, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// CheckNotOwnHub refuses an endpoint on the agent's own hub
// (SCION_HUB_ENDPOINT, SCION_HUB_URL), and fails closed when one of those
// is set but cannot be parsed. Both sides are parsed and canonicalized
// before the comparison: host names compare lowercase without a trailing
// dot, IP literals in canonical form, and any two loopback or container
// bridge hosts (localhost, 127.0.0.0/8, ::1, host.docker.internal,
// host.containers.internal) count as one host. On one host the effective
// ports (the scheme's default when none is given) must be equal, or both
// be default ports (80 or 443); two explicit non-default ports are
// different hubs. Paths are ignored.
//
// It compares names, not servers: a DNS alias of the agent's own hub, or its
// public URL when SCION_HUB_ENDPOINT holds a bridge or internal address, is
// not detected. The target hub's own delegation remains the boundary.
func CheckNotOwnHub(endpoint string, getenv func(string) string) error {
	target, err := parseHubAddr(endpoint)
	if err != nil {
		return fmt.Errorf("invalid endpoint %q: %v", endpoint, err)
	}
	owns, err := ownHubAddrs(getenv)
	if err != nil {
		return err
	}
	for _, own := range owns {
		if sameHub(target, own) {
			return ErrOwnHub
		}
	}
	return nil
}

// sameHub reports whether two parsed endpoints name one hub (see
// CheckNotOwnHub).
func sameHub(a, b hubAddr) bool {
	if a.host != b.host && (!a.loopback || !b.loopback) {
		return false
	}
	isDefault := func(p string) bool { return p == "80" || p == "443" }
	return a.port == b.port || (isDefault(a.port) && isDefault(b.port))
}

// sameAddr reports whether two parsed endpoints have the same canonical
// host and the same effective port, with no loopback, bridge or
// default-port collapsing. It binds a cached identity to the endpoint it
// was issued on; the broad sameHub rule is for CheckNotOwnHub only.
func sameAddr(a, b hubAddr) bool {
	return a.host == b.host && a.port == b.port
}

// sameEndpointExact reports whether two endpoint strings have the same
// canonical host and effective port (sameAddr); false when either does
// not parse.
func sameEndpointExact(a, b string) bool {
	pa, errA := parseHubAddr(a)
	pb, errB := parseHubAddr(b)
	return errA == nil && errB == nil && sameAddr(pa, pb)
}

// Dir returns ~/.scion/hub-profiles for home.
func Dir(home string) string {
	return filepath.Join(home, ".scion", profilesDirName)
}

// HomeDir returns the user's home directory from $HOME.
func HomeDir() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return "", errors.New("cannot find the home directory")
		}
		home = h
	}
	return home, nil
}

// ensurePrivateDir creates dir, which must be base or below it, with mode
// 0700. base is ~/.scion/hub-profiles: it and every component from it down
// to dir is inspected with Lstat, created when missing, and refused when it
// is a symlink or not a directory, so an existing directory below a
// symlinked component is never followed. Components above base (such as
// ~/.scion) are created with MkdirAll when missing and not inspected.
func ensurePrivateDir(base, dir string) error {
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("refusing %s: not below %s", dir, base)
	}
	if _, err := os.Lstat(base); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
			return err
		}
	}
	cur := base
	parts := []string{}
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	for i := -1; i < len(parts); i++ {
		if i >= 0 {
			cur = filepath.Join(cur, parts[i])
		}
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(cur, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			fi, err = os.Lstat(cur)
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing %s: it is a symlink", cur)
		}
		if !fi.IsDir() {
			return fmt.Errorf("refusing %s: it is not a directory", cur)
		}
	}
	return nil
}

// profilePath returns the profile file path, validating the name.
func profilePath(home, name string) (string, error) {
	if err := ValidateName("profile name", name); err != nil {
		return "", err
	}
	return filepath.Join(Dir(home), name+profileFileExt), nil
}

// Save writes a new profile file (0600, exclusive). An existing profile is
// replaced only when replace is true.
func Save(home string, p *Profile, replace bool) error {
	if err := p.Validate(); err != nil {
		return err
	}
	norm, _ := NormalizeEndpoint(p.Endpoint)
	p.Endpoint = norm
	path, err := profilePath(home, p.Name)
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(Dir(home), Dir(home)); err != nil {
		return err
	}
	data, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing %s: it is a symlink", path)
		}
		if !replace {
			return fmt.Errorf("profile %q already exists; remove it first or use --replace", p.Name)
		}
	}
	return writeFileAtomic(path, data)
}

// Load reads and validates the named profile.
func Load(home, name string) (*Profile, error) {
	path, err := profilePath(home, name)
	if err != nil {
		return nil, err
	}
	data, err := readPrivateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("hub profile %q not found (add it with 'scion hub profile add %s ...')", name, name)
	}
	if err != nil {
		return nil, fmt.Errorf("hub profile %q: %w", name, err)
	}
	var p Profile
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("hub profile %q: invalid file: %w", name, err)
	}
	if p.Name != name {
		return nil, fmt.Errorf("hub profile %q: the file names profile %q", name, p.Name)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("hub profile %q: %w", name, err)
	}
	p.Endpoint, _ = NormalizeEndpoint(p.Endpoint)
	return &p, nil
}

// List returns the names of the saved profiles, sorted.
func List(home string) ([]string, error) {
	entries, err := os.ReadDir(Dir(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		n, ok := strings.CutSuffix(e.Name(), profileFileExt)
		if !ok || !e.Type().IsRegular() || ValidateName("profile name", n) != nil {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// Remove deletes the profile file. Its identity cache is kept unless
// removeCache is true; a profile with cached runs is refused otherwise.
func Remove(home, name string, removeCache bool) error {
	path, err := profilePath(home, name)
	if err != nil {
		return err
	}
	runs, err := ListRuns(home, name)
	if err != nil {
		return err
	}
	if len(runs) > 0 && !removeCache {
		return fmt.Errorf("profile %q still has cached runs (%s); tear them down first ('scion hub test-identity teardown --run <label>')", name, strings.Join(runs, ", "))
	}
	if fi, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("hub profile %q not found", name)
		}
		return err
	} else if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing %s: it is a symlink", path)
	}
	if removeCache {
		if err := removeProfileCacheDir(home, name); err != nil {
			return err
		}
	}
	return os.Remove(path)
}
