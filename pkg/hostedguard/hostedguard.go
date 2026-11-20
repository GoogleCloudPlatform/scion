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
	"os"
	"sort"
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
// names the field and the entry as written (a volume target, an env name,
// a hub key), never a host path, server or value.
type RefusalError struct {
	// Field is the config field ("volume", "env" or "hub").
	Field string
	// Entry identifies the refused entry within Field (a volume target, an
	// env name, a hub key).
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
	if err := checkVolumes(cfg.Volumes, o, runtimeKind); err != nil {
		return err
	}
	if err := CheckMemberEnv(cfg.Env, o); err != nil {
		return err
	}
	return checkHub(cfg.Hub, o)
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
		// Runtimes recognize only the exact spelling.
		if v.Type != "gcs" {
			return fmt.Sprintf("volume type must be spelled \"gcs\" in %s on a hosted server; %s", o, volumeRemedy)
		}
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

// reservedEnvPrefixes are the env name prefixes reserved for scion's own
// control-plane variables, the same set the Hub reserves for env secrets and
// config edits (secret.IsReservedEnvTarget).
var reservedEnvPrefixes = []string{"SCION_", "GCE_METADATA_"}

// memberEnvScope is the api.MemberEnvScope of content from o. Agent config
// uses the create scope here (create requests and stored inline config); an
// agent config edit is held to the narrower edit scope by the Hub's own PATCH
// check, which runs first. Other origins get no exemption.
func memberEnvScope(o Origin) api.MemberEnvScope {
	switch o.Kind {
	case OriginAgentConfig:
		return api.MemberEnvCreate
	case OriginHarnessConfig:
		return api.MemberEnvHarnessConfig
	}
	return 0
}

// IsReservedEnvName reports whether the env name is reserved for scion
// itself (SCION_*, GCE_METADATA_*). Members may set a reserved name only from
// the sources api.MemberSettableEnv lists for it.
func IsReservedEnvName(name string) bool {
	for _, p := range reservedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

const envReferenceReason = "host environment references and empty passthrough values in %s are not allowed on a hosted server; use a literal value or a secret"

// CheckMemberEnv applies the env rule to member-written env from o: a name
// or value the Runtime Broker would expand against its own environment
// ($NAME, ${...}), an empty value (which takes the variable of the same
// name from the broker's environment), and a name reserved for scion's own
// variables are refused, unless api.MemberSettableEnv lists the reserved
// name for o's source. Such a name may also be empty (the configure page
// clears an auto-expose key that way): it can only take the broker's own
// setting of the same scion variable. It returns nil or a *RefusalError for
// the first refused name, in name order. The message names the env name,
// never the value.
func CheckMemberEnv(env map[string]string, o Origin) error {
	if len(env) == 0 {
		return nil
	}
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	scope := memberEnvScope(o)
	for _, k := range names {
		v := env[k]
		settable := IsReservedEnvName(k) && api.IsMemberSettableEnv(k, scope)
		var reason string
		switch {
		case expandsHostEnv(k) || expandsHostEnv(v) || (v == "" && !settable):
			reason = fmt.Sprintf(envReferenceReason, o)
		case IsReservedEnvName(k) && !settable:
			reason = fmt.Sprintf("names reserved for scion's own environment variables in %s are not allowed on a hosted server", o)
		default:
			continue
		}
		return &RefusalError{Field: "env", Entry: k, Origin: o, Reason: reason}
	}
	return nil
}

// expandsHostEnv reports whether expanding s the way the Runtime Broker
// expands config env (os.Expand) would change it: a $NAME or ${...}
// reference, including malformed ones that expansion removes.
func expandsHostEnv(s string) bool {
	if !strings.Contains(s, "$") {
		return false
	}
	return os.Expand(s, func(string) string { return "" }) != s
}

// checkHub applies the hub rule: member config may not set the agent's Hub
// connection.
func checkHub(h *api.AgentHubConfig, o Origin) error {
	if h == nil || *h == (api.AgentHubConfig{}) {
		return nil
	}
	return &RefusalError{Field: "hub", Entry: "endpoint", Origin: o,
		Reason: fmt.Sprintf("a hub endpoint in %s is not allowed on a hosted server; the server sets the agent's hub connection", o)}
}
