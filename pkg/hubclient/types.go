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

import "time"

// Agent represents an agent from the Hub API.
type Agent struct {
	ID              string            `json:"id"`              // Hub UUID (database primary key)
	Slug            string            `json:"slug"`            // URL-safe slug identifier (unique per grove)
	ContainerID     string            `json:"containerId"`     // Runtime container ID (ephemeral)
	Name            string            `json:"name"`
	Template        string            `json:"template,omitempty"`
	GroveID         string            `json:"groveId,omitempty"`
	Grove           string            `json:"grove,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	Status          string            `json:"status"`
	ConnectionState string            `json:"connectionState,omitempty"`
	ContainerStatus string            `json:"containerStatus,omitempty"`
	SessionStatus   string            `json:"sessionStatus,omitempty"`
	RuntimeState    string            `json:"runtimeState,omitempty"`
	Image           string            `json:"image,omitempty"`
	Detached        bool              `json:"detached,omitempty"`
	Runtime         string            `json:"runtime,omitempty"`
	RuntimeBrokerID   string            `json:"runtimeBrokerId,omitempty"`
	RuntimeBrokerName string            `json:"runtimeBrokerName,omitempty"`
	RuntimeBrokerType string            `json:"runtimeBrokerType,omitempty"`
	WebPTYEnabled   bool              `json:"webPtyEnabled,omitempty"`
	TaskSummary     string            `json:"taskSummary,omitempty"`
	AppliedConfig   *AgentConfig      `json:"appliedConfig,omitempty"`
	DirectConnect   *DirectConnect    `json:"directConnect,omitempty"`
	Kubernetes      *KubernetesInfo   `json:"kubernetes,omitempty"`
	Created         time.Time         `json:"created"`
	Updated         time.Time         `json:"updated"`
	LastSeen        time.Time         `json:"lastSeen,omitempty"`
	CreatedBy       string            `json:"createdBy,omitempty"`
	OwnerID         string            `json:"ownerId,omitempty"`
	Visibility      string            `json:"visibility,omitempty"`
	StateVersion    int64             `json:"stateVersion,omitempty"`
}

// AgentConfig represents agent configuration.
type AgentConfig struct {
	Image   string            `json:"image,omitempty"`
	Harness string            `json:"harness,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Model   string            `json:"model,omitempty"`
	Task    string            `json:"task,omitempty"`
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

// Grove represents a grove from the Hub API.
type Grove struct {
	ID                   string             `json:"id"`
	Name                 string             `json:"name"`
	Slug                 string             `json:"slug"`
	GitRemote            string             `json:"gitRemote,omitempty"`
	DefaultRuntimeBrokerID string             `json:"defaultRuntimeBrokerId,omitempty"`
	Created              time.Time          `json:"created"`
	Updated              time.Time          `json:"updated"`
	CreatedBy            string             `json:"createdBy,omitempty"`
	OwnerID              string             `json:"ownerId,omitempty"`
	Visibility           string             `json:"visibility,omitempty"`
	Labels               map[string]string  `json:"labels,omitempty"`
	Annotations          map[string]string  `json:"annotations,omitempty"`
	Providers            []GroveProvider    `json:"providers,omitempty"`
	AgentCount           int                `json:"agentCount,omitempty"`
	ActiveBrokerCount      int                `json:"activeBrokerCount,omitempty"`
}

// GroveProvider represents a broker providing runtime services to a grove.
type GroveProvider struct {
	BrokerID   string    `json:"brokerId"`
	BrokerName string    `json:"brokerName"`
	Status     string    `json:"status"`
	LastSeen   time.Time `json:"lastSeen,omitempty"`
	LocalPath  string    `json:"localPath,omitempty"`
	LinkedBy   string    `json:"linkedBy,omitempty"` // User ID who performed the link
	LinkedAt   time.Time `json:"linkedAt,omitempty"` // Timestamp when the link was created
}

// GroveSettings represents grove configuration settings.
type GroveSettings struct {
	ActiveProfile   string                 `json:"activeProfile,omitempty"`
	DefaultTemplate string                 `json:"defaultTemplate,omitempty"`
	Bucket          *BucketConfig          `json:"bucket,omitempty"`
	Runtimes        map[string]interface{} `json:"runtimes,omitempty"`
	Harnesses       map[string]interface{} `json:"harnesses,omitempty"`
	Profiles        map[string]interface{} `json:"profiles,omitempty"`
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
	Groves          []BrokerGroveInfo   `json:"groves,omitempty"`
	AutoProvide     bool                `json:"autoProvide,omitempty"` // Automatically add as provider for new groves
	Created         time.Time           `json:"created"`
	Updated         time.Time           `json:"updated"`
	CreatedBy       string              `json:"createdBy,omitempty"` // User ID who registered this broker
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

// BrokerGroveInfo describes a grove from a broker's perspective.
type BrokerGroveInfo struct {
	GroveID    string `json:"groveId"`
	GroveName  string `json:"groveName"`
	GitRemote  string `json:"gitRemote,omitempty"`
	AgentCount int    `json:"agentCount"`
	LocalPath  string `json:"localPath,omitempty"`
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
	GroveID       string          `json:"groveId,omitempty"` // Deprecated: use ScopeID
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
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	Value       string    `json:"value"`
	Scope       string    `json:"scope"`
	ScopeID     string    `json:"scopeId"`
	Description string    `json:"description,omitempty"`
	Sensitive   bool      `json:"sensitive,omitempty"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	CreatedBy   string    `json:"createdBy,omitempty"`
}

// Secret represents secret metadata from the Hub API.
// Note: Secret values are never returned by the API.
type Secret struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	Scope       string    `json:"scope"`
	ScopeID     string    `json:"scopeId"`
	Description string    `json:"description,omitempty"`
	Version     int       `json:"version"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	CreatedBy   string    `json:"createdBy,omitempty"`
	UpdatedBy   string    `json:"updatedBy,omitempty"`
}
