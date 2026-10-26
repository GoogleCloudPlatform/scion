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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
)

// hubAPIStartupPreflight runs the standalone Hub API listener check at
// startup, before the store, dev auth and the hub are initialised: with dev
// auth enabled (--dev-auth, server.auth.dev_mode or the workstation
// default), the Hub API must listen on a loopback host.
//
// It applies only when the hub runs its own HTTP server (hub enabled, web
// disabled). With the web server enabled the Hub API is mounted on it, and
// webStartupPreflight already refuses dev auth on a non-loopback host.
//
// The listen host is cfg.Hub.Host, which the standalone hub binds, with an
// empty value normalised to 0.0.0.0 (an empty host binds every interface).
// Loopback is the same rule the web path uses (hub.IsLoopbackHost). Like
// webStartupPreflight, it is gated on cfg.Auth.Enabled, so it refuses
// before a dev token exists.
func hubAPIStartupPreflight(cfg *config.GlobalConfig, hubEnabled, webEnabled bool) error {
	if !hubEnabled || webEnabled {
		return nil
	}
	host := webListenHost(cfg)
	if cfg.Auth.Enabled && !hub.IsLoopbackHost(host) {
		return hubAPIDevAuthBindError(host)
	}
	return nil
}

// hubAPIDevAuthBindError is the refusal for dev auth on a standalone Hub
// API bound to a non-loopback host. It names the flags and settings that
// change either side; the only configured value it shows is the host.
func hubAPIDevAuthBindError(host string) error {
	return fmt.Errorf(
		"dev auth requires a loopback hub host, but the Hub API is bound to a non-loopback address (%s). "+
			"Either bind to 127.0.0.1/::1/localhost (--host 127.0.0.1, or server.hub.host in settings) "+
			"or disable dev auth (--dev-auth=false, or server.auth.dev_mode: false in settings)", host)
}
