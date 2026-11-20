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

// Package hostedguard checks agent configuration written by project
// members before it is used on a hosted server. Config the server operator
// wrote (profiles, server settings, global harness configs) is not checked
// here; callers apply the check only where member content enters.
//
// The package stays dependency-light so that both the Hub and the Runtime
// Broker can import it.
package hostedguard

import (
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// ErrorCode is the stable code for refused member config, for API error
// bodies and logs.
const ErrorCode = "hosted_config_refused"

// Origin kinds, naming where checked content came from.
const (
	OriginAgentConfig   = "agent config"
	OriginHarnessConfig = "harness config"
	OriginTemplate      = "template"
)

// RuntimeKubernetes is the runtime kind for agents run on Kubernetes.
// Pass "" as the runtime kind when the runtime is not known (Hub side).
const RuntimeKubernetes = "kubernetes"

// Origin names where checked content came from, for the error message.
type Origin struct {
	// Kind is one of the Origin* constants.
	Kind string
	// Name is the harness-config or template name; empty for agent config.
	Name string
}

func (o Origin) String() string {
	if o.Name == "" {
		return o.Kind
	}
	return fmt.Sprintf("%s %q", o.Kind, o.Name)
}

// RefusalError reports one refused entry of member config. Its message
// names the field and the entry as written (a volume target), never a
// host path, server or value.
type RefusalError struct {
	// Field is the config field ("volume").
	Field string
	// Entry identifies the refused entry within Field (a volume target).
	Entry string
	// Origin is where the entry came from.
	Origin Origin
	// Reason says why it is refused and what to do instead; it names
	// Origin.
	Reason string
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("%s %q: %s", e.Field, e.Entry, e.Reason)
}

// CheckMemberConfig refuses member-written config that names host
// resources on a hosted server. runtimeKind is the runtime that will run
// the agent (RuntimeKubernetes, another runtime's name, or "" when not
// known). It returns nil or a *RefusalError for the first refused entry.
//
// Callers check only on a hosted server and only member sources.
func CheckMemberConfig(cfg *api.ScionConfig, o Origin, runtimeKind string) error {
	if cfg == nil {
		return nil
	}
	return checkVolumes(cfg.Volumes, o, runtimeKind)
}

const volumeRemedy = "use a project shared directory or ask the server operator to configure the volume"

// checkVolumes applies the volume rule: every member-written volume is
// refused except gcs on Kubernetes. When the runtime is not known, gcs is
// let through and the Runtime Broker decides.
func checkVolumes(vols []api.VolumeMount, o Origin, runtimeKind string) error {
	for _, v := range vols {
		reason := volumeRefusal(v, o, runtimeKind)
		if reason == "" {
			continue
		}
		return &RefusalError{Field: "volume", Entry: v.Target, Origin: o, Reason: reason}
	}
	return nil
}

// volumeRefusal returns why v from o is refused, or "" when it is allowed.
func volumeRefusal(v api.VolumeMount, o Origin, runtimeKind string) string {
	switch t := strings.ToLower(strings.TrimSpace(v.Type)); t {
	case "", "local":
		return fmt.Sprintf("host volumes in %s are not allowed on a hosted server; %s", o, volumeRemedy)
	case "gcs":
		if runtimeKind == "" || runtimeKind == RuntimeKubernetes {
			return ""
		}
		return fmt.Sprintf("gcs volumes in %s are allowed on a hosted server only for agents run on Kubernetes; %s", o, volumeRemedy)
	case "nfs", "cloudrun-volume", "gke-shared-volume":
		return fmt.Sprintf("%s volumes in %s are not allowed on a hosted server; %s", t, o, volumeRemedy)
	default:
		return fmt.Sprintf("volumes of this type in %s are not allowed on a hosted server; %s", o, volumeRemedy)
	}
}
