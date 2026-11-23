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

package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

const (
	systemOneAdaptersPath      = "/api/v1/admin/system-one/adapters"
	systemOneInstancesBasePath = "/api/v1/admin/system-one/instances"
)

// SystemOneService handles administrative operations on Hub-managed System One
// provider instances (/api/v1/admin/system-one/instances) and adapter
// capability discovery (/api/v1/admin/system-one/adapters).
type SystemOneService interface {
	// ListAdapters returns non-secret capability metadata for registered
	// System One provider adapters.
	ListAdapters(ctx context.Context) (*ListSystemOneAdaptersResponse, error)

	// CreateInstance registers a new System One provider instance.
	CreateInstance(ctx context.Context, req *CreateSystemOneInstanceRequest) (*SystemOneInstance, error)

	// ListInstances lists configured System One provider instances.
	ListInstances(ctx context.Context, opts *ListSystemOneInstancesOptions) (*ListSystemOneInstancesResponse, error)

	// GetInstance returns the credential-free representation of a named instance.
	GetInstance(ctx context.Context, name string) (*SystemOneInstance, error)

	// DeleteInstance deletes a named instance and removes its managed secret.
	DeleteInstance(ctx context.Context, name string) error

	// ProbeInstance runs an admin probe or model listing (Models: true) against
	// a named instance.
	ProbeInstance(ctx context.Context, name string, req *ProbeSystemOneInstanceRequest) (*ProbeSystemOneInstanceResponse, error)
}

type systemOneService struct {
	c *client
}

// SystemOneInstanceLimitsDTO is the JSON wire representation of per-instance
// operational bounds on the System One admin API. CallTimeout is formatted as a
// Go duration string (for example "5s") when positive.
type SystemOneInstanceLimitsDTO struct {
	MaxInFlight    int     `json:"maxInFlight,omitempty"`
	RatePerSec     float64 `json:"ratePerSec,omitempty"`
	MaxInputTokens int     `json:"maxInputTokens,omitempty"`
	CallTimeout    string  `json:"callTimeout,omitempty"`
}

// SystemOneSecretRefDTO is the credential-free JSON wire representation of a
// named Hub/project secret reference on a System One provider instance
// (PV3 Addendum F §§F2, F7).
type SystemOneSecretRefDTO struct {
	Name  string `json:"name"`
	Scope string `json:"scope,omitempty"`
}

// CreateSystemOneInstanceAuthRequest carries credential settings for creating a
// System One provider instance. APIKey is write-only: it is stored as a
// hub-scoped internal secret and never returned in any response or log.
type CreateSystemOneInstanceAuthRequest struct {
	Kind       string                 `json:"kind,omitempty"`
	APIKey     string                 `json:"apiKey,omitempty"`
	SecretRef  *SystemOneSecretRefDTO `json:"secretRef,omitempty"`
	HeaderName string                 `json:"headerName,omitempty"`
	Scopes     []string               `json:"scopes,omitempty"`
	Audience   string                 `json:"audience,omitempty"`
}

// LogValue redacts APIKey if the request struct is ever passed to slog.
func (a CreateSystemOneInstanceAuthRequest) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("kind", a.Kind),
		slog.Bool("has_api_key", a.APIKey != ""),
		slog.String("header_name", a.HeaderName),
	)
}

// CreateSystemOneInstanceRequest is the request body for
// POST /api/v1/admin/system-one/instances.
type CreateSystemOneInstanceRequest struct {
	Name                string                              `json:"name"`
	Adapter             string                              `json:"adapter"`
	Model               string                              `json:"model,omitempty"`
	Endpoint            string                              `json:"endpoint,omitempty"`
	Location            string                              `json:"location,omitempty"`
	APIKey              string                              `json:"apiKey,omitempty"`
	Auth                *CreateSystemOneInstanceAuthRequest `json:"auth,omitempty"`
	Options             map[string]string                   `json:"options,omitempty"`
	Limits              *SystemOneInstanceLimitsDTO         `json:"limits,omitempty"`
	AllowPrivateNetwork bool                                `json:"allowPrivateNetwork,omitempty"`
	Enabled             *bool                               `json:"enabled,omitempty"`
}

// LogValue redacts credential fields if the request struct is ever passed to slog.
func (r CreateSystemOneInstanceRequest) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", r.Name),
		slog.String("adapter", r.Adapter),
		slog.String("model", r.Model),
		slog.String("endpoint", r.Endpoint),
		slog.Bool("has_api_key", r.APIKey != "" || (r.Auth != nil && r.Auth.APIKey != "")),
	)
}

// SystemOneInstanceAuthDTO is the credential-free JSON wire representation of
// authentication metadata on a System One provider instance response.
// QuotaProject, CredentialType, and ServiceAccountEmail are read-only metadata
// fields populated at response time for keyless ADC instances and never stored
// as secrets.
type SystemOneInstanceAuthDTO struct {
	Kind                string                 `json:"kind"`
	SecretName          string                 `json:"secretName,omitempty"`
	SecretRef           *SystemOneSecretRefDTO `json:"secretRef,omitempty"`
	HeaderName          string                 `json:"headerName,omitempty"`
	Scopes              []string               `json:"scopes,omitempty"`
	Audience            string                 `json:"audience,omitempty"`
	QuotaProject        string                 `json:"quotaProject,omitempty"`
	CredentialType      string                 `json:"credentialType,omitempty"`
	ServiceAccountEmail string                 `json:"serviceAccountEmail,omitempty"`
}

// SystemOneInstance is the credential-free JSON wire representation of a
// configured System One provider instance. Auth carries only reference metadata
// (such as SecretName = "systemone.instance.<id>" or non-secret ADC metadata);
// secret and token values are never returned.
type SystemOneInstance struct {
	ID                  string                     `json:"id"`
	Name                string                     `json:"name"`
	Adapter             string                     `json:"adapter"`
	Model               string                     `json:"model,omitempty"`
	Endpoint            string                     `json:"endpoint"`
	Location            string                     `json:"location,omitempty"`
	Auth                SystemOneInstanceAuthDTO   `json:"auth"`
	Options             map[string]string          `json:"options,omitempty"`
	Limits              SystemOneInstanceLimitsDTO `json:"limits,omitempty"`
	AllowPrivateNetwork bool                       `json:"allowPrivateNetwork,omitempty"`
	Enabled             bool                       `json:"enabled"`
	Revision            int64                      `json:"revision"`
	CreatedBy           string                     `json:"createdBy,omitempty"`
	UpdatedBy           string                     `json:"updatedBy,omitempty"`
	CreatedAt           time.Time                  `json:"createdAt"`
	UpdatedAt           time.Time                  `json:"updatedAt"`
}

// ListSystemOneInstancesOptions configures filtering and pagination on
// GET /api/v1/admin/system-one/instances.
type ListSystemOneInstancesOptions struct {
	Adapter string
	Enabled *bool
	Cursor  string
	Limit   int
}

// ListSystemOneInstancesResponse is the response body for
// GET /api/v1/admin/system-one/instances.
type ListSystemOneInstancesResponse struct {
	Instances  []SystemOneInstance `json:"instances"`
	NextCursor string              `json:"nextCursor,omitempty"`
	TotalCount int                 `json:"totalCount"`
}

// SystemOneAdapterAuthMode describes one authentication mode supported by a
// registered System One adapter.
type SystemOneAdapterAuthMode struct {
	Mode                  string `json:"mode"`
	AuthKind              string `json:"authKind"`
	TransportScheme       string `json:"transportScheme,omitempty"`
	CustomEndpointAllowed bool   `json:"customEndpointAllowed"`
	ScopesRequired        bool   `json:"scopesRequired,omitempty"`
}

// SystemOneAdapterCapability is the non-secret capability metadata for a
// registered System One provider adapter returned by
// GET /api/v1/admin/system-one/adapters.
type SystemOneAdapterCapability struct {
	ID                   string                     `json:"id"`
	Version              string                     `json:"version,omitempty"`
	DisplayName          string                     `json:"displayName,omitempty"`
	ProviderRetention    string                     `json:"providerRetention,omitempty"`
	DefaultEndpoint      string                     `json:"defaultEndpoint,omitempty"`
	ModelRequired        bool                       `json:"modelRequired"`
	SupportsModelListing bool                       `json:"supportsModelListing"`
	AuthModes            []SystemOneAdapterAuthMode `json:"authModes"`
	AllowedSecretNames   []string                   `json:"allowedSecretNames,omitempty"`
	DefaultSecretName    string                     `json:"defaultSecretName,omitempty"`
	AllowedSecretScopes  []string                   `json:"allowedSecretScopes,omitempty"`
	DefaultSecretScope   string                     `json:"defaultSecretScope,omitempty"`
}

// ListSystemOneAdaptersResponse is the response body for
// GET /api/v1/admin/system-one/adapters.
type ListSystemOneAdaptersResponse struct {
	Adapters []SystemOneAdapterCapability `json:"adapters"`
}

// ProbeSystemOneInput holds the synthetic probe content passed to
// POST /api/v1/admin/system-one/instances/{name}/probe.
type ProbeSystemOneInput struct {
	Text string `json:"text"`
}

// LogValue omits probe text if the struct is ever passed to slog.
func (p ProbeSystemOneInput) LogValue() slog.Value {
	return slog.GroupValue(slog.Int("text_bytes", len(p.Text)))
}

// ProbeSystemOneInstanceRequest is the request body for
// POST /api/v1/admin/system-one/instances/{name}/probe.
type ProbeSystemOneInstanceRequest struct {
	Models    bool                 `json:"models,omitempty"`
	Project   string               `json:"project,omitempty"`
	Kind      string               `json:"kind,omitempty"`
	RuleID    string               `json:"ruleId,omitempty"`
	Params    map[string]string    `json:"params,omitempty"`
	Spec      json.RawMessage      `json:"spec,omitempty"`
	Probe     *ProbeSystemOneInput `json:"probe,omitempty"`
	ProbeText *string              `json:"probeText,omitempty"`
}

// LogValue omits probe text and spec payloads if the struct is ever passed to slog.
func (r ProbeSystemOneInstanceRequest) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("models", r.Models),
		slog.String("project", r.Project),
		slog.String("kind", r.Kind),
		slog.String("rule_id", r.RuleID),
	)
}

// ProbeSystemOneDecision carries the typed consumer projection result when a
// probe evaluation reaches ClassSuccess.
type ProbeSystemOneDecision struct {
	Kind   spec.Kind `json:"kind"`
	Bool   *bool     `json:"bool,omitempty"`
	String *string   `json:"string,omitempty"`
}

// ProbeSystemOneInstanceResponse is the response body for
// POST /api/v1/admin/system-one/instances/{name}/probe.
type ProbeSystemOneInstanceResponse struct {
	Class      systemoneruntime.Class       `json:"class"`
	Reason     string                       `json:"reason,omitempty"`
	Models     []string                     `json:"models,omitempty"`
	Decision   *ProbeSystemOneDecision      `json:"decision,omitempty"`
	Evaluation *systemoneruntime.Evaluation `json:"evaluation,omitempty"`
}

func systemOneInstancePath(name string) string {
	return systemOneInstancesBasePath + "/" + url.PathEscape(name)
}

func (s *systemOneService) ListAdapters(ctx context.Context) (*ListSystemOneAdaptersResponse, error) {
	resp, err := s.c.get(ctx, systemOneAdaptersPath, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListSystemOneAdaptersResponse](resp)
}

func (s *systemOneService) CreateInstance(ctx context.Context, req *CreateSystemOneInstanceRequest) (*SystemOneInstance, error) {
	if req == nil {
		return nil, errors.New("create instance request is required")
	}
	resp, err := s.c.postNoRetry(ctx, systemOneInstancesBasePath, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[SystemOneInstance](resp)
}

func (s *systemOneService) ListInstances(ctx context.Context, opts *ListSystemOneInstancesOptions) (*ListSystemOneInstancesResponse, error) {
	query := url.Values{}
	if opts != nil {
		if opts.Adapter != "" {
			query.Set("adapter", opts.Adapter)
		}
		if opts.Enabled != nil {
			query.Set("enabled", strconv.FormatBool(*opts.Enabled))
		}
		if opts.Cursor != "" {
			query.Set("cursor", opts.Cursor)
		}
		if opts.Limit > 0 {
			query.Set("limit", strconv.Itoa(opts.Limit))
		}
	}
	resp, err := s.c.getWithQuery(ctx, systemOneInstancesBasePath, query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ListSystemOneInstancesResponse](resp)
}

func (s *systemOneService) GetInstance(ctx context.Context, name string) (*SystemOneInstance, error) {
	if name == "" {
		return nil, errors.New("instance name is required")
	}
	resp, err := s.c.get(ctx, systemOneInstancePath(name), nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[SystemOneInstance](resp)
}

func (s *systemOneService) DeleteInstance(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("instance name is required")
	}
	resp, err := s.c.delete(ctx, systemOneInstancePath(name), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

func (s *systemOneService) ProbeInstance(ctx context.Context, name string, req *ProbeSystemOneInstanceRequest) (*ProbeSystemOneInstanceResponse, error) {
	if name == "" {
		return nil, errors.New("instance name is required")
	}
	if req == nil {
		return nil, errors.New("probe request is required")
	}
	resp, err := s.c.postNoRetry(ctx, systemOneInstancePath(name)+"/probe", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ProbeSystemOneInstanceResponse](resp)
}
