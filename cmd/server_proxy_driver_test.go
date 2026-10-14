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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// WS2 (ptone/scion#4240): the web server is never built with a driver
// principal list the hub would refuse: a non-empty list outside proxy auth
// mode, or an entry that is not one exact email, stops startup.
func TestInitWebServer_ProxyDriverPrincipals(t *testing.T) {
	drivers := []string{"ui-driver@ti-test.iam.gserviceaccount.com"}
	newCfg := func(mode string, list []string) *config.GlobalConfig {
		cfg := &config.GlobalConfig{}
		cfg.Hub.Host = "127.0.0.1"
		cfg.Auth.Mode = mode
		cfg.Auth.Proxy = &config.ProxyAuthConfig{
			Provider:         "iap",
			IAP:              &config.IAPAuthConfig{Audience: "/projects/1/global/backendServices/2"},
			DriverPrincipals: list,
		}
		return cfg
	}

	for _, mode := range []string{"", "oauth"} {
		_, err := initWebServer(context.Background(), newCfg(mode, drivers), nil, "", false, "", nil, nil)
		require.Error(t, err, "mode %q", mode)
		assert.ErrorIs(t, err, config.ErrProxyDriverPrincipalsWithoutProxyAuth, "mode %q", mode)
	}

	_, err := initWebServer(context.Background(), newCfg("proxy", []string{"*@ti-test.iam.gserviceaccount.com"}), nil, "", false, "", nil, nil)
	assert.ErrorIs(t, err, config.ErrInvalidProxyDriverPrincipal)

	// A valid list in proxy mode, and no list outside it, are not refused
	// for driver reasons (initWebServer may still fail on other missing
	// dependencies here).
	for _, cfg := range []*config.GlobalConfig{newCfg("proxy", drivers), newCfg("proxy", nil), newCfg("", nil)} {
		_, err := initWebServer(context.Background(), cfg, nil, "", false, "", nil, nil)
		assert.NotErrorIs(t, err, config.ErrProxyDriverPrincipalsWithoutProxyAuth)
		assert.NotErrorIs(t, err, config.ErrInvalidProxyDriverPrincipal)
	}
}
