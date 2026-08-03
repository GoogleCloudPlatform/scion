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

package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync/atomic"
	"time"
)

// validAuthSchemes lists the recognized auth schemes for validation.
var validAuthSchemes = map[string]bool{
	"apiKey": true,
	"bearer": true,
	"none":   true,
	"hubUAT": true,
	"hubJWT": true,
}

// AdminOverlay holds the parsed admin-managed config values.
// These override corresponding bridge YAML values when present.
type AdminOverlay struct {
	ExternalURL        string
	AuthScheme         string // "apiKey"|"bearer"|"none"|"hubUAT"|"hubJWT"
	APIKey             string // from secret backend, never persisted
	UATCacheTTL        time.Duration
	RateLimitEnabled   *bool   // pointer so we can distinguish absent from false
	RateLimitRPS       float64 // 0 means not set
	RateLimitBurst     int     // -1 means not set (0 means use default 20)
	SendMessageTimeout time.Duration
	SSEKeepalive       time.Duration
	PushRetryMax       int // -1 means not set
	ProviderOrg        string
	ProviderURL        string
	Projects           []ProjectConfig // parsed from projects_json

	// presentKeys tracks which keys were explicitly provided in the Configure push.
	// Only present keys override base YAML values.
	presentKeys map[string]bool
}

// IsPresent returns true if the given key was explicitly provided in the overlay.
func (o *AdminOverlay) IsPresent(key string) bool {
	if o == nil || o.presentKeys == nil {
		return false
	}
	return o.presentKeys[key]
}

// persistedOverlay is the JSON-serializable form of AdminOverlay.
// The api_key field is deliberately excluded.
type persistedOverlay struct {
	ExternalURL        string          `json:"external_url,omitempty"`
	AuthScheme         string          `json:"auth_scheme,omitempty"`
	UATCacheTTL        string          `json:"uat_cache_ttl,omitempty"`
	RateLimitEnabled   *bool           `json:"rate_limit_enabled,omitempty"`
	RateLimitRPS       float64         `json:"rate_limit_rps,omitempty"`
	RateLimitBurst     *int            `json:"rate_limit_burst,omitempty"`
	SendMessageTimeout string          `json:"send_message_timeout,omitempty"`
	SSEKeepalive       string          `json:"sse_keepalive,omitempty"`
	PushRetryMax       *int            `json:"push_retry_max,omitempty"`
	ProviderOrg        string          `json:"provider_org,omitempty"`
	ProviderURL        string          `json:"provider_url,omitempty"`
	Projects           []ProjectConfig `json:"projects,omitempty"`
	PresentKeys        []string        `json:"present_keys,omitempty"`
}

// ConfigSnapshot holds the effective runtime configuration.
// Readers access it atomically; writers swap the entire snapshot.
type ConfigSnapshot struct {
	Config  Config         // effective merged config
	Auth    AuthValidators // pre-built validators matching current scheme
	Limiter *RateLimiter   // nil if disabled; rebuilt on config change
}

// AuthValidators holds the active auth validation functions.
type AuthValidators struct {
	Scheme       string
	UATValidator *UATValidator // non-nil when scheme is hubUAT
	JWTValidator *JWTValidator // non-nil when scheme is hubJWT
	APIKey       string        // non-empty when scheme is apiKey or bearer
}

// SnapshotHolder wraps an atomic pointer to ConfigSnapshot for lock-free reads.
type SnapshotHolder struct {
	ptr atomic.Pointer[ConfigSnapshot]
}

// NewSnapshotHolder creates a SnapshotHolder initialized with the given snapshot.
func NewSnapshotHolder(snap *ConfigSnapshot) *SnapshotHolder {
	h := &SnapshotHolder{}
	h.ptr.Store(snap)
	return h
}

// Load returns the current snapshot.
func (h *SnapshotHolder) Load() *ConfigSnapshot {
	return h.ptr.Load()
}

// Store atomically replaces the current snapshot.
func (h *SnapshotHolder) Store(snap *ConfigSnapshot) {
	h.ptr.Store(snap)
}

// ParseAdminOverlay parses and validates the flat key map from Configure().
// Returns an error for invalid values (the bridge rejects the push and keeps last-good).
// Absent keys mean "not set by admin, use base YAML value."
func ParseAdminOverlay(cfg map[string]string) (*AdminOverlay, error) {
	overlay := &AdminOverlay{
		RateLimitBurst: -1, // sentinel: not set
		PushRetryMax:   -1, // sentinel: not set
		presentKeys:    make(map[string]bool),
	}

	// String fields can be explicitly cleared; non-string fields with empty
	// values should not override base config.
	for key, val := range cfg {
		isStringField := key == "external_url" || key == "auth_scheme" || key == "api_key" ||
			key == "provider_org" || key == "provider_url"
		if val != "" || isStringField {
			overlay.presentKeys[key] = true
		}
	}

	// external_url
	if v, ok := cfg["external_url"]; ok {
		overlay.ExternalURL = v
	}

	// auth_scheme
	if v, ok := cfg["auth_scheme"]; ok {
		if v != "" && !validAuthSchemes[v] {
			return nil, fmt.Errorf("invalid auth_scheme %q: must be one of apiKey, bearer, none, hubUAT, hubJWT", v)
		}
		overlay.AuthScheme = v
	}

	// api_key (from secret backend)
	if v, ok := cfg["api_key"]; ok {
		overlay.APIKey = v
	}

	// uat_cache_ttl
	if v, ok := cfg["uat_cache_ttl"]; ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid uat_cache_ttl %q: %w", v, err)
		}
		if d < 0 {
			return nil, fmt.Errorf("invalid uat_cache_ttl %q: must not be negative", v)
		}
		if d > 300*time.Second {
			return nil, fmt.Errorf("invalid uat_cache_ttl %q: must not exceed 300s", v)
		}
		overlay.UATCacheTTL = d
	}

	// rate_limit_enabled
	if v, ok := cfg["rate_limit_enabled"]; ok && v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("invalid rate_limit_enabled %q: %w", v, err)
		}
		overlay.RateLimitEnabled = &b
	}

	// rate_limit_rps
	if v, ok := cfg["rate_limit_rps"]; ok && v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid rate_limit_rps %q: %w", v, err)
		}
		if f <= 0 {
			return nil, fmt.Errorf("invalid rate_limit_rps %q: must be > 0", v)
		}
		overlay.RateLimitRPS = f
	}

	// rate_limit_burst
	if v, ok := cfg["rate_limit_burst"]; ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid rate_limit_burst %q: %w", v, err)
		}
		if n < 0 {
			return nil, fmt.Errorf("invalid rate_limit_burst %q: must be >= 0", v)
		}
		overlay.RateLimitBurst = n
	}

	// send_message_timeout
	if v, ok := cfg["send_message_timeout"]; ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid send_message_timeout %q: %w", v, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("invalid send_message_timeout %q: must be positive", v)
		}
		overlay.SendMessageTimeout = d
	}

	// sse_keepalive
	if v, ok := cfg["sse_keepalive"]; ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("invalid sse_keepalive %q: %w", v, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("invalid sse_keepalive %q: must be positive", v)
		}
		overlay.SSEKeepalive = d
	}

	// push_retry_max
	if v, ok := cfg["push_retry_max"]; ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid push_retry_max %q: %w", v, err)
		}
		if n < 0 {
			return nil, fmt.Errorf("invalid push_retry_max %q: must be >= 0", v)
		}
		overlay.PushRetryMax = n
	}

	// provider_org
	if v, ok := cfg["provider_org"]; ok {
		overlay.ProviderOrg = v
	}

	// provider_url
	if v, ok := cfg["provider_url"]; ok {
		overlay.ProviderURL = v
	}

	// projects_json
	if v, ok := cfg["projects_json"]; ok && v != "" {
		var projects []ProjectConfig
		if err := json.Unmarshal([]byte(v), &projects); err != nil {
			return nil, fmt.Errorf("invalid projects_json: %w", err)
		}
		overlay.Projects = projects
	}

	return overlay, nil
}

// ApplyOverlay merges an admin overlay onto a base config, producing the effective config.
// Overlay values win for each present key; absent overlay keys leave base values intact.
func ApplyOverlay(base Config, overlay *AdminOverlay) Config {
	if overlay == nil {
		return base
	}

	cfg := base // copy

	if overlay.IsPresent("external_url") {
		cfg.Bridge.ExternalURL = overlay.ExternalURL
	}
	if overlay.IsPresent("auth_scheme") {
		cfg.Auth.Scheme = overlay.AuthScheme
	}
	if overlay.IsPresent("api_key") {
		cfg.Auth.APIKey = overlay.APIKey
	}
	if overlay.IsPresent("uat_cache_ttl") {
		cfg.Auth.UATCacheTTL = overlay.UATCacheTTL
	}
	if overlay.IsPresent("rate_limit_enabled") && overlay.RateLimitEnabled != nil {
		cfg.RateLimit.Enabled = *overlay.RateLimitEnabled
	}
	if overlay.IsPresent("rate_limit_rps") && overlay.RateLimitRPS > 0 {
		cfg.RateLimit.RequestsPerSec = overlay.RateLimitRPS
	}
	if overlay.IsPresent("rate_limit_burst") && overlay.RateLimitBurst >= 0 {
		cfg.RateLimit.Burst = overlay.RateLimitBurst
	}
	if overlay.IsPresent("send_message_timeout") {
		cfg.Timeouts.SendMessage = overlay.SendMessageTimeout
	}
	if overlay.IsPresent("sse_keepalive") {
		cfg.Timeouts.SSEKeepalive = overlay.SSEKeepalive
	}
	if overlay.IsPresent("push_retry_max") && overlay.PushRetryMax >= 0 {
		cfg.Timeouts.PushRetryMax = overlay.PushRetryMax
	}
	if overlay.IsPresent("provider_org") {
		cfg.Bridge.Provider.Organization = overlay.ProviderOrg
	}
	if overlay.IsPresent("provider_url") {
		cfg.Bridge.Provider.URL = overlay.ProviderURL
	}
	if overlay.IsPresent("projects_json") && overlay.Projects != nil {
		cfg.Projects = overlay.Projects
	}

	return cfg
}

// BuildAuthValidators constructs the appropriate validators for the given config.
func BuildAuthValidators(cfg *Config) AuthValidators {
	av := AuthValidators{
		Scheme: cfg.Auth.Scheme,
	}
	switch cfg.Auth.Scheme {
	case "hubUAT":
		ttl := cfg.Auth.UATCacheTTL
		av.UATValidator = NewUATValidator(cfg.Hub.Endpoint, ttl)
	case "hubJWT":
		// JWTValidator requires a signing key which is loaded separately.
		// It will be set via SetJWTValidator on the Server.
	case "apiKey", "bearer", "":
		av.APIKey = cfg.Auth.APIKey
	}
	return av
}

// BuildSnapshot creates a complete ConfigSnapshot from the effective config.
func BuildSnapshot(cfg Config) *ConfigSnapshot {
	snap := &ConfigSnapshot{
		Config: cfg,
		Auth:   BuildAuthValidators(&cfg),
	}
	if cfg.RateLimit.Enabled {
		rate := cfg.RateLimit.RequestsPerSec
		if rate == 0 {
			rate = 10
		}
		burst := cfg.RateLimit.Burst
		if burst == 0 {
			burst = 20
		}
		snap.Limiter = NewRateLimiter(rate, burst)
	}
	return snap
}

const overlayFileName = "admin-overlay.json"

// PersistOverlay writes the overlay to admin-overlay.json in the state directory.
// The api_key field is deliberately excluded from persistence.
func PersistOverlay(stateDir string, overlay *AdminOverlay) error {
	p := persistedOverlay{
		ExternalURL:  overlay.ExternalURL,
		AuthScheme:   overlay.AuthScheme,
		RateLimitRPS: overlay.RateLimitRPS,
		ProviderOrg:  overlay.ProviderOrg,
		ProviderURL:  overlay.ProviderURL,
		Projects:     overlay.Projects,
	}
	// Only persist non-sentinel values for integer fields.
	if overlay.RateLimitBurst >= 0 {
		b := overlay.RateLimitBurst
		p.RateLimitBurst = &b
	}
	if overlay.PushRetryMax >= 0 {
		b := overlay.PushRetryMax
		p.PushRetryMax = &b
	}
	if overlay.RateLimitEnabled != nil {
		p.RateLimitEnabled = overlay.RateLimitEnabled
	}
	if overlay.IsPresent("uat_cache_ttl") {
		p.UATCacheTTL = overlay.UATCacheTTL.String()
	}
	if overlay.SendMessageTimeout > 0 {
		p.SendMessageTimeout = overlay.SendMessageTimeout.String()
	}
	if overlay.SSEKeepalive > 0 {
		p.SSEKeepalive = overlay.SSEKeepalive.String()
	}

	// Persist which keys were present so the overlay can be re-applied correctly.
	// Keys are sorted for deterministic serialization.
	keys := make([]string, 0, len(overlay.presentKeys))
	for k := range overlay.presentKeys {
		// Never persist api_key presence — it comes fresh on each Configure push.
		if k == "api_key" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	p.PresentKeys = keys

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal overlay: %w", err)
	}

	path := filepath.Join(stateDir, overlayFileName)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("write temp overlay: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename overlay: %w", err)
	}
	return nil
}

// LoadPersistedOverlay reads a previously saved overlay. Returns nil, nil if no file exists.
func LoadPersistedOverlay(stateDir string) (*AdminOverlay, error) {
	path := filepath.Join(stateDir, overlayFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read overlay file: %w", err)
	}

	var p persistedOverlay
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse overlay file: %w", err)
	}

	overlay := &AdminOverlay{
		ExternalURL:      p.ExternalURL,
		AuthScheme:       p.AuthScheme,
		RateLimitEnabled: p.RateLimitEnabled,
		RateLimitRPS:     p.RateLimitRPS,
		RateLimitBurst:   -1, // sentinel: not set
		PushRetryMax:     -1, // sentinel: not set
		ProviderOrg:      p.ProviderOrg,
		ProviderURL:      p.ProviderURL,
		Projects:         p.Projects,
		presentKeys:      make(map[string]bool),
	}

	// Dereference persisted pointers; nil means sentinel stays.
	if p.RateLimitBurst != nil {
		overlay.RateLimitBurst = *p.RateLimitBurst
	}
	if p.PushRetryMax != nil {
		overlay.PushRetryMax = *p.PushRetryMax
	}

	// Restore present keys.
	for _, k := range p.PresentKeys {
		overlay.presentKeys[k] = true
	}

	if p.UATCacheTTL != "" {
		d, err := time.ParseDuration(p.UATCacheTTL)
		if err != nil {
			return nil, fmt.Errorf("parse persisted uat_cache_ttl: %w", err)
		}
		overlay.UATCacheTTL = d
	}
	if p.SendMessageTimeout != "" {
		d, err := time.ParseDuration(p.SendMessageTimeout)
		if err != nil {
			return nil, fmt.Errorf("parse persisted send_message_timeout: %w", err)
		}
		overlay.SendMessageTimeout = d
	}
	if p.SSEKeepalive != "" {
		d, err := time.ParseDuration(p.SSEKeepalive)
		if err != nil {
			return nil, fmt.Errorf("parse persisted sse_keepalive: %w", err)
		}
		overlay.SSEKeepalive = d
	}

	return overlay, nil
}
