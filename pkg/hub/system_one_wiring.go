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

package hub

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	systemoneprovider "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	_ "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/jevwire" // register built-in "typesafe" System One adapter
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

const (
	// systemOneSecretPrefix reserves the hub-scoped internal secret namespace
	// used for System One provider instance credentials. Generic secret and
	// environment-variable endpoints refuse to list, read, create, update, or
	// delete keys under this prefix (or any backend-sanitized collision form) in
	// any scope.
	systemOneSecretPrefix = "systemone.instance."

	// systemOneSanitizedSecretPrefix is systemOneSecretPrefix after backend
	// secret-ID sanitization (for example GCP Secret Manager maps non-[a-zA-Z0-9_-]
	// characters to "-", turning "systemone.instance.<id>" into
	// "systemone-instance-<id>").
	systemOneSanitizedSecretPrefix = "systemone-instance-"

	// systemOneInstancesListPageSize is the store page size used when
	// Runtime.ListInstances scans configured System One instances.
	systemOneInstancesListPageSize = 200
)

// systemOneKeyNonIDChars matches pkg/secret's invalidSecretIDChars
// ([^a-zA-Z0-9_-] in pkg/secret/gcpbackend.go) so every rune outside GCP
// Secret Manager's secret-ID alphabet—including non-ASCII letters such as
// U+212A (KELVIN SIGN) and U+0130 (LATIN CAPITAL LETTER I WITH DOT ABOVE) that
// strings.ToLower folds to ASCII letters—is replaced with "-" BEFORE case
// folding. Parity with GCPBackend's real secret-ID computation is enforced in
// TestIsSystemOneReservedSecretKey_SanitizedForms.
var systemOneKeyNonIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// systemOneInstanceSecretName returns the hub-scoped internal secret name for
// the System One provider instance with the given immutable UUID.
func systemOneInstanceSecretName(instanceID string) string {
	return systemOneSecretPrefix + strings.TrimSpace(instanceID)
}

func hasSystemOneSanitizedPrefix(s string) bool {
	k := systemOneKeyNonIDChars.ReplaceAllString(s, "-")
	k = strings.ReplaceAll(strings.ToLower(k), "_", "-")
	return strings.HasPrefix(k, systemOneSanitizedSecretPrefix)
}

// isSystemOneReservedSecretKey reports whether key falls under the reserved
// System One provider-credential secret namespace, applying GCP Secret
// Manager's character sanitization first and case/separator folding second so
// generic secret and env endpoints refuse both "systemone.instance.<id>" and
// any colliding form (such as "systemone-instance-<id>",
// "SystemOne_Instance_<id>", or Unicode separator collisions like
// "systemone\u0130instance\u0130<id>" and "systemone\u212ainstance.<id>").
func isSystemOneReservedSecretKey(key string) bool {
	if hasSystemOneSanitizedPrefix(key) {
		return true
	}
	trimmed := strings.TrimSpace(key)
	return trimmed != key && hasSystemOneSanitizedPrefix(trimmed)
}

// withoutSystemOneReservedSecrets filters out secrets under the reserved
// System One provider-credential namespace so generic secret and env endpoints
// never expose them.
func withoutSystemOneReservedSecrets(metas []secret.SecretMeta) []secret.SecretMeta {
	if len(metas) == 0 {
		return metas
	}
	out := make([]secret.SecretMeta, 0, len(metas))
	for _, m := range metas {
		if isSystemOneReservedSecretKey(m.Name) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// isValidSystemOneAPIKey reports whether v is a non-empty bearer credential
// free of HTTP header control characters.
func isValidSystemOneAPIKey(v string) bool {
	if strings.TrimSpace(v) == "" {
		return false
	}
	for i := 0; i < len(v); i++ {
		b := v[i]
		if b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

// setSystemOneInstanceSecret stores apiKey as a hub-scoped internal secret
// under systemone.instance.<instanceID> via pkg/secret.
func (s *Server) setSystemOneInstanceSecret(ctx context.Context, instanceID, apiKey, actorID string) (string, error) {
	if s.secretBackend == nil {
		return "", secret.ErrNoSecretBackend
	}
	secretName := systemOneInstanceSecretName(instanceID)
	_, _, err := s.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name:       secretName,
		Value:      apiKey,
		SecretType: store.SecretTypeInternal,
		Target:     secretName,
		Scope:      store.ScopeHub,
		ScopeID:    s.hubID,
		CreatedBy:  actorID,
		UpdatedBy:  actorID,
	})
	if err != nil {
		return "", err
	}
	return secretName, nil
}

// deleteSystemOneInstanceSecret removes a hub-scoped internal System One
// instance credential from pkg/secret. Missing secrets are treated as a no-op.
func (s *Server) deleteSystemOneInstanceSecret(ctx context.Context, secretName string) error {
	secretName = strings.TrimSpace(secretName)
	if secretName == "" || s.secretBackend == nil {
		return nil
	}
	if !strings.HasPrefix(secretName, systemOneSecretPrefix) {
		return fmt.Errorf("system_one: refusing to delete non-reserved secret %q", secretName)
	}
	if err := s.secretBackend.Delete(ctx, secretName, store.ScopeHub, s.hubID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// resolveSystemOneMaxEvalTime resolves the effective System One MaxEvalTime
// ceiling from ServerConfig, falling back to StartupServerSettings
// (server.hub.system_one.max_eval_time) and finally the 3s default.
func resolveSystemOneMaxEvalTime(cfg ServerConfig) time.Duration {
	if cfg.SystemOneMaxEvalTime > 0 {
		if cfg.SystemOneMaxEvalTime > systemoneruntime.MaxAllowedEvalTime {
			return systemoneruntime.MaxAllowedEvalTime
		}
		return cfg.SystemOneMaxEvalTime
	}
	if cfg.StartupServerSettings != nil &&
		cfg.StartupServerSettings.Hub != nil &&
		cfg.StartupServerSettings.Hub.SystemOne != nil {
		hs := config.HubSystemOneConfig{MaxEvalTime: cfg.StartupServerSettings.Hub.SystemOne.MaxEvalTime}
		if d, err := hs.MaxEvalTimeDuration(); err == nil && d > 0 {
			return d
		}
	}
	return config.SystemOneDefaultMaxEvalTime
}

// SystemOneProviderRegistry returns the Hub's System One provider adapter
// registry, defaulting to provider.DefaultRegistry().
func (s *Server) SystemOneProviderRegistry() *systemoneprovider.Registry {
	s.mu.RLock()
	reg := s.systemOneRegistry
	s.mu.RUnlock()
	if reg != nil {
		return reg
	}
	return systemoneprovider.DefaultRegistry()
}

// initSystemOneRuntime constructs the Hub's System One runtime using the
// configured MaxEvalTime ceiling, store-backed InstanceSource, and
// pkg/secret-backed CredentialSource.
func (s *Server) initSystemOneRuntime() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rebuildSystemOneRuntimeLocked()
}

func (s *Server) rebuildSystemOneRuntimeLocked() {
	reg := s.systemOneRegistry
	if reg == nil {
		reg = systemoneprovider.DefaultRegistry()
	}
	credSrc := &hubSystemOneCredentialSource{srv: s}
	rt := systemoneruntime.New(
		systemoneruntime.Config{
			Registry:    reg,
			Instances:   &hubSystemOneInstanceSource{srv: s},
			Credentials: credSrc.BearerToken,
			MaxEvalTime: resolveSystemOneMaxEvalTime(s.config),
			Logger:      logging.Subsystem("hub.system-one"),
		},
		s.systemOneRuntimeOpts...,
	)
	if s.systemOneRuntime != nil {
		s.systemOneRuntime.CloseIdleConnections()
	}
	s.systemOneRuntime = rt
}

// SystemOneRuntime returns the Hub's shared System One evaluation runtime,
// lazily initializing it if needed.
func (s *Server) SystemOneRuntime() *systemoneruntime.Runtime {
	s.mu.RLock()
	rt := s.systemOneRuntime
	s.mu.RUnlock()
	if rt != nil {
		return rt
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.systemOneRuntime == nil {
		s.rebuildSystemOneRuntimeLocked()
	}
	return s.systemOneRuntime
}

// storeSystemOneInstanceToRuntime converts a persisted store record into the
// credential-free runtime.Instance representation.
func storeSystemOneInstanceToRuntime(inst *store.SystemOneInstance) systemoneruntime.Instance {
	if inst == nil {
		return systemoneruntime.Instance{}
	}
	var opts map[string]string
	if len(inst.Options) > 0 {
		opts = make(map[string]string, len(inst.Options))
		for k, v := range inst.Options {
			opts[k] = v
		}
	}
	return systemoneruntime.Instance{
		ID:       inst.ID,
		Name:     inst.Name,
		Adapter:  inst.Adapter,
		Model:    inst.Model,
		Endpoint: inst.Endpoint,
		Location: inst.Location,
		Auth: systemoneruntime.InstanceAuth{
			Kind:       inst.Auth.Kind,
			SecretName: inst.Auth.SecretName,
			HeaderName: inst.Auth.HeaderName,
			Scopes:     slices.Clone(inst.Auth.Scopes),
			Audience:   inst.Auth.Audience,
		},
		Options: opts,
		Limits: systemoneruntime.InstanceLimits{
			MaxInFlight:    inst.Limits.MaxInFlight,
			RatePerSec:     inst.Limits.RatePerSec,
			MaxInputTokens: inst.Limits.MaxInputTokens,
			CallTimeout:    inst.Limits.CallTimeout,
		},
		Enabled:  inst.Enabled,
		Revision: inst.Revision,
	}
}

// hubSystemOneInstanceSource adapts store.SystemOneInstanceStore to
// systemoneruntime.InstanceSource.
type hubSystemOneInstanceSource struct {
	srv *Server
}

func (src *hubSystemOneInstanceSource) GetInstance(ctx context.Context, name string) (systemoneruntime.Instance, bool, error) {
	if src.srv == nil || src.srv.store == nil {
		return systemoneruntime.Instance{}, false, errors.New("system_one: instance store not configured")
	}
	inst, err := src.srv.store.GetSystemOneInstanceByName(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return systemoneruntime.Instance{}, false, nil
		}
		return systemoneruntime.Instance{}, false, err
	}
	return storeSystemOneInstanceToRuntime(inst), true, nil
}

func (src *hubSystemOneInstanceSource) ListInstances(ctx context.Context) ([]systemoneruntime.Instance, error) {
	if src.srv == nil || src.srv.store == nil {
		return nil, errors.New("system_one: instance store not configured")
	}
	var out []systemoneruntime.Instance
	cursor := ""
	seen := make(map[string]struct{})
	for {
		res, err := src.srv.store.ListSystemOneInstances(ctx, store.SystemOneInstanceFilter{}, store.ListOptions{
			Limit:          systemOneInstancesListPageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return nil, err
		}
		for i := range res.Items {
			out = append(out, storeSystemOneInstanceToRuntime(&res.Items[i]))
		}
		if res.NextCursor == "" {
			return out, nil
		}
		if _, dup := seen[res.NextCursor]; dup {
			return nil, fmt.Errorf("system_one: pagination returned repeated cursor %q", res.NextCursor)
		}
		seen[res.NextCursor] = struct{}{}
		cursor = res.NextCursor
	}
}

// hubSystemOneCredentialSource resolves hub-scoped internal provider
// credentials through pkg/secret. A missing, deleted, or rejected secret (or a
// stored value that is empty or invalid as an HTTP bearer token) returns
// egress.ErrAuth (classified by runtime as Failed with reason "auth"); any
// other secret-backend fault returns egress.ErrCredentialUnavailable
// (classified as Failed with reason "unavailable"); context cancellation and
// deadline errors pass through unchanged.
type hubSystemOneCredentialSource struct {
	srv *Server
}

// BearerToken resolves the provider API key for inst from the Hub's secret
// backend. Missing, deleted, or rejected secrets return egress.ErrAuth
// (reason "auth"); other backend faults return egress.ErrCredentialUnavailable
// (reason "unavailable"). Secret values and raw backend errors are never
// included in returned errors.
func (c *hubSystemOneCredentialSource) BearerToken(ctx context.Context, inst systemoneruntime.Instance) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if inst.Auth.Kind != "" && inst.Auth.Kind != store.SystemOneAuthKindAPIKey {
		return "", egress.ErrAuth
	}
	secretName := strings.TrimSpace(inst.Auth.SecretName)
	if secretName == "" || !strings.HasPrefix(secretName, systemOneSecretPrefix) {
		return "", egress.ErrAuth
	}
	if c.srv == nil || c.srv.secretBackend == nil {
		return "", egress.ErrCredentialUnavailable
	}

	sv, err := c.srv.secretBackend.Get(ctx, secretName, store.ScopeHub, c.srv.hubID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		if errors.Is(err, secret.ErrNoSecretBackend) {
			return "", egress.ErrCredentialUnavailable
		}
		if isSystemOneSecretAuthError(err) {
			return "", egress.ErrAuth
		}
		return "", egress.ErrCredentialUnavailable
	}
	if sv == nil {
		return "", egress.ErrAuth
	}
	if sv.SecretType != "" && sv.SecretType != store.SecretTypeInternal {
		return "", egress.ErrAuth
	}
	if !isValidSystemOneAPIKey(sv.Value) {
		return "", egress.ErrAuth
	}
	return sv.Value, nil
}

// isSystemOneSecretAuthError reports whether err from SecretBackend.Get
// indicates a missing, deleted, disabled/destroyed, or access-rejected secret
// (mapped to egress.ErrAuth -> reason "auth") rather than a transient or
// unconfigured backend fault (mapped to egress.ErrCredentialUnavailable ->
// reason "unavailable").
func isSystemOneSecretAuthError(err error) bool {
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	switch status.Code(err) {
	case codes.NotFound, codes.PermissionDenied, codes.Unauthenticated, codes.FailedPrecondition:
		return true
	}
	var permErr *secret.PermissionError
	return errors.As(err, &permErr)
}
