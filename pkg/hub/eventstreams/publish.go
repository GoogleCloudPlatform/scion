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

package eventstreams

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/ssemetrics"
)

type heartbeatTriggerKey struct{}

// WithHeartbeatTrigger marks ctx so that an agent status event published
// with it counts as trigger=heartbeat in scion.hub.events.published. The
// heartbeat paths set it: an agent heartbeat report with nothing else in it,
// and the Runtime Broker heartbeat loop. It changes nothing but the count.
func WithHeartbeatTrigger(ctx context.Context) context.Context {
	return context.WithValue(ctx, heartbeatTriggerKey{}, true)
}

// Trigger returns the scion.hub.events.published trigger for a publish made
// with ctx: ssemetrics.TriggerHeartbeat if WithHeartbeatTrigger marked it,
// ssemetrics.TriggerChange otherwise.
func Trigger(ctx context.Context) string {
	if ctx != nil {
		if v, _ := ctx.Value(heartbeatTriggerKey{}).(bool); v {
			return ssemetrics.TriggerHeartbeat
		}
	}
	return ssemetrics.TriggerChange
}

// subjectIDPosition is the token position of the resource id in subjects of
// each category, keyed by the category's leading tokens. Categories absent
// here carry no id (admin.*, notification.*, conduit.*).
var subjectIDPosition = map[string]int{
	"agent":           1,
	"project":         1,
	"user":            1,
	"broker":          1,
	"broker.dispatch": 2,
	"system.images":   2,
}

// subjectFixedCategories are the categories whose subjects carry no
// resource id and are kept whole.
var subjectFixedCategories = map[string]bool{
	"admin":        true,
	"notification": true,
	"conduit":      true,
}

// SubjectClass returns the scion.hub.events.published subject_class of a
// published subject: the subject with its resource id removed, for example
// agent.status for agent.<id>.status and project.agent.status for
// project.<id>.agent.status. A subject of an unknown category is "other",
// so the attribute stays bounded whatever is published.
func SubjectClass(subject string) string {
	tokens := strings.Split(subject, ".")
	if len(tokens) == 0 || tokens[0] == "" {
		return "other"
	}
	pos, ok := 0, false
	if len(tokens) > 1 {
		pos, ok = subjectIDPosition[tokens[0]+"."+tokens[1]]
	}
	if !ok {
		pos, ok = subjectIDPosition[tokens[0]]
	}
	switch {
	case ok:
		if pos >= len(tokens) {
			return strings.Join(tokens, ".")
		}
		return strings.Join(append(append([]string{}, tokens[:pos]...), tokens[pos+1:]...), ".")
	case subjectFixedCategories[tokens[0]]:
		return subject
	default:
		return "other"
	}
}

// PublishCounter counts published events on scion.hub.events.published.
// Each event publisher embeds one and calls Count once per event it
// publishes. The zero value counts nothing until SetMetrics.
type PublishCounter struct {
	rec atomic.Pointer[recorderBox]
}

// SetMetrics wires the recorder. Nil disables counting.
func (c *PublishCounter) SetMetrics(rec ssemetrics.Recorder) {
	if rec == nil {
		c.rec.Store(nil)
		return
	}
	c.rec.Store(&recorderBox{rec: rec})
}

// Count counts one event published on subject with the given trigger.
func (c *PublishCounter) Count(subject, trigger string) {
	if b := c.rec.Load(); b != nil {
		b.rec.IncPublished(context.Background(), SubjectClass(subject), trigger)
	}
}
