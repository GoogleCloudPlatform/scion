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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	systemoneprovider "github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

const (
	maxCreateSystemOneInstanceBodyBytes = 64 * 1024
	maxProbeSystemOneInstanceBodyBytes  = 256 * 1024
	maxSystemOneAPIKeyBytes             = 8 * 1024
	maxSystemOneModelBytes              = 128
	maxSystemOneRuleIDBytes             = 128
	maxSystemOneLocationBytes           = 128
	maxSystemOneOptionsEntries          = 32
	maxSystemOneOptionKeyBytes          = 64
	maxSystemOneOptionValueBytes        = 512
)

var forbiddenSystemOneOptionSubstrings = []string{
	"secret",
	"token",
	"password",
	"passwd",
	"apikey",
	"api_key",
	"api-key",
	"privatekey",
	"private_key",
	"private-key",
	"credential",
	"bearer",
	"authorization",
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

// CreateSystemOneInstanceAuthRequest carries credential settings for creating a
// System One provider instance. APIKey is write-only: it is stored as a
// hub-scoped internal secret and never returned in any response or log.
type CreateSystemOneInstanceAuthRequest struct {
	Kind       string   `json:"kind,omitempty"`
	APIKey     string   `json:"apiKey,omitempty"`
	HeaderName string   `json:"headerName,omitempty"`
	Scopes     []string `json:"scopes,omitempty"`
	Audience   string   `json:"audience,omitempty"`
}

// LogValue redacts APIKey if the request struct is ever passed to slog.
func (a CreateSystemOneInstanceAuthRequest) LogValue() slog.Value {
	hasKey := a.APIKey != ""
	return slog.GroupValue(
		slog.String("kind", a.Kind),
		slog.Bool("has_api_key", hasKey),
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

// SystemOneInstanceResponse is the credential-free JSON wire representation of
// a configured System One provider instance. Auth carries only reference
// metadata (such as SecretName = "systemone.instance.<id>"); secret values are
// never returned.
type SystemOneInstanceResponse struct {
	ID                  string                      `json:"id"`
	Name                string                      `json:"name"`
	Adapter             string                      `json:"adapter"`
	Model               string                      `json:"model,omitempty"`
	Endpoint            string                      `json:"endpoint"`
	Location            string                      `json:"location,omitempty"`
	Auth                store.SystemOneInstanceAuth `json:"auth"`
	Options             map[string]string           `json:"options,omitempty"`
	Limits              SystemOneInstanceLimitsDTO  `json:"limits,omitempty"`
	AllowPrivateNetwork bool                        `json:"allowPrivateNetwork,omitempty"`
	Enabled             bool                        `json:"enabled"`
	Revision            int64                       `json:"revision"`
	CreatedBy           string                      `json:"createdBy,omitempty"`
	UpdatedBy           string                      `json:"updatedBy,omitempty"`
	CreatedAt           time.Time                   `json:"createdAt"`
	UpdatedAt           time.Time                   `json:"updatedAt"`
}

// ListSystemOneInstancesResponse is the response body for
// GET /api/v1/admin/system-one/instances.
type ListSystemOneInstancesResponse struct {
	Instances  []SystemOneInstanceResponse `json:"instances"`
	NextCursor string                      `json:"nextCursor,omitempty"`
	TotalCount int                         `json:"totalCount"`
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
//
// Callers supply either:
//   - Models: true — queries provider.ModelLister via Runtime.ListModels; or
//   - Spec + Probe (or ProbeText) — compiles an inline portable Spec or rule
//     Envelope against the built-in probe environment and evaluates it once
//     with no cache against the named instance.
type ProbeSystemOneInstanceRequest struct {
	Models    bool                 `json:"models,omitempty"`
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

func toSystemOneInstanceResponse(inst *store.SystemOneInstance) SystemOneInstanceResponse {
	if inst == nil {
		return SystemOneInstanceResponse{}
	}
	var opts map[string]string
	if len(inst.Options) > 0 {
		opts = make(map[string]string, len(inst.Options))
		for k, v := range inst.Options {
			opts[k] = v
		}
	}
	var limits SystemOneInstanceLimitsDTO
	limits.MaxInFlight = inst.Limits.MaxInFlight
	limits.RatePerSec = inst.Limits.RatePerSec
	limits.MaxInputTokens = inst.Limits.MaxInputTokens
	if inst.Limits.CallTimeout > 0 {
		limits.CallTimeout = inst.Limits.CallTimeout.String()
	}
	return SystemOneInstanceResponse{
		ID:       inst.ID,
		Name:     inst.Name,
		Adapter:  inst.Adapter,
		Model:    inst.Model,
		Endpoint: inst.Endpoint,
		Location: inst.Location,
		Auth: store.SystemOneInstanceAuth{
			Kind:       inst.Auth.Kind,
			SecretName: inst.Auth.SecretName,
			HeaderName: inst.Auth.HeaderName,
			Scopes:     slices.Clone(inst.Auth.Scopes),
			Audience:   inst.Auth.Audience,
		},
		Options:             opts,
		Limits:              limits,
		AllowPrivateNetwork: inst.AllowPrivateNetwork,
		Enabled:             inst.Enabled,
		Revision:            inst.Revision,
		CreatedBy:           inst.CreatedBy,
		UpdatedBy:           inst.UpdatedBy,
		CreatedAt:           inst.CreatedAt,
		UpdatedAt:           inst.UpdatedAt,
	}
}

func (s *Server) requireSystemOneInstancePermission(
	w http.ResponseWriter,
	r *http.Request,
	resourceID, permission, action string,
) (UserIdentity, bool) {
	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return nil, false
	}
	user, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return nil, false
	}
	if s.authzService == nil {
		Forbidden(w)
		return nil, false
	}
	if resourceID == "" {
		resourceID = "hub"
	}
	decision := s.authzService.Decide(r.Context(), AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   hubScopedResource("system_one_instance", resourceID),
		Action:     Action(action),
		Permission: permission,
	})
	if !decision.Allowed {
		Forbidden(w)
		return nil, false
	}
	return user, true
}

// handleAdminSystemOneInstances serves GET (list) and POST (create) on
// /api/v1/admin/system-one/instances.
//
// Route metadata (RouteHubAdmin, permission system_one_instance.read,
// SessionOnly) gates every request before this handler runs; POST additionally
// checks system_one_instance.create inline.
func (s *Server) handleAdminSystemOneInstances(w http.ResponseWriter, r *http.Request) {
	if !s.experimentEnabled(experiments.SystemOne) {
		NotFound(w, "route")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listSystemOneInstances(w, r)
	case http.MethodPost:
		user, ok := s.requireSystemOneInstancePermission(w, r, "hub", "system_one_instance.create", "create")
		if !ok {
			return
		}
		s.createSystemOneInstance(w, r, user)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

// handleAdminSystemOneInstanceByName serves GET and DELETE on
// /api/v1/admin/system-one/instances/{name}, and POST on
// /api/v1/admin/system-one/instances/{name}/probe.
func (s *Server) handleAdminSystemOneInstanceByName(w http.ResponseWriter, r *http.Request) {
	if !s.experimentEnabled(experiments.SystemOne) {
		NotFound(w, "route")
		return
	}
	name, action := extractAction(r, "/api/v1/admin/system-one/instances")
	if name == "" {
		if action != "" {
			NotFound(w, "route")
			return
		}
		BadRequest(w, "instance name is required")
		return
	}

	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			s.getSystemOneInstance(w, r, name)
		case http.MethodDelete:
			user, ok := s.requireSystemOneInstancePermission(w, r, name, "system_one_instance.delete", "delete")
			if !ok {
				return
			}
			s.deleteSystemOneInstance(w, r, name, user)
		default:
			MethodNotAllowed(w, http.MethodGet, http.MethodDelete)
		}
	case "probe":
		if r.Method != http.MethodPost {
			MethodNotAllowed(w, http.MethodPost)
			return
		}
		user, ok := s.requireSystemOneInstancePermission(w, r, name, "system_one_instance.execute", "execute")
		if !ok {
			return
		}
		s.probeSystemOneInstance(w, r, name, user)
	default:
		NotFound(w, "route")
	}
}

func readBoundedJSONStrict(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) bool {
	if r.Body == nil {
		ValidationError(w, "empty request body", nil)
		return false
	}
	limited := http.MaxBytesReader(w, r.Body, maxBytes)
	defer func() { _ = limited.Close() }()

	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, ErrCodeValidationError,
				fmt.Sprintf("request body exceeds maximum size of %d bytes", maxBytes), nil)
			return false
		}
		ValidationError(w, safeSystemOneJSONError(err), nil)
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, ErrCodeValidationError,
				fmt.Sprintf("request body exceeds maximum size of %d bytes", maxBytes), nil)
			return false
		}
		ValidationError(w, "request body must contain a single JSON object", nil)
		return false
	}
	return true
}

// safeSystemOneJSONError formats JSON decode errors without echoing raw string
// values from the request body (such as API keys or probe text).
func safeSystemOneJSONError(err error) string {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "invalid or truncated JSON request body"
	}
	var synErr *json.SyntaxError
	if errors.As(err, &synErr) {
		return fmt.Sprintf("malformed JSON at byte offset %d", synErr.Offset)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		if typeErr.Field != "" {
			return fmt.Sprintf("invalid JSON value for field %q (expected %s)", typeErr.Field, typeErr.Type.String())
		}
		return fmt.Sprintf("invalid JSON value (expected %s)", typeErr.Type.String())
	}
	const unknownFieldPrefix = "json: unknown field "
	if msg := err.Error(); strings.HasPrefix(msg, unknownFieldPrefix) {
		return msg
	}
	return "invalid JSON request body"
}

func defaultSystemOneAdapterEndpoint(adapter systemoneprovider.Adapter) string {
	if defaulter, ok := adapter.(systemoneprovider.EndpointDefaulter); ok {
		return strings.TrimSpace(defaulter.DefaultEndpoint())
	}
	return ""
}

func safeSystemOneAdapterConfigMessage(err error) string {
	if pe, ok := systemoneprovider.AsProviderError(err); ok && pe.Message != "" && !hasControlChars(pe.Message) {
		return pe.Message
	}
	return "adapter rejected instance configuration"
}

func validateSystemOneEndpoint(raw, defaultEndpoint, adapterName string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		trimmed = strings.TrimSpace(defaultEndpoint)
	}
	if trimmed == "" {
		return "", fmt.Errorf("endpoint is required for adapter %q", adapterName)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", errors.New("endpoint must be a valid https URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", errors.New("endpoint scheme must be https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", errors.New("endpoint must include a hostname")
	}
	if u.User != nil {
		return "", errors.New("endpoint must not include userinfo credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("endpoint must not include query parameters or fragments")
	}
	u.Scheme = "https"
	if u.Path == "/" {
		u.Path = ""
		u.RawPath = ""
	}
	return u.String(), nil
}

func validateSystemOneOptions(opts map[string]string) (map[string]string, error) {
	if len(opts) == 0 {
		return nil, nil
	}
	if len(opts) > maxSystemOneOptionsEntries {
		return nil, fmt.Errorf("options exceeds maximum of %d entries", maxSystemOneOptionsEntries)
	}
	clean := make(map[string]string, len(opts))
	for k, v := range opts {
		key := strings.TrimSpace(k)
		if key == "" || key != k || len(key) > maxSystemOneOptionKeyBytes {
			return nil, fmt.Errorf("invalid options key %q", k)
		}
		for i := 0; i < len(key); i++ {
			b := key[i]
			if b <= 0x20 || b >= 0x7f {
				return nil, fmt.Errorf("options key %q must contain printable ASCII characters only", key)
			}
		}
		lowerKey := strings.ToLower(key)
		for _, forbidden := range forbiddenSystemOneOptionSubstrings {
			if strings.Contains(lowerKey, forbidden) {
				return nil, fmt.Errorf("options key %q is not permitted: credentials must not be stored in options", key)
			}
		}
		if !utf8.ValidString(v) || len(v) > maxSystemOneOptionValueBytes {
			return nil, fmt.Errorf("options value for %q must be valid UTF-8 of at most %d bytes", key, maxSystemOneOptionValueBytes)
		}
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return nil, fmt.Errorf("options value for %q must not contain control characters", key)
			}
		}
		clean[key] = v
	}
	return clean, nil
}

func parseSystemOneLimits(dto *SystemOneInstanceLimitsDTO) (store.SystemOneInstanceLimits, error) {
	if dto == nil {
		return store.SystemOneInstanceLimits{}, nil
	}
	if dto.MaxInFlight < 0 {
		return store.SystemOneInstanceLimits{}, errors.New("limits.maxInFlight must be >= 0")
	}
	if math.IsNaN(dto.RatePerSec) || math.IsInf(dto.RatePerSec, 0) || dto.RatePerSec < 0 {
		return store.SystemOneInstanceLimits{}, errors.New("limits.ratePerSec must be a finite number >= 0")
	}
	if dto.MaxInputTokens < 0 {
		return store.SystemOneInstanceLimits{}, errors.New("limits.maxInputTokens must be >= 0")
	}
	var callTimeout time.Duration
	if raw := strings.TrimSpace(dto.CallTimeout); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return store.SystemOneInstanceLimits{}, fmt.Errorf("invalid limits.callTimeout %q: must be a Go duration such as \"5s\"", raw)
		}
		if d <= 0 || d > systemoneruntime.MaxAllowedEvalTime {
			return store.SystemOneInstanceLimits{}, fmt.Errorf("limits.callTimeout must be > 0 and <= %s", systemoneruntime.MaxAllowedEvalTime)
		}
		callTimeout = d
	}
	return store.SystemOneInstanceLimits{
		MaxInFlight:    dto.MaxInFlight,
		RatePerSec:     dto.RatePerSec,
		MaxInputTokens: dto.MaxInputTokens,
		CallTimeout:    callTimeout,
	}, nil
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func (s *Server) createSystemOneInstance(w http.ResponseWriter, r *http.Request, user UserIdentity) {
	ctx := r.Context()

	var req CreateSystemOneInstanceRequest
	if !readBoundedJSONStrict(w, r, maxCreateSystemOneInstanceBodyBytes, &req) {
		return
	}

	name := strings.TrimSpace(req.Name)
	if !store.IsValidSystemOneInstanceName(name) {
		ValidationError(w, "invalid instance name: must be a lowercase alphanumeric slug (1..63 characters)", nil)
		return
	}

	adapterName := strings.TrimSpace(req.Adapter)
	if adapterName == "" {
		ValidationError(w, "adapter is required", nil)
		return
	}
	adapter, ok := s.SystemOneProviderRegistry().Lookup(adapterName)
	if !ok {
		ValidationError(w, fmt.Sprintf("unknown System One adapter %q", adapterName), nil)
		return
	}

	authKind := store.SystemOneAuthKindAPIKey
	if req.Auth != nil && strings.TrimSpace(req.Auth.Kind) != "" {
		authKind = strings.TrimSpace(req.Auth.Kind)
	}
	if authKind != store.SystemOneAuthKindAPIKey {
		ValidationError(w, fmt.Sprintf("auth kind %q: auth kind not yet supported", authKind), nil)
		return
	}

	if req.AllowPrivateNetwork {
		ValidationError(w, "allowPrivateNetwork is not supported in P1", nil)
		return
	}

	endpoint, err := validateSystemOneEndpoint(req.Endpoint, defaultSystemOneAdapterEndpoint(adapter), adapterName)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	model := strings.TrimSpace(req.Model)
	if len(model) > maxSystemOneModelBytes || !utf8.ValidString(model) || hasControlChars(model) {
		ValidationError(w, "invalid model identifier", nil)
		return
	}

	location := strings.TrimSpace(req.Location)
	if len(location) > maxSystemOneLocationBytes || !utf8.ValidString(location) || hasControlChars(location) {
		ValidationError(w, "invalid location", nil)
		return
	}

	if req.Auth != nil {
		if len(req.Auth.Scopes) > 0 || strings.TrimSpace(req.Auth.Audience) != "" {
			ValidationError(w, "auth scopes and audience are not supported for api_key auth", nil)
			return
		}
		if hdr := strings.TrimSpace(req.Auth.HeaderName); hdr != "" && !strings.EqualFold(hdr, "Authorization") {
			ValidationError(w, "custom auth headerName is not supported in P1", nil)
			return
		}
	}

	opts, err := validateSystemOneOptions(req.Options)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	limits, err := parseSystemOneLimits(req.Limits)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	instView := systemoneprovider.InstanceView{
		Name:     name,
		Model:    model,
		Endpoint: endpoint,
		Location: location,
		Options:  maps.Clone(opts),
	}
	caps, err := adapter.Capabilities(instView)
	if err != nil {
		ValidationError(w, safeSystemOneAdapterConfigMessage(err), nil)
		return
	}
	if !caps.SideEffectFree {
		ValidationError(w, "adapter must be side-effect-free", nil)
		return
	}
	if validator, ok := adapter.(systemoneprovider.InstanceValidator); ok {
		if err := validator.ValidateInstance(instView); err != nil {
			ValidationError(w, safeSystemOneAdapterConfigMessage(err), nil)
			return
		}
	}

	if req.APIKey != "" && req.Auth != nil && req.Auth.APIKey != "" && req.APIKey != req.Auth.APIKey {
		ValidationError(w, "conflicting apiKey values in request", nil)
		return
	}
	apiKey := req.APIKey
	if apiKey == "" && req.Auth != nil {
		apiKey = req.Auth.APIKey
	}
	if len(apiKey) > maxSystemOneAPIKeyBytes || !isValidSystemOneAPIKey(apiKey) {
		ValidationError(w, "apiKey is required and must be a non-empty credential free of control characters", nil)
		return
	}

	if s.secretBackend == nil {
		writeError(w, http.StatusNotImplemented, ErrCodeNotImplemented, "Secret backend is not configured", nil)
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	id := api.NewUUID()
	secretName := systemOneInstanceSecretName(id)
	actorID := user.ID()

	inst := &store.SystemOneInstance{
		ID:       id,
		Name:     name,
		Adapter:  adapterName,
		Model:    model,
		Endpoint: endpoint,
		Location: location,
		Auth: store.SystemOneInstanceAuth{
			Kind:       store.SystemOneAuthKindAPIKey,
			SecretName: secretName,
		},
		Options:             opts,
		Limits:              limits,
		AllowPrivateNetwork: false,
		Enabled:             enabled,
		CreatedBy:           actorID,
		UpdatedBy:           actorID,
	}

	// Pre-check name uniqueness before writing to the secret backend so
	// duplicate creates fail cleanly without churning secret storage.
	if existing, lookupErr := s.store.GetSystemOneInstanceByName(ctx, name); lookupErr == nil && existing != nil {
		Conflict(w, fmt.Sprintf("System One instance %q already exists", name))
		return
	} else if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
		InternalError(w)
		return
	}

	if _, err = s.setSystemOneInstanceSecret(ctx, id, apiKey, actorID); err != nil {
		s.logSystemOneAudit().Error("Failed to store System One instance credential",
			"instance_id", id,
			"instance_name", name,
			"secret_name", secretName,
			"actor_id", actorID,
		)
		InternalError(w)
		return
	}

	if err := s.store.CreateSystemOneInstance(ctx, inst); err != nil {
		// Roll back the hub-scoped internal secret if row creation fails.
		if rbErr := s.deleteSystemOneInstanceSecret(ctx, secretName); rbErr != nil {
			s.logSystemOneAudit().Error("Failed to roll back System One instance credential after store failure",
				"instance_id", id,
				"instance_name", name,
				"secret_name", secretName,
				"actor_id", actorID,
			)
		}
		if errors.Is(err, store.ErrAlreadyExists) {
			Conflict(w, fmt.Sprintf("System One instance %q already exists", name))
			return
		}
		if errors.Is(err, store.ErrInvalidInput) {
			ValidationError(w, "invalid System One instance configuration", nil)
			return
		}
		InternalError(w)
		return
	}

	s.logSystemOneAudit().Info("System One instance created",
		"instance_id", inst.ID,
		"instance_name", inst.Name,
		"adapter", inst.Adapter,
		"model", inst.Model,
		"endpoint", inst.Endpoint,
		"secret_name", inst.Auth.SecretName,
		"revision", inst.Revision,
		"actor_id", actorID,
	)

	writeJSON(w, http.StatusCreated, toSystemOneInstanceResponse(inst))
}

func (s *Server) listSystemOneInstances(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var filter store.SystemOneInstanceFilter
	if adapter := strings.TrimSpace(q.Get("adapter")); adapter != "" {
		filter.Adapter = adapter
	}
	if enabledRaw := strings.TrimSpace(q.Get("enabled")); enabledRaw != "" {
		v, err := strconv.ParseBool(enabledRaw)
		if err != nil {
			ValidationError(w, "query parameter \"enabled\" must be a boolean", nil)
			return
		}
		filter.Enabled = &v
	}

	var listOpts store.ListOptions
	if cursor := strings.TrimSpace(q.Get("cursor")); cursor != "" {
		listOpts.Cursor = cursor
	}
	if limitRaw := strings.TrimSpace(q.Get("limit")); limitRaw != "" {
		n, err := strconv.Atoi(limitRaw)
		if err != nil || n <= 0 || n > 500 {
			ValidationError(w, "query parameter \"limit\" must be an integer between 1 and 500", nil)
			return
		}
		listOpts.Limit = n
	}

	res, err := s.store.ListSystemOneInstances(r.Context(), filter, listOpts)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			ValidationError(w, "invalid pagination parameters", nil)
			return
		}
		InternalError(w)
		return
	}

	items := make([]SystemOneInstanceResponse, 0, len(res.Items))
	for i := range res.Items {
		items = append(items, toSystemOneInstanceResponse(&res.Items[i]))
	}
	writeJSON(w, http.StatusOK, ListSystemOneInstancesResponse{
		Instances:  items,
		NextCursor: res.NextCursor,
		TotalCount: res.TotalCount,
	})
}

func (s *Server) getSystemOneInstance(w http.ResponseWriter, r *http.Request, name string) {
	if !store.IsValidSystemOneInstanceName(name) {
		NotFound(w, "system_one_instance")
		return
	}
	inst, err := s.store.GetSystemOneInstanceByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "system_one_instance")
			return
		}
		InternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, toSystemOneInstanceResponse(inst))
}

func (s *Server) deleteSystemOneInstance(w http.ResponseWriter, r *http.Request, name string, user UserIdentity) {
	ctx := r.Context()
	if !store.IsValidSystemOneInstanceName(name) {
		NotFound(w, "system_one_instance")
		return
	}
	inst, err := s.store.GetSystemOneInstanceByName(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "system_one_instance")
			return
		}
		InternalError(w)
		return
	}

	// Remove the hub-scoped internal credential first (treating ErrNotFound as
	// success). If secret deletion fails, keep the DB row and answer 500 so the
	// caller can retry without permanently orphaning the secret.
	if delErr := s.deleteSystemOneInstanceSecret(ctx, inst.Auth.SecretName); delErr != nil {
		s.logSystemOneAudit().Error("Failed to delete System One instance credential",
			"instance_id", inst.ID,
			"instance_name", inst.Name,
			"secret_name", inst.Auth.SecretName,
			"actor_id", user.ID(),
		)
		InternalError(w)
		return
	}

	if err := s.store.DeleteSystemOneInstance(ctx, inst.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.SystemOneRuntime().ForgetInstance(inst.ID)
			NotFound(w, "system_one_instance")
			return
		}
		InternalError(w)
		return
	}

	s.SystemOneRuntime().ForgetInstance(inst.ID)

	s.logSystemOneAudit().Info("System One instance deleted",
		"instance_id", inst.ID,
		"instance_name", inst.Name,
		"secret_name", inst.Auth.SecretName,
		"actor_id", user.ID(),
	)

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) probeSystemOneInstance(w http.ResponseWriter, r *http.Request, name string, user UserIdentity) {
	ctx := r.Context()
	if !store.IsValidSystemOneInstanceName(name) {
		NotFound(w, "system_one_instance")
		return
	}

	inst, err := s.store.GetSystemOneInstanceByName(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "system_one_instance")
			return
		}
		InternalError(w)
		return
	}

	var req ProbeSystemOneInstanceRequest
	if !readBoundedJSONStrict(w, r, maxProbeSystemOneInstanceBodyBytes, &req) {
		return
	}

	rawSpec := bytes.TrimSpace(req.Spec)
	hasSpec := len(rawSpec) > 0 && !bytes.Equal(rawSpec, []byte("null"))

	if req.Models {
		if hasSpec || req.Probe != nil || req.ProbeText != nil || req.Kind != "" || len(req.Params) > 0 {
			ValidationError(w, "models probe must not combine \"models\": true with spec or probe inputs", nil)
			return
		}
		runtimeInst := storeSystemOneInstanceToRuntime(inst)
		models, listErr := s.SystemOneRuntime().ListModels(ctx, runtimeInst)
		if listErr != nil {
			reason := string(systemoneprovider.FailInternal)
			if pe, ok := systemoneprovider.AsProviderError(listErr); ok && pe.Reason.Valid() {
				if pe.Reason == systemoneprovider.FailUnsupported {
					ValidationError(w, "adapter does not support model listing", map[string]any{
						"reason": string(pe.Reason),
					})
					return
				}
				reason = string(pe.Reason)
			}
			s.logSystemOneAudit().Info("System One models probe completed",
				"instance_name", inst.Name,
				"adapter", inst.Adapter,
				"class", string(systemoneruntime.ClassFailed),
				"reason", reason,
				"actor_id", user.ID(),
			)
			writeJSON(w, http.StatusOK, ProbeSystemOneInstanceResponse{
				Class:  systemoneruntime.ClassFailed,
				Reason: reason,
			})
			return
		}

		s.logSystemOneAudit().Info("System One models probe completed",
			"instance_name", inst.Name,
			"adapter", inst.Adapter,
			"class", string(systemoneruntime.ClassSuccess),
			"model_count", len(models),
			"actor_id", user.ID(),
		)
		writeJSON(w, http.StatusOK, ProbeSystemOneInstanceResponse{
			Class:  systemoneruntime.ClassSuccess,
			Models: models,
		})
		return
	}

	if !hasSpec {
		ValidationError(w, "probe request must specify either \"models\": true or a non-empty \"spec\"", nil)
		return
	}
	if req.Probe != nil && req.ProbeText != nil {
		ValidationError(w, "probe request must not set both \"probe\" and \"probeText\"", nil)
		return
	}

	var probeText string
	var hasProbeText bool
	if req.Probe != nil {
		probeText = req.Probe.Text
		hasProbeText = true
	} else if req.ProbeText != nil {
		probeText = *req.ProbeText
		hasProbeText = true
	}
	if !hasProbeText {
		ValidationError(w, "probe.text is required when evaluating an inline spec", nil)
		return
	}

	runtimeInst := storeSystemOneInstanceToRuntime(inst)
	var capsLimits *spec.CapabilityLimits
	if adapter, ok := s.SystemOneProviderRegistry().Lookup(inst.Adapter); ok {
		if caps, capsErr := adapter.Capabilities(runtimeInst.View()); capsErr == nil {
			capsLimits = caps.SpecLimits()
		}
	}

	kind := spec.Kind(strings.TrimSpace(req.Kind))
	if kind == "" {
		kind = inferProbeSpecKind(rawSpec)
	}

	compiled, compileErr := spec.Compile(rawSpec, spec.CompileOptions{
		Kind:   kind,
		Params: req.Params,
		Caps:   capsLimits,
	})
	if compileErr != nil {
		var ce *spec.CompileError
		if errors.As(compileErr, &ce) {
			ValidationError(w, ce.Error(), map[string]any{
				"path":     ce.Path,
				"category": string(ce.Category),
			})
			return
		}
		ValidationError(w, "failed to compile System One probe spec", nil)
		return
	}

	if compiled.Spec.Binding.Instance != "" && compiled.Spec.Binding.Instance != inst.Name {
		ValidationError(w, fmt.Sprintf("spec binding.instance %q does not match target instance %q", compiled.Spec.Binding.Instance, inst.Name), nil)
		return
	}
	if compiled.Spec.Binding.Pool != "" {
		ValidationError(w, "spec binding.pool is not supported in P1", nil)
		return
	}

	ruleID := strings.TrimSpace(req.RuleID)
	if len(ruleID) > maxSystemOneRuleIDBytes || hasControlChars(ruleID) {
		ValidationError(w, "invalid ruleId", nil)
		return
	}

	input := systemoneruntime.ProbeInput(probeText)
	if len(req.Params) > 0 {
		input.Params = make(map[string]string, len(req.Params))
		for k, v := range req.Params {
			input.Params[k] = v
		}
	}

	decision, evalErr := s.SystemOneRuntime().Evaluate(ctx, compiled, input, systemoneruntime.EvalOptions{
		Instance: &runtimeInst,
		Consumer: "admin_probe",
		RuleID:   ruleID,
	})

	eval := decision.Eval
	if eval == nil && evalErr != nil {
		eval, _ = systemoneruntime.EvaluationFrom(evalErr)
	}

	resp := ProbeSystemOneInstanceResponse{
		Class:      systemoneruntime.ClassFailed,
		Evaluation: eval,
	}
	if eval != nil {
		resp.Class = eval.Class
		resp.Reason = eval.Reason
	}
	if evalErr == nil && resp.Class == systemoneruntime.ClassSuccess {
		decDTO := &ProbeSystemOneDecision{
			Kind: decision.Kind,
		}
		switch decision.Kind {
		case spec.KindFilter, spec.KindJudgmentBool:
			b := decision.Bool
			decDTO.Bool = &b
		case spec.KindRoute, spec.KindJudgmentChoice:
			str := decision.String
			decDTO.String = &str
		}
		resp.Decision = decDTO
	}

	s.logSystemOneAudit().Info("System One spec probe completed",
		"instance_name", inst.Name,
		"instance_revision", inst.Revision,
		"adapter", inst.Adapter,
		"eval_id", func() string {
			if eval != nil {
				return eval.EvalID
			}
			return ""
		}(),
		"class", string(resp.Class),
		"reason", resp.Reason,
		"actor_id", user.ID(),
	)

	writeJSON(w, http.StatusOK, resp)
}

func inferProbeSpecKind(raw []byte) spec.Kind {
	type questionPeek struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	type specPeek struct {
		Questions []questionPeek `json:"questions"`
		Output    *struct {
			Question string   `json:"question"`
			Match    []string `json:"match"`
		} `json:"output"`
	}
	var env struct {
		Spec *specPeek `json:"spec"`
	}
	var sp specPeek
	if err := json.Unmarshal(raw, &env); err == nil && env.Spec != nil {
		sp = *env.Spec
	} else if err := json.Unmarshal(raw, &sp); err != nil {
		return spec.KindJudgments
	}
	if sp.Output == nil {
		return spec.KindJudgments
	}
	if len(sp.Output.Match) > 0 {
		return spec.KindFilter
	}
	for _, q := range sp.Questions {
		if q.ID == sp.Output.Question {
			switch q.Type {
			case spec.PrimitiveBinary:
				return spec.KindJudgmentBool
			case spec.PrimitiveChoice:
				return spec.KindJudgmentChoice
			}
		}
	}
	return spec.KindJudgments
}

func (s *Server) logSystemOneAudit() *slog.Logger {
	if s.envSecretLog != nil {
		return s.envSecretLog
	}
	return slog.Default()
}
