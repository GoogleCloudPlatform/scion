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

package telemetry

import (
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// SessionUsage is one usage increment for the session metrics aggregator:
// model/API calls and their token counts. The native UsageDeriver produces
// one per derived increment, and Aggregator.RecordUsage adds it.
type SessionUsage struct {
	Calls           int64
	TokensInput     int64
	TokensOutput    int64
	TokensCached    int64
	TokensReasoning int64
}

// IsZero reports whether u adds nothing.
func (u SessionUsage) IsZero() bool {
	return u == SessionUsage{}
}

// SessionUsageSink receives the usage the native UsageDeriver derives, so it
// can be added to the open session's metrics. It is called from the OTLP
// receiver's request goroutines, possibly concurrently.
type SessionUsageSink func(SessionUsage)

// HookUsageFeedsSessionMetrics reports whether usage carried on hook events
// (model-end token counts and session-end token totals) feeds the session
// metrics aggregator. With SCION_USAGE_SOURCE=native the native UsageDeriver
// owns usage, and the init daemon adds it to the session through its
// SessionUsageSink, so hook usage is left out and a call is never counted
// twice. Any other value, including unset, keeps hook usage as the source.
func HookUsageFeedsSessionMetrics() bool {
	return os.Getenv("SCION_USAGE_SOURCE") != UsageSourceNative
}

// sessionUsageFromIncrement maps a derived increment onto the aggregator's
// fields the same way hook model-end events map: input, output, cache_read
// (as cached) and reasoning. cache_write has no aggregator field, as on the
// hook path.
func sessionUsageFromIncrement(increment usageIncrement) SessionUsage {
	return SessionUsage{
		Calls:           increment.Calls,
		TokensInput:     increment.Tokens[telemetrycontract.TokenTypeInput],
		TokensOutput:    increment.Tokens[telemetrycontract.TokenTypeOutput],
		TokensCached:    increment.Tokens[telemetrycontract.TokenTypeCacheRead],
		TokensReasoning: increment.Tokens[telemetrycontract.TokenTypeReasoning],
	}
}

// SetSessionUsageSink sets the sink that receives native derived usage. A
// nil sink stops forwarding. Safe to call at any time, including before or
// after the usage deriver is created or activated.
func (p *Pipeline) SetSessionUsageSink(sink SessionUsageSink) {
	if p == nil {
		return
	}
	if sink == nil {
		p.sessionUsageSink.Store(nil)
		return
	}
	p.sessionUsageSink.Store(&sink)
}

// forwardSessionUsage passes u to the current sink, if any. It is the
// UsageDeriver's sessionUsage hook for derivers this pipeline creates.
func (p *Pipeline) forwardSessionUsage(u SessionUsage) {
	if sink := p.sessionUsageSink.Load(); sink != nil {
		(*sink)(u)
	}
}
