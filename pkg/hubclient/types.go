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
	"encoding/json"
	"time"
)

// Agent represents an agent from the Hub API.
type Agent struct {
	ID                string            `json:"id"`          // Hub UUID (database primary key)
	Slug              string            `json:"slug"`        // URL-safe slug identifier (unique per project)
	ContainerID       string            `json:"containerId"` // Runtime container ID (ephemeral)
	Name              string            `json:"name"`
	Template          string            `json:"template,omitempty"`
	HarnessConfig     string            `json:"harnessConfig,omitempty"`
	HarnessAuth       string            `json:"harnessAuth,omitempty"`
	ProjectID         string            `json:"projectId,omitempty"`
	Project           string            `json:"project,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	Phase             string            `json:"phase,omitempty"`    // Lifecycle phase (created, provisioning, running, stopped, error)
	Activity          string            `json:"activity,omitempty"` // Runtime activity (working, thinking, executing, waiting_for_input, completed)
	Status            string            `json:"status"`             // Legacy/fallback status field
	ConnectionState   string            `json:"connectionState,omitempty"`
	ContainerStatus   string            `json:"containerStatus,omitempty"`
	RuntimeState      string            `json:"runtimeState,omitempty"`
	Image             string            `json:"image,omitempty"`
	Detached          bool              `json:"detached,omitempty"`
	Runtime           string            `json:"runtime,omitempty"`
	RuntimeBrokerID   string            `json:"runtimeBrokerId,omitempty"`
	RuntimeBrokerName string            `json:"runtimeBrokerName,omitempty"`
	RuntimeBrokerType string            `json:"runtimeBrokerType,omitempty"`
	WebPTYEnabled     bool              `json:"webPtyEnabled,omitempty"`
	TaskSummary       string            `json:"taskSummary,omitempty"`
	AppliedConfig     *AgentConfig      `json:"appliedConfig,omitempty"`
	DirectConnect     *DirectConnect    `json:"directConnect,omitempty"`
	Kubernetes        *KubernetesInfo   `json:"kubernetes,omitempty"`
	Created           time.Time         `json:"created"`
	Updated           time.Time         `json:"updated"`
	LastSeen          time.Time         `json:"lastSeen,omitempty"`
	LastActivityEvent time.Time         `json:"lastActivityEvent,omitempty"`
	DeletedAt         time.Time         `json:"deletedAt,omitempty"`
	CreatedBy         string            `json:"createdBy,omitempty"`
	OwnerID           string            `json:"ownerId,omitempty"`
	Visibility        string            `json:"visibility,omitempty"`
	StateVersion      int64             `json:"stateVersion,omitempty"`
}

// UnmarshalJSON implements custom unmarshaling to support legacy grove fields.
func (a *Agent) UnmarshalJSON(data []byte) error {
	type Alias Agent
	aux := &struct {
		Grove   string `json:"grove"`
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(a),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if a.Project == "" && aux.Grove != "" {
		a.Project = aux.Grove
	}
	if a.ProjectID == "" && aux.GroveID != "" {
		a.ProjectID = aux.GroveID
	}
	return nil
}

// MarshalJSON implements custom marshaling to support legacy grove fields.
func (a Agent) MarshalJSON() ([]byte, error) {
	type Alias Agent
	return json.Marshal(&struct {
		Alias
		Grove   string `json:"grove,omitempty"`
		GroveID string `json:"groveId,omitempty"`
	}{
		Alias:   Alias(a),
		Grove:   a.Project,
		GroveID: a.ProjectID,
	})
}

// AgentConfig represents agent configuration.
type AgentConfig struct {
	Image         string            `json:"image,omitempty"`
	HarnessConfig string            `json:"harnessConfig,omitempty"`
	HarnessAuth   string            `json:"harnessAuth,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Model         string            `json:"model,omitempty"`
	Profile       string            `json:"profile,omitempty"`
	Task          string            `json:"task,omitempty"`
}

// DirectConnect contains direct connection info.
type DirectConnect struct {
	Enabled bool   `json:"enabled"`
	SSHHost string `json:"sshHost,omitempty"`
	SSHPort int    `json:"sshPort,omitempty"`
	SSHUser string `json:"sshUser,omitempty"`
}

// KubernetesInfo contains K8s-specific metadata.
type KubernetesInfo struct {
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	PodName   string `json:"podName,omitempty"`
	SyncedAt  string `json:"syncedAt,omitempty"`
}

// Project represents a project from the Hub API.
type Project struct {
	ID                     string            `json:"id"`
	Name                   string            `json:"name"`
	Slug                   string            `json:"slug"`
	GitRemote              string            `json:"gitRemote,omitempty"`
	DefaultRuntimeBrokerID string            `json:"defaultRuntimeBrokerId,omitempty"`
	Created                time.Time         `json:"created"`
	Updated                time.Time         `json:"updated"`
	CreatedBy              string            `json:"createdBy,omitempty"`
	OwnerID                string            `json:"ownerId,omitempty"`
	Visibility             string            `json:"visibility,omitempty"`
	Labels                 map[string]string `json:"labels,omitempty"`
	Annotations            map[string]string `json:"annotations,omitempty"`
	Providers              []ProjectProvider `json:"providers,omitempty"`
	AgentCount             int               `json:"agentCount,omitempty"`
	ActiveBrokerCount      int               `json:"activeBrokerCount,omitempty"`
	ProjectType            string            `json:"projectType,omitempty"`
}

// UnmarshalJSON implements custom unmarshaling to support legacy grove fields.
func (p *Project) UnmarshalJSON(data []byte) error {
	type Alias Project
	aux := &struct {
		GroveID   string `json:"groveId"`
		GroveName string `json:"groveName"`
		GroveType string `json:"groveType"`
		*Alias
	}{
		Alias: (*Alias)(p),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if p.ID == "" && aux.GroveID != "" {
		p.ID = aux.GroveID
	}
	if p.Name == "" && aux.GroveName != "" {
		p.Name = aux.GroveName
	}
	if p.ProjectType == "" && aux.GroveType != "" {
		p.ProjectType = aux.GroveType
	}
	return nil
}

// MarshalJSON implements custom marshaling to support legacy grove fields.
func (p Project) MarshalJSON() ([]byte, error) {
	type Alias Project
	return json.Marshal(&struct {
		Alias
		ProjectID string `json:"projectId,omitempty"`
		GroveID   string `json:"groveId,omitempty"`
		GroveName string `json:"groveName,omitempty"`
		GroveType string `json:"groveType,omitempty"`
	}{
		Alias:     Alias(p),
		ProjectID: p.ID,
		GroveID:   p.ID,
		GroveName: p.Name,
		GroveType: p.ProjectType,
	})
}

// ProjectProvider represents a broker providing runtime services to a project.
type ProjectProvider struct {
	BrokerID   string    `json:"brokerId"`
	BrokerName string    `json:"brokerName"`
	Status     string    `json:"status"`
	LastSeen   time.Time `json:"lastSeen,omitempty"`
	LocalPath  string    `json:"localPath,omitempty"`
	LinkedBy   string    `json:"linkedBy,omitempty"` // User ID who performed the link
	LinkedAt   time.Time `json:"linkedAt,omitempty"` // Timestamp when the link was created
}

// ProjectSettings represents project configuration settings.
type ProjectSettings struct {
	ActiveProfile          string                 `json:"activeProfile,omitempty"`
	DefaultTemplate        string                 `json:"defaultTemplate,omitempty"`
	DefaultHarnessConfig   string                 `json:"defaultHarnessConfig,omitempty"`
	DefaultModel           string                 `json:"defaultModel,omitempty"`
	DefaultThinkingLevel   *int                   `json:"defaultThinkingLevel,omitempty"`
	TelemetryEnabled       *bool                  `json:"telemetryEnabled,omitempty"`
	AutoExposePortsEnabled *bool                  `json:"autoExposePortsEnabled,omitempty"`
	Bucket                 *BucketConfig          `json:"bucket,omitempty"`
	Runtimes               map[string]interface{} `json:"runtimes,omitempty"`
	Harnesses              map[string]interface{} `json:"harnesses,omitempty"`
	Profiles               map[string]interface{} `json:"profiles,omitempty"`

	// Default agent limits
	DefaultMaxTurns      int                  `json:"defaultMaxTurns,omitempty"`
	DefaultMaxModelCalls int                  `json:"defaultMaxModelCalls,omitempty"`
	DefaultMaxDuration   string               `json:"defaultMaxDuration,omitempty"`
	DefaultResources     *ProjectResourceSpec `json:"defaultResources,omitempty"`

	// Default GCP identity for new agents
	DefaultGCPIdentityMode             string `json:"defaultGCPIdentityMode,omitempty"`             // "block", "passthrough", or "assign"
	DefaultGCPIdentityServiceAccountID string `json:"defaultGCPIdentityServiceAccountID,omitempty"` // Required when mode is "assign"

	// Agent authorization
	MaxAgentRole     string `json:"maxAgentRole,omitempty"`
	DefaultAgentRole string `json:"defaultAgentRole,omitempty"`
}

// ResolvedHubDefault reports whether a hub-level default exists for a project
// setting, in the GET /api/v1/projects/{id}/settings/resolved response.
//
// Tri-state, not a bool: "unknown" means presence could not be determined
// (the hub source was unreachable, or it cannot distinguish an
// explicitly-zero value from an unset one) and must NOT be read as "no".
type ResolvedHubDefault string

const (
	// ResolvedHubDefaultPresent — a hub-level default was observed.
	ResolvedHubDefaultPresent ResolvedHubDefault = "present"
	// ResolvedHubDefaultAbsent — the hub source was consulted and carries no value.
	ResolvedHubDefaultAbsent ResolvedHubDefault = "absent"
	// ResolvedHubDefaultUnknown — presence could not be honestly determined.
	ResolvedHubDefaultUnknown ResolvedHubDefault = "unknown"
)

// ResolvedProjectSetting is one key's entry in the resolved-settings response.
//
// NOTE FOR ANYONE ADDING A FIELD HERE: this type deliberately carries no
// effective value and no winning source. The endpoint named "resolved" does not
// resolve, because resolution is the resolver's job: computing an effective
// value here would be a second implementation of the precedence order, and a
// second implementation can silently drift from the real one.
//
// HubValue is the raw hub-configured value for display purposes only. It does
// NOT represent the effective value: template and harness-config layers may sit
// between the hub default and what an agent actually receives. Clients must
// treat it as an informational hint, never as a precedence statement.
//
// The hub-side mirror of this type is guarded by an exact-set assertion over
// JSON tags, so adding a field here without adding it there fails the build.
type ResolvedProjectSetting struct {
	// ProjectSet reports whether the project annotation is present at all.
	ProjectSet bool `json:"projectSet"`
	// ProjectValue is the raw annotation string, or null when unset.
	ProjectValue *string `json:"projectValue"`
	// HubDefault reports the existence of a hub default — not its rank.
	HubDefault ResolvedHubDefault `json:"hubDefault"`
	// HubValue is the raw hub-configured value when HubDefault is "present",
	// null otherwise. Informational only — not an effective value.
	HubValue any `json:"hubValue"`
}

// ResolvedProjectSettings is the GET /api/v1/projects/{id}/settings/resolved
// response body.
//
// Settings is keyed by raw annotation key (e.g. "scion.io/default-model"), and
// its key set is exactly the hub's authoritative project-settings registry.
//
// A consumer that needs an effective value must resolve it itself, against
// whatever precedence ladder exists at the time it asks. See ResolvedProjectSetting.
type ResolvedProjectSettings struct {
	// Project is the existing GET /settings payload, unchanged.
	Project *ProjectSettings `json:"project"`
	// Settings is keyed by annotation key.
	Settings map[string]ResolvedProjectSetting `json:"settings"`
}

// ProjectResourceSpec defines default resource requirements at the project level.
type ProjectResourceSpec struct {
	Requests *ProjectResourceList `json:"requests,omitempty"`
	Limits   *ProjectResourceList `json:"limits,omitempty"`
	Disk     string               `json:"disk,omitempty"`
}

// ProjectResourceList is a set of resource name/quantity pairs.
type ProjectResourceList struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// BucketConfig represents cloud storage configuration.
type BucketConfig struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Prefix   string `json:"prefix,omitempty"`
}

// RuntimeBroker represents a runtime broker from the Hub API.
type RuntimeBroker struct {
	ID              string              `json:"id"`
	Name            string              `json:"name"`
	Slug            string              `json:"slug"`
	Version         string              `json:"version"`
	Status          string              `json:"status"`
	ConnectionState string              `json:"connectionState"`
	LastHeartbeat   time.Time           `json:"lastHeartbeat,omitempty"`
	Capabilities    *BrokerCapabilities `json:"capabilities,omitempty"`
	Profiles        []BrokerProfile     `json:"profiles,omitempty"`
	Labels          map[string]string   `json:"labels,omitempty"`
	Annotations     map[string]string   `json:"annotations,omitempty"`
	Endpoint        string              `json:"endpoint,omitempty"`
	Projects        []BrokerProjectInfo `json:"projects,omitempty"`
	AutoProvide     bool                `json:"autoProvide,omitempty"` // Automatically add as provider for new projects
	Created         time.Time           `json:"created"`
	Updated         time.Time           `json:"updated"`
	CreatedBy       string              `json:"createdBy,omitempty"` // User ID who registered this broker
}

// UnmarshalJSON implements custom unmarshaling to support legacy grove fields.
func (b *RuntimeBroker) UnmarshalJSON(data []byte) error {
	type Alias RuntimeBroker
	aux := &struct {
		Groves []BrokerProjectInfo `json:"groves"`
		*Alias
	}{
		Alias: (*Alias)(b),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(b.Projects) == 0 && len(aux.Groves) > 0 {
		b.Projects = aux.Groves
	}
	return nil
}

// MarshalJSON implements custom marshaling to support legacy grove fields.
func (b RuntimeBroker) MarshalJSON() ([]byte, error) {
	type Alias RuntimeBroker
	return json.Marshal(&struct {
		Alias
		Groves []BrokerProjectInfo `json:"groves,omitempty"`
	}{
		Alias:  Alias(b),
		Groves: b.Projects,
	})
}

// BrokerCapabilities describes runtime broker capabilities.
type BrokerCapabilities struct {
	WebPTY bool `json:"webPty"`
	Sync   bool `json:"sync"`
	Attach bool `json:"attach"`
}

// BrokerProfile describes a runtime profile available on a broker.
type BrokerProfile struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Available bool   `json:"available"`
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// BrokerProjectInfo describes a project from a broker's perspective.
type BrokerProjectInfo struct {
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	GitRemote   string `json:"gitRemote,omitempty"`
	AgentCount  int    `json:"agentCount"`
	LocalPath   string `json:"localPath,omitempty"`
}

// UnmarshalJSON implements custom unmarshaling to support legacy grove fields.
func (i *BrokerProjectInfo) UnmarshalJSON(data []byte) error {
	type Alias BrokerProjectInfo
	aux := &struct {
		GroveID   string `json:"groveId"`
		GroveName string `json:"groveName"`
		*Alias
	}{
		Alias: (*Alias)(i),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if i.ProjectID == "" && aux.GroveID != "" {
		i.ProjectID = aux.GroveID
	}
	if i.ProjectName == "" && aux.GroveName != "" {
		i.ProjectName = aux.GroveName
	}
	return nil
}

// MarshalJSON implements custom marshaling to support legacy grove fields.
func (i BrokerProjectInfo) MarshalJSON() ([]byte, error) {
	type Alias BrokerProjectInfo
	return json.Marshal(&struct {
		Alias
		GroveID   string `json:"groveId,omitempty"`
		GroveName string `json:"groveName,omitempty"`
	}{
		Alias:     Alias(i),
		GroveID:   i.ProjectID,
		GroveName: i.ProjectName,
	})
}

// Template represents a template from the Hub API.
type Template struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Slug          string          `json:"slug"`
	DisplayName   string          `json:"displayName,omitempty"`
	Description   string          `json:"description,omitempty"`
	Harness       string          `json:"harness"`
	ContentHash   string          `json:"contentHash,omitempty"`
	Image         string          `json:"image,omitempty"`
	Config        *TemplateConfig `json:"config,omitempty"`
	Scope         string          `json:"scope"`
	ScopeID       string          `json:"scopeId,omitempty"`
	ProjectID     string          `json:"projectId,omitempty"` // Deprecated: use ScopeID
	StorageURI    string          `json:"storageUri,omitempty"`
	StorageBucket string          `json:"storageBucket,omitempty"`
	StoragePath   string          `json:"storagePath,omitempty"`
	Files         []TemplateFile  `json:"files,omitempty"`
	BaseTemplate  string          `json:"baseTemplate,omitempty"`
	Locked        bool            `json:"locked,omitempty"`
	Status        string          `json:"status"`
	OwnerID       string          `json:"ownerId,omitempty"`
	CreatedBy     string          `json:"createdBy,omitempty"`
	UpdatedBy     string          `json:"updatedBy,omitempty"`
	Visibility    string          `json:"visibility,omitempty"`
	Created       time.Time       `json:"created"`
	Updated       time.Time       `json:"updated"`
}

// UnmarshalJSON implements custom unmarshaling to support legacy grove fields.
func (t *Template) UnmarshalJSON(data []byte) error {
	type Alias Template
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(t),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if t.ProjectID == "" && aux.GroveID != "" {
		t.ProjectID = aux.GroveID
	}
	return nil
}

// MarshalJSON implements custom marshaling to support legacy grove fields.
func (t Template) MarshalJSON() ([]byte, error) {
	type Alias Template
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId,omitempty"`
	}{
		Alias:   Alias(t),
		GroveID: t.ProjectID,
	})
}

// TemplateFile represents a file within a template.
type TemplateFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Hash string `json:"hash"`
	Mode string `json:"mode,omitempty"`
}

// TemplateConfig holds template configuration.
type TemplateConfig struct {
	Harness     string            `json:"harness,omitempty"`
	Image       string            `json:"image,omitempty"`
	ConfigDir   string            `json:"configDir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Detached    bool              `json:"detached,omitempty"`
	CommandArgs []string          `json:"commandArgs,omitempty"`
	Model       string            `json:"model,omitempty"`
	Kubernetes  *KubernetesConfig `json:"kubernetes,omitempty"`
}

// KubernetesConfig holds Kubernetes-specific configuration.
type KubernetesConfig struct {
	Resources    *ResourceRequirements `json:"resources,omitempty"`
	NodeSelector map[string]string     `json:"nodeSelector,omitempty"`
}

// ResourceRequirements defines compute resource requirements.
type ResourceRequirements struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}

// User represents a user from the Hub API.
type User struct {
	ID          string           `json:"id"`
	Email       string           `json:"email"`
	DisplayName string           `json:"displayName"`
	AvatarURL   string           `json:"avatarUrl,omitempty"`
	Role        string           `json:"role"`
	Status      string           `json:"status"`
	Preferences *UserPreferences `json:"preferences,omitempty"`
	Created     time.Time        `json:"created"`
	LastLogin   time.Time        `json:"lastLogin,omitempty"`
}

// UserPreferences holds user preferences.
type UserPreferences struct {
	DefaultTemplate string `json:"defaultTemplate,omitempty"`
	DefaultProfile  string `json:"defaultProfile,omitempty"`
	Theme           string `json:"theme,omitempty"`
}

// EnvVar represents an environment variable from the Hub API.
type EnvVar struct {
	ID            string    `json:"id"`
	Key           string    `json:"key"`
	Value         string    `json:"value"`
	Scope         string    `json:"scope"`
	ScopeID       string    `json:"scopeId"`
	Description   string    `json:"description,omitempty"`
	Sensitive     bool      `json:"sensitive,omitempty"`
	InjectionMode string    `json:"injectionMode,omitempty"`
	Secret        bool      `json:"secret,omitempty"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
	CreatedBy     string    `json:"createdBy,omitempty"`
}

// Secret represents secret metadata from the Hub API.
// Note: Secret values are never returned by the API.
type Secret struct {
	ID            string    `json:"id"`
	Key           string    `json:"key"`
	SecretRef     string    `json:"secretRef,omitempty"`
	SecretType    string    `json:"type"`
	Target        string    `json:"target,omitempty"`
	Scope         string    `json:"scope"`
	ScopeID       string    `json:"scopeId"`
	Description   string    `json:"description,omitempty"`
	InjectionMode string    `json:"injectionMode,omitempty"`
	AllowProgeny  bool      `json:"allowProgeny,omitempty"`
	Version       int       `json:"version"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
	CreatedBy     string    `json:"createdBy,omitempty"`
	UpdatedBy     string    `json:"updatedBy,omitempty"`
}

// ResolvedSecret represents a secret that has been resolved from the Hub
// and is ready for projection into an agent container.
type ResolvedSecret struct {
	Name   string `json:"name"`          // Secret key name
	Type   string `json:"type"`          // environment, variable, file
	Target string `json:"target"`        // Projection target (env var name, json key, or file path)
	Value  string `json:"value"`         // Decrypted secret value
	Source string `json:"source"`        // Scope that provided this secret (user, project, runtime_broker)
	Ref    string `json:"ref,omitempty"` // External secret reference (e.g., "gcpsm:projects/123/secrets/name")
}

// UnmarshalJSON implements custom unmarshaling to support legacy "grove" source.
func (s *ResolvedSecret) UnmarshalJSON(data []byte) error {
	type Alias ResolvedSecret
	aux := (*Alias)(s)
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if s.Source == "grove" {
		s.Source = "project"
	}
	return nil
}

// HarnessConfig represents a harness config from the Hub API.
type HarnessConfig struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Slug          string             `json:"slug"`
	DisplayName   string             `json:"displayName,omitempty"`
	Description   string             `json:"description,omitempty"`
	Harness       string             `json:"harness"`
	ContentHash   string             `json:"contentHash,omitempty"`
	Config        *HarnessConfigData `json:"config,omitempty"`
	Scope         string             `json:"scope"`
	ScopeID       string             `json:"scopeId,omitempty"`
	StorageURI    string             `json:"storageUri,omitempty"`
	StorageBucket string             `json:"storageBucket,omitempty"`
	StoragePath   string             `json:"storagePath,omitempty"`
	Files         []TemplateFile     `json:"files,omitempty"`
	SourceURL     string             `json:"sourceUrl,omitempty"`
	Locked        bool               `json:"locked,omitempty"`
	Status        string             `json:"status"`
	OwnerID       string             `json:"ownerId,omitempty"`
	CreatedBy     string             `json:"createdBy,omitempty"`
	UpdatedBy     string             `json:"updatedBy,omitempty"`
	Visibility    string             `json:"visibility,omitempty"`
	Created       time.Time          `json:"created"`
	Updated       time.Time          `json:"updated"`
}

// CloneProjectRequest is the request body for POST /api/v1/projects/{id}/clone.
type CloneProjectRequest struct {
	Name       string `json:"name"`                 // required
	Slug       string `json:"slug,omitempty"`       // optional explicit slug override
	AsTemplate bool   `json:"asTemplate,omitempty"` // mark clone as template
	GitRemote  string `json:"gitRemote,omitempty"`  // override source git remote
}

// HarnessConfigData holds harness-specific configuration.
type HarnessConfigData struct {
	Harness                 string            `json:"harness,omitempty"`
	Image                   string            `json:"image,omitempty"`
	User                    string            `json:"user,omitempty"`
	Model                   string            `json:"model,omitempty"`
	Args                    []string          `json:"args,omitempty"`
	Env                     map[string]string `json:"env,omitempty"`
	AuthSelectedType        string            `json:"authSelectedType,omitempty"`
	ModelAliases            map[string]string `json:"modelAliases,omitempty"`
	ThinkingBudgetMap       map[string]int    `json:"thinkingBudgetMap,omitempty"`
	ThinkingBudgetFlag      string            `json:"thinkingBudgetFlag,omitempty"`
	ThinkingBudgetConfigKey string            `json:"thinkingBudgetConfigKey,omitempty"`
	NoAuthBehavior          string            `json:"noAuthBehavior,omitempty"`
}
