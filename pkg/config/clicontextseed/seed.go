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

// Package clicontextseed renders a minimal ~/.scion/context.yaml seed file
// for freshly started execution containers. It is a small leaf package shared
// by sciontool and pkg/config's schema-drift tests without pulling pkg/config
// into sciontool or cmd/sciontool into pkg/config tests.
package clicontextseed

import (
	"errors"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config/hubname"
	"gopkg.in/yaml.v3"
)

// Version matches config.CLIContextVersion (context.yaml v1).
const Version = 1

// TransportOptions configures the optional hub transport-layer auth block
// (matching config.HubTransportConfig: mode and audience). It holds no
// credentials; PID 1's transport token is never written to context.yaml.
type TransportOptions struct {
	Mode     string `yaml:"mode,omitempty"`
	Audience string `yaml:"audience,omitempty"`
}

// Options configures the rendered context.yaml seed.
type Options struct {
	Endpoint  string
	Transport *TransportOptions
}

type seedDocument struct {
	Version    int                      `yaml:"version"`
	CurrentHub string                   `yaml:"current_hub"`
	Hubs       map[string]*seedHubEntry `yaml:"hubs"`
}

type seedHubEntry struct {
	Endpoint  string            `yaml:"endpoint"`
	Transport *TransportOptions `yaml:"transport,omitempty"`
}

// Render renders the context.yaml bytes for opts using
// hubname.NormalizeEndpoint and hubname.DeriveHubName.
func Render(opts Options) ([]byte, error) {
	endpoint := hubname.NormalizeEndpoint(opts.Endpoint)
	if endpoint == "" {
		return nil, errors.New("empty hub endpoint")
	}
	entry := &seedHubEntry{Endpoint: endpoint}
	if opts.Transport != nil {
		mode := strings.TrimSpace(opts.Transport.Mode)
		audience := strings.TrimSpace(opts.Transport.Audience)
		if mode != "" && mode != "iap" && mode != "cloudrun_invoker" {
			return nil, fmt.Errorf("unsupported transport mode %q", mode)
		}
		if mode != "" || audience != "" {
			entry.Transport = &TransportOptions{
				Mode:     mode,
				Audience: audience,
			}
		}
	}
	nick := hubname.DeriveHubName(endpoint)
	doc := seedDocument{
		Version:    Version,
		CurrentHub: nick,
		Hubs: map[string]*seedHubEntry{
			nick: entry,
		},
	}
	data, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("marshal context.yaml seed: %w", err)
	}
	return data, nil
}
