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
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth/adcsource"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// hubTransportSettings returns the transport-layer auth settings (IAP,
// Cloud Run invoker) for reaching the hub at endpoint. The transport of
// that hub's entry in the hub list wins; otherwise hub.transport from
// settings applies. It returns nil when neither is set.
func hubTransportSettings(settings *config.Settings, endpoint string) *transportauth.TransportSettings {
	if t := hubEntryTransport(endpoint); t != nil {
		return &transportauth.TransportSettings{Mode: t.Mode, Audience: t.Audience}
	}
	if settings != nil && settings.Hub != nil && settings.Hub.Transport != nil {
		return &transportauth.TransportSettings{Mode: settings.Hub.Transport.Mode, Audience: settings.Hub.Transport.Audience}
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

// hubEntryTransportSource returns the transport token source for the
// audience of a hub list entry: the metadata server on GCE (unless it is
// redirected), otherwise application default credentials.
var hubEntryTransportSource = func(audience string) (transportauth.TokenSource, error) {
	if transportauth.IsOnGCEFunc() && !transportauth.IsMetadataRedirected(os.Getenv(transportauth.EnvMetadataMode)) {
		return transportauth.NewMetadataSource(audience), nil
	}
	return adcsource.New(audience)
}

// hubEntryTransportAuth returns the transport auth option for the hub list
// checks ('hub add', 'hub list --check'), which reach hubs other than the
// effective one. They use the transport of the hub's own entry and nothing
// else: transport auth from the environment belongs to the effective hub
// and is never inherited, and without an entry transport they use none.
func hubEntryTransportAuth(endpoint string) hubclient.Option {
	t := hubEntryTransport(endpoint)
	if t == nil || t.Audience == "" {
		return hubclient.WithTransportAuth(nil, transportauth.HeaderAuthorization)
	}
	src, err := hubEntryTransportSource(t.Audience)
	if err != nil {
		util.Debugf("No transport credential for hub %s: %v", endpoint, err)
		return hubclient.WithTransportAuth(nil, transportauth.HeaderAuthorization)
	}
	return hubclient.WithTransportAuth(src, transportauth.ModeFromString(t.Mode))
}
