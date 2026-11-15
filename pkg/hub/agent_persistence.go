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
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// An agent's persistence (ptone/scion#7780) is resolved once, at create,
// and stamped on the agent before its first dispatch. Every dispatch then
// sends the stamped value; an agent with no stamp is persistent.
const (
	annotationPersistence       = "scion.dev/persistence"
	annotationPersistenceSource = "scion.dev/persistence-source"
)

// ErrCodePersistenceUnsupported refuses an ephemeral create on a Runtime
// Broker that does not advertise the ephemeralPersistence capability.
const ErrCodePersistenceUnsupported = "persistence_unsupported"

// persistenceInput is what resolveAgentPersistence decides from.
type persistenceInput struct {
	// Request is the agent config's persistence (create request config).
	Request api.Persistence
	// Template is the template's persistence and TemplateName its name.
	// Not wired yet: the template field comes with ptone/scion#7956.
	Template     api.Persistence
	TemplateName string
	// BrokerName names the Runtime Broker in a refusal ("" when none).
	BrokerName string
	// BrokerSupports is the broker's ephemeralPersistence capability.
	BrokerSupports bool
	// GateOn is experiments.CloudRunEphemeralAgents.
	GateOn bool
}

// persistenceRefusal is a create refused by resolveAgentPersistence.
type persistenceRefusal struct {
	Status  int
	Code    string
	Message string
	Details map[string]interface{}
}

func (e *persistenceRefusal) Error() string { return e.Message }

// resolveAgentPersistence returns the agent's persistence and its source:
// the agent config's value, else the template's, else none (persistent,
// with an empty value and source, so nothing is stamped). Ephemeral is
// refused with experiment_disabled while the gate is off, and with
// persistence_unsupported on a broker without the capability; both
// messages name the source.
func resolveAgentPersistence(in persistenceInput) (api.Persistence, string, *persistenceRefusal) {
	p, source := in.Request, "the agent config"
	if p == "" {
		p, source = in.Template, "template "+in.TemplateName
	}
	if p == "" {
		return "", "", nil
	}
	if err := p.Validate(); err != nil {
		return "", "", &persistenceRefusal{Status: http.StatusBadRequest, Code: ErrCodeValidationError,
			Message: fmt.Sprintf("%v (from %s)", err, source)}
	}
	if !p.IsEphemeral() {
		return p, source, nil
	}
	if !in.GateOn {
		return "", "", &persistenceRefusal{Status: http.StatusPreconditionFailed, Code: ErrCodeExperimentDisabled,
			Message: fmt.Sprintf("Ephemeral persistence (from %s) is not enabled on this Hub (experiment %s is off)",
				source, experiments.CloudRunEphemeralAgents),
			Details: map[string]interface{}{"experiment": experiments.CloudRunEphemeralAgents}}
	}
	if !in.BrokerSupports {
		broker := in.BrokerName
		if broker == "" {
			broker = "(none selected)"
		}
		return "", "", &persistenceRefusal{Status: http.StatusUnprocessableEntity, Code: ErrCodePersistenceUnsupported,
			Message: fmt.Sprintf("Ephemeral persistence (from %s) isn't available on Runtime Broker %s; it is supported on Cloud Run brokers only",
				source, broker)}
	}
	return p, source, nil
}

// brokerSupportsEphemeral reports the broker's ephemeralPersistence
// capability; a nil broker has none.
func brokerSupportsEphemeral(b *store.RuntimeBroker) bool {
	return b != nil && b.Capabilities != nil && b.Capabilities.EphemeralPersistence
}

// persistenceBrokerName names b in a refusal ("" for none).
func persistenceBrokerName(b *store.RuntimeBroker) string {
	if b == nil {
		return ""
	}
	return brokerDisplayName(b)
}

// stampAgentPersistence records a resolved persistence and its source on a
// not-yet-stored agent. An empty value stamps nothing.
func stampAgentPersistence(a *store.Agent, p api.Persistence, source string) {
	if p == "" {
		return
	}
	if a.Annotations == nil {
		a.Annotations = map[string]string{}
	}
	a.Annotations[annotationPersistence] = string(p)
	a.Annotations[annotationPersistenceSource] = source
}

// dispatchPersistence is the persistence every dispatch of a carries: the
// stamp when it is ephemeral, otherwise none, so a persistent agent's
// dispatch is unchanged.
func dispatchPersistence(a *store.Agent) api.Persistence {
	if a != nil && api.Persistence(a.Annotations[annotationPersistence]).IsEphemeral() {
		return api.PersistenceEphemeral
	}
	return ""
}

// keepPersistenceStamp returns next (an annotations update) with the
// persistence stamp of prev in place of anything next says about it: the
// value is fixed at create and an update can neither set nor clear it.
func keepPersistenceStamp(prev, next map[string]string) map[string]string {
	for _, k := range []string{annotationPersistence, annotationPersistenceSource} {
		v, ok := prev[k]
		if !ok {
			delete(next, k)
			continue
		}
		if next == nil {
			next = map[string]string{}
		}
		next[k] = v
	}
	return next
}
