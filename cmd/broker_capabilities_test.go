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

package cmd

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// heartbeatOnlyCapabilities are the capabilities a heartbeat adds to the
// static set that the hub does not store. TemplateTiers is not one: the
// heartbeat adds it to the static set (pinned in pkg/runtimebroker,
// TestHeartbeatCapabilities_StaticSetPlusTemplateTiers), and the hub stores
// it from the heartbeat (store.BrokerCapabilities.TemplateTiers,
// ptone/scion#4222). Registration leaves it false, the static set's value.
var heartbeatOnlyCapabilities = map[string]bool{}

// TestCapabilities_RegistrationMatchesHeartbeat: what a broker registers
// (the embedded registrations' stored capabilities and the capability names
// a remote registration sends) agrees with the static set the heartbeat
// builds on, for every stored capability. The one capability the heartbeat
// adds to that set, TemplateTiers, is registered false and becomes true at
// the first heartbeat.
// Heartbeat-only capabilities are absent from the static set, the
// registration names and the stored capabilities.
func TestCapabilities_RegistrationMatchesHeartbeat(t *testing.T) {
	for name, rt := range map[string]runtime.Runtime{
		"docker": runtime.NewDockerRuntime(),
		"nil":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			heartbeat := runtimebroker.StaticCapabilities(rt)
			registered := flatInstanceCapabilities(rt)
			names := runtimebroker.StaticCapabilityNames(rt)

			hv := reflect.ValueOf(*heartbeat)
			rv := reflect.ValueOf(*registered)
			for i := 0; i < hv.NumField(); i++ {
				f := hv.Type().Field(i)
				stored := rv.FieldByName(f.Name)
				if heartbeatOnlyCapabilities[f.Name] {
					tag := strings.Split(f.Tag.Get("json"), ",")[0]
					assert.False(t, hv.Field(i).Bool(), "the static set (registered) carries the heartbeat-only %s", f.Name)
					assert.False(t, slices.Contains(names, tag), "registration names carry the heartbeat-only %s", tag)
					assert.False(t, stored.IsValid(), "store.BrokerCapabilities has the heartbeat-only %s; update heartbeatOnlyCapabilities", f.Name)
					continue
				}
				require.True(t, stored.IsValid(), "store.BrokerCapabilities has no %s", f.Name)
				assert.Equal(t, hv.Field(i).Bool(), stored.Bool(), "embedded registration and heartbeat disagree on %s", f.Name)
				if f.Name == "StartsInFlight" {
					continue // reported with the heartbeat's starts-in-flight list, not a static capability
				}
				tag := strings.Split(f.Tag.Get("json"), ",")[0]
				assert.Equal(t, hv.Field(i).Bool(), slices.Contains(names, tag), "registration names and heartbeat disagree on %s", tag)
			}
			assert.Equal(t, reflect.TypeOf(hubclient.BrokerCapabilities{}).NumField()-len(heartbeatOnlyCapabilities), rv.NumField(), "every capability but the heartbeat-only ones has a stored counterpart")
		})
	}
}
