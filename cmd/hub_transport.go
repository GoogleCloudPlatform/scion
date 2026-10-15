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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// hubTransportSettings returns the transport-layer auth settings (IAP,
// Cloud Run invoker) for reaching the hub at endpoint: the transport of
// that hub's entry in the hub list, or nil. hub.transport in settings is
// not read. The environment (SCION_TRANSPORT_*) still wins over the
// result in transportauth.FromSettings.
func hubTransportSettings(endpoint string) *transportauth.TransportSettings {
	if t := hubEntryTransport(endpoint); t != nil {
		return &transportauth.TransportSettings{Mode: t.Mode, Audience: t.Audience}
	}
	return nil
}

// hubEntryTransport returns the transport of the hub list entry for
// endpoint, or nil. Agents never read the hub list.
func hubEntryTransport(endpoint string) *config.HubTransportConfig {
	if endpoint == "" || contextDisabled(resolveMode()) {
		return nil
	}
	c, err := config.LoadCLIContext()
	if err != nil {
		return nil
	}
	_, h := c.HubByEndpoint(endpoint)
	if h == nil || h.Transport == nil || (h.Transport.Mode == "" && h.Transport.Audience == "") {
		return nil
	}
	return h.Transport
}
