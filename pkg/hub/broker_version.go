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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// Runtime Broker version comparison (ptone/scion#5972). The hub compares
// each Runtime Broker's stored build token with the fleet hub version and
// reports the result as match, mismatch or unknown; the web and the CLI
// never compare versions themselves.

// hubBuildToken is this hub binary's build token: version.Short(), bounded
// like every stored version. Hub-instance registry rows and the co-located
// Runtime Broker rows are written with it, so both sides of a comparison
// come from the same expression.
func hubBuildToken() string {
	return boundedPrintable(version.Short(), hubInstanceMaxVersionBytes)
}

// HubBuildToken is hubBuildToken for the co-located Runtime Broker
// registration in cmd.
func HubBuildToken() string {
	return hubBuildToken()
}

// brokerVersionToken bounds a Runtime Broker supplied version the same way
// as hubBuildToken. It is applied when a version is stored and again when
// one is read, so rows stored before the bound existed are covered too.
func brokerVersionToken(v string) string {
	return boundedPrintable(v, hubInstanceMaxVersionBytes)
}

// brokerVersionStatus compares a Runtime Broker's stored version with the
// fleet hub version (HealthSummaryHub.Version). A "mixed" or empty fleet
// version has no single hub version to compare against, so every broker is
// unknown.
func brokerVersionStatus(hubVersion, brokerVersion string) string {
	if hubVersion == HubVersionMixed {
		hubVersion = ""
	}
	return string(version.CompareBuild(hubVersion, brokerVersionToken(brokerVersion)))
}

// hubFleetVersion is the fleet hub version for the runtime broker list and
// get handlers: the version every live hub instance reports, "mixed" when
// they differ, or "" when none is live or the registry cannot be read. It is
// the same value as the health summary's hub.version.
func (s *Server) hubFleetVersion(ctx context.Context) string {
	rows, now, err := s.store.ListHubInstances(ctx, hubInstanceDisplayWindow)
	if err != nil {
		slog.Warn("runtime brokers: failed to list hub instances for the version comparison", "error", err)
		return ""
	}
	return buildHealthSummaryFleetHub(rows, now, true).Version
}

// runtimeBrokerVersionStatus is the versionStatus of a runtime broker list
// or get row: empty (omitted) for a plugin record, which carries no Scion
// build version.
func runtimeBrokerVersionStatus(b *store.RuntimeBroker, hubVersion string) string {
	if isPluginBroker(b) {
		return ""
	}
	return brokerVersionStatus(hubVersion, b.Version)
}
