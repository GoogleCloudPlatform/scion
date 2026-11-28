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

package store

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// SystemOneAuthKind enumerates the credential kinds supported on a System One
// provider instance (runtime design §4.2). All five values are persisted in the
// schema enum from day one so adding adapter support needs no DB migration.
const (
	SystemOneAuthKindNone              = "none"
	SystemOneAuthKindAPIKey            = "api_key"
	SystemOneAuthKindGoogleAccessToken = "google_access_token"
	SystemOneAuthKindGoogleIDToken     = "google_id_token"
	SystemOneAuthKindAzureEntra        = "azure_entra"
)

// IsValidSystemOneAuthKind reports whether kind is a supported System One
// credential kind.
func IsValidSystemOneAuthKind(kind string) bool {
	switch kind {
	case SystemOneAuthKindNone,
		SystemOneAuthKindAPIKey,
		SystemOneAuthKindGoogleAccessToken,
		SystemOneAuthKindGoogleIDToken,
		SystemOneAuthKindAzureEntra:
		return true
	default:
		return false
	}
}

// IsValidSystemOneInstanceName reports whether name is a canonical slug
// suitable for a System One instance (1..63 lowercase ASCII letters/digits
// separated by single hyphens, e.g. "typesafe-prod").
func IsValidSystemOneInstanceName(name string) bool {
	return len(name) > 0 && len(name) <= 63 && api.Slugify(name) == name
}

// SystemOneInstanceAuth describes how the Hub authenticates to a System One
// provider instance. Only reference identifiers (such as a Hub secret name or
// GCP service account ID) are stored here — never secret values.
type SystemOneInstanceAuth struct {
	Kind       string   `json:"kind"`
	SecretName string   `json:"secretName,omitempty"`
	HeaderName string   `json:"headerName,omitempty"`
	Scopes     []string `json:"scopes,omitempty"`
	Audience   string   `json:"audience,omitempty"`
}

// SystemOneInstanceLimits holds per-instance operational bounds (runtime
// design §4.2). Zero values mean "use runtime/adapter default".
type SystemOneInstanceLimits struct {
	MaxInFlight    int           `json:"maxInFlight,omitempty"`
	RatePerSec     float64       `json:"ratePerSec,omitempty"`
	MaxInputTokens int           `json:"maxInputTokens,omitempty"`
	CallTimeout    time.Duration `json:"callTimeout,omitempty"`
}

// SystemOneInstance is a Hub-scoped System One provider instance record.
type SystemOneInstance struct {
	ID                  string                  `json:"id"`
	Name                string                  `json:"name"`
	Adapter             string                  `json:"adapter"`
	Model               string                  `json:"model,omitempty"`
	Endpoint            string                  `json:"endpoint"`
	Location            string                  `json:"location,omitempty"`
	Auth                SystemOneInstanceAuth   `json:"auth"`
	Options             map[string]string       `json:"options,omitempty"`
	Limits              SystemOneInstanceLimits `json:"limits,omitempty"`
	AllowPrivateNetwork bool                    `json:"allowPrivateNetwork,omitempty"`
	Enabled             bool                    `json:"enabled"`
	Revision            int64                   `json:"revision"`
	CreatedBy           string                  `json:"createdBy,omitempty"`
	UpdatedBy           string                  `json:"updatedBy,omitempty"`
	CreatedAt           time.Time               `json:"createdAt"`
	UpdatedAt           time.Time               `json:"updatedAt"`
}

// Clone returns a deep copy of inst (copying Auth.Scopes and Options).
func (inst *SystemOneInstance) Clone() *SystemOneInstance {
	if inst == nil {
		return nil
	}
	cp := *inst
	if len(inst.Auth.Scopes) > 0 {
		cp.Auth.Scopes = slices.Clone(inst.Auth.Scopes)
	} else {
		cp.Auth.Scopes = nil
	}
	if len(inst.Options) > 0 {
		cp.Options = make(map[string]string, len(inst.Options))
		for k, v := range inst.Options {
			cp.Options[k] = v
		}
	} else {
		cp.Options = nil
	}
	return &cp
}

// SystemOneRuntimeConfigChanged reports whether any evaluator-relevant setting
// differs between prev and next: Adapter, Model, Endpoint, Auth, Options, or
// Limits. UpdateSystemOneInstance bumps Revision whenever this returns true.
func SystemOneRuntimeConfigChanged(prev, next *SystemOneInstance) bool {
	if prev == nil || next == nil {
		return prev != next
	}
	if prev.Adapter != next.Adapter ||
		prev.Model != next.Model ||
		prev.Endpoint != next.Endpoint {
		return true
	}
	if !systemOneAuthEqual(prev.Auth, next.Auth) {
		return true
	}
	if !stringMapEqual(prev.Options, next.Options) {
		return true
	}
	return !systemOneLimitsEqual(prev.Limits, next.Limits)
}

func systemOneAuthEqual(a, b SystemOneInstanceAuth) bool {
	kindA := a.Kind
	if kindA == "" {
		kindA = SystemOneAuthKindAPIKey
	}
	kindB := b.Kind
	if kindB == "" {
		kindB = SystemOneAuthKindAPIKey
	}
	if kindA != kindB ||
		a.SecretName != b.SecretName ||
		a.HeaderName != b.HeaderName ||
		a.Audience != b.Audience {
		return false
	}
	return slices.Equal(a.Scopes, b.Scopes)
}

func systemOneLimitsEqual(a, b SystemOneInstanceLimits) bool {
	return a.MaxInFlight == b.MaxInFlight &&
		math.Float64bits(a.RatePerSec) == math.Float64bits(b.RatePerSec) &&
		a.MaxInputTokens == b.MaxInputTokens &&
		a.CallTimeout == b.CallTimeout
}

func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		if vb, ok := b[k]; !ok || va != vb {
			return false
		}
	}
	return true
}

// SystemOneInstanceFilter filters ListSystemOneInstances queries.
type SystemOneInstanceFilter struct {
	Adapter string
	Enabled *bool
}

// SystemOneInstanceProjectRef is the minimal project identity returned when
// listing projects that allowlist a System One provider instance.
type SystemOneInstanceProjectRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SystemOneInstanceStore defines persistence operations for Hub-scoped System
// One provider instances.
type SystemOneInstanceStore interface {
	// CreateSystemOneInstance creates a new System One instance with Revision=1.
	// Populates ID (if empty), Revision, CreatedAt, and UpdatedAt on inst.
	// Returns ErrInvalidInput for invalid slug/auth/limits or missing required
	// fields, and ErrAlreadyExists if ID or Name collides with an existing row.
	CreateSystemOneInstance(ctx context.Context, inst *SystemOneInstance) error

	// GetSystemOneInstance retrieves a System One instance by UUID.
	// Returns ErrNotFound if not found or id is not a valid UUID.
	GetSystemOneInstance(ctx context.Context, id string) (*SystemOneInstance, error)

	// GetSystemOneInstanceByName retrieves a System One instance by its unique
	// canonical slug Name. Returns ErrNotFound if not found.
	GetSystemOneInstanceByName(ctx context.Context, name string) (*SystemOneInstance, error)

	// ListSystemOneInstances lists System One instances ordered by Name
	// ascending.
	ListSystemOneInstances(ctx context.Context, filter SystemOneInstanceFilter, opts ListOptions) (*ListResult[SystemOneInstance], error)

	// UpdateSystemOneInstance updates an existing System One instance (resolved
	// by ID when non-empty, otherwise by Name).
	//
	// Name is immutable once created: supplying a different non-empty Name
	// returns ErrInvalidInput so project allowlists cannot drift to a different
	// instance.
	//
	// If inst.Revision > 0, it must match the stored Revision or
	// ErrRevisionConflict is returned. Revision is incremented by 1 whenever
	// any of Adapter, Model, Endpoint, Auth, Options, or Limits changes;
	// changes only to metadata/toggles (Enabled, Location, AllowPrivateNetwork,
	// UpdatedBy) preserve Revision. Updates inst.Revision and inst.UpdatedAt.
	UpdateSystemOneInstance(ctx context.Context, inst *SystemOneInstance) error

	// DeleteSystemOneInstance atomically prunes the instance's Name from every
	// project's allowlist and deletes the System One instance by UUID in a
	// single transaction. Returns ErrNotFound if no row with that ID exists.
	DeleteSystemOneInstance(ctx context.Context, id string) error

	// DeleteSystemOneInstanceByName atomically prunes the instance's Name from
	// every project's allowlist and deletes the System One instance by its
	// unique slug Name in a single transaction. Returns ErrNotFound if no row
	// with that Name exists.
	DeleteSystemOneInstanceByName(ctx context.Context, name string) error

	// ListSystemOneInstanceProjects returns the minimal references (ID, Name) of
	// all projects whose System One allowlist contains instanceName, ordered by
	// Name ascending then ID ascending. Returns an empty slice when no project
	// allowlists the instance.
	ListSystemOneInstanceProjects(ctx context.Context, instanceName string) ([]SystemOneInstanceProjectRef, error)
}
