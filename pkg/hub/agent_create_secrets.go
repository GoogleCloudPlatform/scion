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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// maxCreateAgentSecrets caps the number of agent-scoped secrets that may be
	// supplied in a single CreateAgentRequest (design §4.5).
	maxCreateAgentSecrets = 50

	// maxCreateAgentSecretValueBytes caps each decoded create-time secret value
	// at 64 KiB, matching setSecret's limit.
	maxCreateAgentSecretValueBytes = 64 * 1024

	// createAgentSecretsStoreFailedMessage is the 503 response message returned
	// when create-time secret writes fail and the agent creation is rolled back
	// (design §4.5 step 4).
	createAgentSecretsStoreFailedMessage = "agent secrets could not be stored; the agent was not created"

	// existingAgentSecretsStoreFailedMessage is the 503 response message
	// returned when upserting secrets on an existing agent before start fails.
	existingAgentSecretsStoreFailedMessage = "agent secrets could not be stored; the agent was not started"

	// agentSecretsExperimentDisabledMessage is returned when create-time secrets
	// are supplied while the hub.agent_secrets experiment is disabled.
	agentSecretsExperimentDisabledMessage = "agent secrets require the hub.agent_secrets experiment (experiment not enabled)"
)

// AgentSecretInput describes an agent-scoped secret supplied at agent creation
// (or existing-agent start) time. Values are stored directly in the secret
// backend and are never persisted on the agent record or logged.
type AgentSecretInput struct {
	Key           string `json:"key"`
	Value         string `json:"value"` // base64-encoded when Encoding == "base64"; raw UTF-8 otherwise
	Encoding      string `json:"encoding,omitempty"`
	Type          string `json:"type,omitempty"` // environment (default) | file | on_demand
	Target        string `json:"target,omitempty"`
	InjectionMode string `json:"injectionMode,omitempty"`
	Description   string `json:"description,omitempty"`
}

// LogValue implements slog.LogValuer so secret values can never leak into
// structured logs if an AgentSecretInput is logged directly.
func (s AgentSecretInput) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("key", s.Key),
		slog.String("type", s.Type),
		slog.String("target", s.Target),
		slog.String("injection_mode", s.InjectionMode),
		slog.String("encoding", s.Encoding),
		slog.String("value", "[REDACTED]"),
	)
}

// LogValue implements slog.LogValuer on CreateAgentRequest so logging a create
// request can never leak secret values or inline env values.
func (r CreateAgentRequest) LogValue() slog.Value {
	keys := make([]string, 0, len(r.Secrets))
	for _, s := range r.Secrets {
		keys = append(keys, s.Key)
	}
	return slog.GroupValue(
		slog.String("name", r.Name),
		slog.String("project_id", r.ProjectID),
		slog.String("template", r.Template),
		slog.String("runtime_broker_id", r.RuntimeBrokerID),
		slog.Bool("managed_agent", r.ManagedAgent),
		slog.Bool("provision_only", r.ProvisionOnly),
		slog.Int("secret_count", len(r.Secrets)),
		slog.Any("secret_keys", keys),
	)
}

// validatedAgentSecret holds a validated and decoded create-time secret ready
// to be written to the secret backend.
type validatedAgentSecret struct {
	Key           string
	Value         string
	SecretType    string
	Target        string
	InjectionMode string
	Description   string
}

type secretValidationError struct {
	status  int
	code    string
	message string
	details map[string]interface{}
}

func (e *secretValidationError) Error() string {
	return e.message
}

// validateCreateAgentSecrets checks the hub.agent_secrets experiment gate and
// validates all create-time secret inputs before any database transaction runs.
func (s *Server) validateCreateAgentSecrets(inputs []AgentSecretInput) ([]validatedAgentSecret, *secretValidationError) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if !s.experimentEnabled(experiments.AgentSecrets) {
		return nil, &secretValidationError{
			status:  http.StatusBadRequest,
			code:    ErrCodeInvalidRequest,
			message: agentSecretsExperimentDisabledMessage,
		}
	}
	if len(inputs) > maxCreateAgentSecrets {
		return nil, &secretValidationError{
			status:  http.StatusBadRequest,
			code:    ErrCodeValidationError,
			message: fmt.Sprintf("at most %d secrets may be provided at agent creation", maxCreateAgentSecrets),
			details: map[string]interface{}{
				"field": "secrets",
				"count": len(inputs),
				"limit": maxCreateAgentSecrets,
			},
		}
	}

	seen := make(map[string]struct{}, len(inputs))
	out := make([]validatedAgentSecret, 0, len(inputs))
	for i, in := range inputs {
		if in.Key == "" {
			return nil, &secretValidationError{
				status:  http.StatusBadRequest,
				code:    ErrCodeValidationError,
				message: "secret key is required",
				details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].key", i)},
			}
		}
		if strings.ContainsAny(in.Key, "= \t\n\r") {
			return nil, &secretValidationError{
				status:  http.StatusBadRequest,
				code:    ErrCodeValidationError,
				message: "secret key cannot contain whitespace or '='",
				details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].key", i), "key": in.Key},
			}
		}
		if _, dup := seen[in.Key]; dup {
			return nil, &secretValidationError{
				status:  http.StatusBadRequest,
				code:    ErrCodeValidationError,
				message: fmt.Sprintf("duplicate secret key: %s", in.Key),
				details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].key", i), "key": in.Key},
			}
		}
		seen[in.Key] = struct{}{}

		if isSystemOneReservedSecretKey(in.Key) {
			return nil, &secretValidationError{
				status:  http.StatusBadRequest,
				code:    ErrCodeValidationError,
				message: "secret key prefix \"systemone.instance.\" is reserved for System One provider credentials",
				details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].key", i), "key": in.Key},
			}
		}
		if isWebhookEndpointReservedSecretKey(in.Key) {
			return nil, &secretValidationError{
				status:  http.StatusBadRequest,
				code:    ErrCodeValidationError,
				message: fmt.Sprintf("secret key prefix %q is reserved for webhook endpoint signing secrets", webhookEndpointReservedSecretPrefix),
				details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].key", i), "key": in.Key},
			}
		}
		if in.Value == "" {
			return nil, &secretValidationError{
				status:  http.StatusBadRequest,
				code:    ErrCodeValidationError,
				message: fmt.Sprintf("secret value is required for key %s", in.Key),
				details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].value", i), "key": in.Key},
			}
		}

		var decoded []byte
		switch in.Encoding {
		case "", "raw":
			decoded = []byte(in.Value)
		case "base64":
			var err error
			decoded, err = base64.StdEncoding.DecodeString(in.Value)
			if err != nil {
				return nil, &secretValidationError{
					status:  http.StatusBadRequest,
					code:    ErrCodeValidationError,
					message: fmt.Sprintf("secret value for key %s must be valid base64", in.Key),
					details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].value", i), "key": in.Key},
				}
			}
		default:
			return nil, &secretValidationError{
				status:  http.StatusBadRequest,
				code:    ErrCodeValidationError,
				message: "encoding must be \"raw\" or \"base64\"",
				details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].encoding", i), "key": in.Key},
			}
		}

		if len(decoded) > maxCreateAgentSecretValueBytes {
			return nil, &secretValidationError{
				status:  http.StatusBadRequest,
				code:    ErrCodeValidationError,
				message: fmt.Sprintf("secret value for key %s exceeds 64KB limit", in.Key),
				details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].value", i), "key": in.Key},
			}
		}

		secretType := in.Type
		if secretType == "" {
			secretType = store.SecretTypeEnvironment
		}
		if msg, ok := validateSecretTypeInput(secretType); !ok {
			return nil, &secretValidationError{
				status:  http.StatusBadRequest,
				code:    ErrCodeValidationError,
				message: msg,
				details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].type", i), "key": in.Key, "value": secretType},
			}
		}

		target := in.Target
		if target == "" {
			target = in.Key
		}
		injectionMode := in.InjectionMode
		if injectionMode != "" && injectionMode != store.InjectionModeAsNeeded && injectionMode != store.InjectionModeAlways {
			return nil, &secretValidationError{
				status:  http.StatusBadRequest,
				code:    ErrCodeValidationError,
				message: "injectionMode must be \"always\" or \"as_needed\"",
				details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].injectionMode", i), "key": in.Key, "value": injectionMode},
			}
		}
		if secretType == store.SecretTypeOnDemand {
			var fErr *onDemandFieldError
			target, injectionMode, fErr = validateOnDemandFields(store.ScopeAgent, in.Key, in.Target, injectionMode, false)
			if fErr != nil {
				details := map[string]interface{}{
					"field": fmt.Sprintf("secrets[%d].%s", i, fErr.field),
					"key":   in.Key,
				}
				if v, ok := fErr.details["value"]; ok {
					details["value"] = v
				}
				return nil, &secretValidationError{
					status:  http.StatusBadRequest,
					code:    ErrCodeValidationError,
					message: fErr.message,
					details: details,
				}
			}
		}
		if secretType == store.SecretTypeEnvironment && secret.IsReservedEnvTarget(target) {
			return nil, &secretValidationError{
				status:  http.StatusBadRequest,
				code:    ErrCodeValidationError,
				message: "target is reserved for scion's own control-plane environment variables",
				details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].target", i), "key": in.Key, "value": target},
			}
		}
		if secretType == store.SecretTypeFile {
			if strings.Contains(target, "..") {
				return nil, &secretValidationError{
					status:  http.StatusBadRequest,
					code:    ErrCodeValidationError,
					message: "target path must not contain '..'",
					details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].target", i), "key": in.Key, "value": target},
				}
			}
			if !strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "~/") {
				return nil, &secretValidationError{
					status:  http.StatusBadRequest,
					code:    ErrCodeValidationError,
					message: "file secret target must be an absolute path (or start with ~/)",
					details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].target", i), "key": in.Key, "value": target},
				}
			}
			if !s.config.Workstation && secret.IsReservedFileTarget(store.ScopeAgent, target) {
				return nil, &secretValidationError{
					status:  http.StatusBadRequest,
					code:    ErrCodeReservedTarget,
					message: "target path is reserved for hub-scoped telemetry credentials in hosted mode",
					details: map[string]interface{}{"field": fmt.Sprintf("secrets[%d].target", i), "key": in.Key, "value": target},
				}
			}
		}

		out = append(out, validatedAgentSecret{
			Key:           in.Key,
			Value:         string(decoded),
			SecretType:    secretType,
			Target:        target,
			InjectionMode: injectionMode,
			Description:   in.Description,
		})
	}
	return out, nil
}

// createAuditAfterSummaryWithSecrets builds the JSON AfterSummary for the
// agent-create mutation audit record when create-time secrets are supplied.
// Only key names are recorded — never values.
func createAuditAfterSummaryWithSecrets(secrets []validatedAgentSecret) string {
	if len(secrets) == 0 {
		return ""
	}
	keys := make([]string, len(secrets))
	for i, sec := range secrets {
		keys[i] = sec.Key
	}
	b, err := json.Marshal(struct {
		SecretKeys []string `json:"secret_keys"`
	}{SecretKeys: keys})
	if err != nil {
		return ""
	}
	return string(b)
}

// secretCreatorAttribution returns the CreatedBy/UpdatedBy attribution string
// for create-time secret writes: "agent:<id>" for agent callers, user ID for
// user callers, or fallbackCreatedBy otherwise.
func secretCreatorAttribution(ctx context.Context, fallbackCreatedBy string) string {
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		return "agent:" + agentIdent.ID()
	}
	if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
		return userIdent.ID()
	}
	return fallbackCreatedBy
}

// headlessSecretCreatorAttribution returns the CreatedBy/UpdatedBy string for
// headless create-time secret writes.
func headlessSecretCreatorAttribution(auth FireAuthority) string {
	if auth.PrincipalKind == store.DelegationPrincipalAgent {
		return "agent:" + auth.PrincipalID
	}
	return auth.PrincipalID
}

// storeCreateAgentSecrets writes create-time secrets for a newly committed
// agent. If rollbackOnFailure is true (new agent create) and any write fails,
// all secrets already written in this call are deleted best-effort before the
// error is returned so the caller can fail closed and hard-delete the agent.
func (s *Server) storeCreateAgentSecrets(ctx context.Context, agent *store.Agent, secrets []validatedAgentSecret, createdBy string, rollbackOnFailure bool) error {
	if len(secrets) == 0 || agent == nil {
		return nil
	}
	backend := s.GetSecretBackend()
	if backend == nil {
		return secret.ErrNoSecretBackend
	}
	written := make([]string, 0, len(secrets))
	for _, sec := range secrets {
		_, _, err := backend.Set(ctx, &secret.SetSecretInput{
			Name:          sec.Key,
			Value:         sec.Value,
			SecretType:    sec.SecretType,
			Target:        sec.Target,
			Scope:         secret.ScopeAgent,
			ScopeID:       agent.ID,
			ProjectID:     agent.ProjectID,
			Description:   sec.Description,
			InjectionMode: sec.InjectionMode,
			CreatedBy:     createdBy,
			UpdatedBy:     createdBy,
		})
		if err != nil {
			if rollbackOnFailure {
				// Include sec.Key in rollback: an external backend such as
				// GCPBackend.Set may write the secret version to Secret Manager
				// before failing on the DB metadata upsert or a deadline.
				s.removeAgentScopedSecrets(ctx, agent.ID, append(written, sec.Key))
			}
			return err
		}
		written = append(written, sec.Key)
	}
	// Close the write vs delete race: if the agent was deleted or soft-deleted
	// while backend.Set was in flight, compensate by removing the written keys.
	if s.compensateIfAgentDeleted(ctx, agent.ID, written) {
		return errAgentDeletedDuringSecretWrite
	}
	return nil
}

// upsertExistingAgentSecrets authorizes agent.update on existingAgent and
// upserts createSecrets at agent scope before starting/resuming the agent
// (`scion start <existing-agent> --secrets-file`, design §4.5).
// Returns true if no secrets were provided or all upserts succeeded.
func (s *Server) upsertExistingAgentSecrets(ctx context.Context, w http.ResponseWriter, existingAgent *store.Agent, createSecrets []validatedAgentSecret, createdBy string) bool {
	if len(createSecrets) == 0 {
		return true
	}
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}
	decision := s.authzService.CheckAccess(ctx, identity, Resource{
		Type:       "agent",
		ID:         existingAgent.ID,
		OwnerID:    existingAgent.OwnerID,
		ParentType: "project",
		ParentID:   existingAgent.ProjectID,
	}, ActionUpdate)
	if !decision.Allowed {
		Forbidden(w)
		return false
	}
	if !existingAgent.DeletedAt.IsZero() {
		NotFound(w, "Agent")
		return false
	}
	if err := s.storeCreateAgentSecrets(ctx, existingAgent, createSecrets, secretCreatorAttribution(ctx, createdBy), false); err != nil {
		if errors.Is(err, errAgentDeletedDuringSecretWrite) {
			NotFound(w, "Agent")
			return false
		}
		slog.ErrorContext(ctx, "agent start: failed to store agent secrets on existing agent",
			"agent_id", existingAgent.ID, "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, existingAgentSecretsStoreFailedMessage, nil)
		return false
	}
	return true
}
