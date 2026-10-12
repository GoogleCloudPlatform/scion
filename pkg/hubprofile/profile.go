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
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
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
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// OwnHubEndpoints returns the agent's own hub endpoints from the
// environment (SCION_HUB_ENDPOINT and SCION_HUB_URL), normalized. Values
// that do not parse are returned raw, lowercased, so they still compare.
func OwnHubEndpoints(getenv func(string) string) []string {
	var out []string
	for _, k := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
		v := strings.TrimSpace(getenv(k))
		if v == "" {
			continue
		}
		if n, err := normalizeAny(v); err == nil {
			out = append(out, n)
		} else {
			out = append(out, strings.ToLower(strings.TrimRight(v, "/")))
		}
	}
	return out
}

// normalizeAny normalizes like NormalizeEndpoint but accepts http on any
// host, for comparing against the agent's own hub.
func normalizeAny(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", errors.New("not an absolute URL")
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
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

// ErrOwnHub is returned when a profile's endpoint is the agent's own hub.
var ErrOwnHub = errors.New("the profile's endpoint is this agent's own hub; a target-hub profile must name another hub")

// CheckNotOwnHub refuses an endpoint on the agent's own hub. Endpoints
// compare by host name and port, never by path: one host name is the same
// hub when the effective ports (the scheme's default when none is given)
// are equal, or when both are default ports (80 or 443), so http and https
// of one host also count as the same hub. Two explicit non-default ports
// on one host are different hubs (local test hubs).
func CheckNotOwnHub(endpoint string, getenv func(string) string) error {
	n, err := normalizeAny(endpoint)
	if err != nil {
		return err
	}
	for _, own := range OwnHubEndpoints(getenv) {
		if sameHub(n, own) {
			return ErrOwnHub
		}
	}
	return nil
}

// sameHub reports whether two normalized endpoints name one hub (see
// CheckNotOwnHub).
func sameHub(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil || ua.Host == "" || ub.Host == "" {
		return hostOf(a) == hostOf(b)
	}
	if !strings.EqualFold(ua.Hostname(), ub.Hostname()) {
		return false
	}
	pa, pb := effectivePort(ua), effectivePort(ub)
	isDefault := func(p string) bool { return p == "80" || p == "443" }
	return pa == pb || (isDefault(pa) && isDefault(pb))
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	return "443"
}

func stripScheme(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		return s[i+3:]
	}
	return s
}

// hostOf returns host[:port] of a normalized endpoint.
func hostOf(s string) string {
	s = stripScheme(s)
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
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

// ensurePrivateDir creates dir (and its missing parents under base) with
// mode 0700, refusing a symlink at any component it inspects.
func ensurePrivateDir(dir string) error {
	if fi, err := os.Lstat(dir); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing %s: it is a symlink", dir)
		}
		if !fi.IsDir() {
			return fmt.Errorf("refusing %s: it is not a directory", dir)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensurePrivateDir(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("refusing %s: it is not a directory", dir)
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
	if err := ensurePrivateDir(Dir(home)); err != nil {
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
