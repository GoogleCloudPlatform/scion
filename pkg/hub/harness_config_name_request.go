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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// harnessConfigNameRefusal is the fixed error for a harness-config name in an
// agent request that is not exactly one path component. It never contains
// the value.
const harnessConfigNameRefusal = "harness config must be a harness-config name"

// harnessConfigNameAllowed reports whether a harness-config name that
// reaches agent creation is accepted: empty, or exactly one path component
// (api.IsSingleNameComponent). A Runtime Broker joins the name under its
// harness-configs directories, so a value with separators, "..", an
// absolute path or a remote source is never accepted.
func harnessConfigNameAllowed(v string) bool {
	return v == "" || api.IsSingleNameComponent(v)
}

// createRequestHarnessConfigAllowed reports whether every harness-config
// name an agent create request carries (the top-level field and the
// inline config's) is accepted (harnessConfigNameAllowed).
func createRequestHarnessConfigAllowed(req *CreateAgentRequest) bool {
	if !harnessConfigNameAllowed(req.HarnessConfig) {
		return false
	}
	return req.Config == nil || harnessConfigNameAllowed(req.Config.HarnessConfig)
}

// Default tiers named by dropUnusableDefaultHarnessConfig's warning.
const (
	// harnessConfigTierRecorded is an explicit value replayed from an
	// agent's recorded create inputs (reincarnate), written before request
	// values were checked.
	harnessConfigTierRecorded = "recorded"
	harnessConfigTierProject  = "project default"
	harnessConfigTierTemplate = "template default"
	harnessConfigTierHub      = "hub default"
)

// dropUnusableDefaultHarnessConfig clears a harness-config name that a
// default (tier) filled into ac when it is not exactly one path component,
// so it is never stored or dispatched, and reports whether it did. The
// warning names the tier and the agent, never the value. Request values
// are refused before this point (createRequestHarnessConfigAllowed); a
// value recorded before that check (harnessConfigTierRecorded) is dropped
// the same way.
func dropUnusableDefaultHarnessConfig(ctx context.Context, agent *store.Agent, tier string) bool {
	ac := agent.AppliedConfig
	if ac == nil || harnessConfigNameAllowed(ac.HarnessConfig) {
		return false
	}
	ac.HarnessConfig = ""
	slog.Warn("harness config ignored: not a harness-config name",
		"tier", tier, "agent", agent.Name, "agent_id", agent.ID)
	addDispatchWarnings(ctx, "the "+tier+" harness config was ignored: it is not a harness-config name")
	return true
}
