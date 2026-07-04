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

package grpcbroker

import (
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
)

// NewAdapterFromEntry creates a GRPCBrokerClient from a PluginEntry.
// This is the factory function injected into plugin.Manager.NewGRPCBrokerAdapter
// to avoid import cycles.
func NewAdapterFromEntry(entry plugin.PluginEntry, logger *slog.Logger) (plugin.GRPCBrokerClient, error) {
	var tlsCfg *TLSConfig
	if entry.TLSCertFile != "" || entry.TLSCAFile != "" || entry.TLSSkipVerify {
		tlsCfg = &TLSConfig{
			CertFile:   entry.TLSCertFile,
			KeyFile:    entry.TLSKeyFile,
			CAFile:     entry.TLSCAFile,
			SkipVerify: entry.TLSSkipVerify,
		}
	}

	adapter := NewGRPCBrokerAdapter(AdapterConfig{
		Address: entry.Address,
		TLS:     tlsCfg,
		Logger:  logger,
	})

	return adapter, nil
}
