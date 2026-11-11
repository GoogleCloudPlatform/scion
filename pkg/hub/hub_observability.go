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
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubexport"
)

// hubObservabilitySection is the opsettings section of the hub's own
// metrics and trace export switches (server.hub.observability). Both keys
// are restart_required: the exporters are created once at startup, from
// the snapshot startup reads (hubexport).
const hubObservabilitySection = "hub_observability"

// SetObservabilityExportStatus records the startup state of the hub's own
// metrics and trace exporters, for the admin settings view.
func (s *Server) SetObservabilityExportStatus(st hubexport.Status) {
	s.observabilityExport.Store(&st)
}

// ObservabilityExportStatus returns the startup state of the hub's own
// metrics and trace exporters, or nil when startup has not recorded it
// (for example a hub built without the server command).
func (s *Server) ObservabilityExportStatus() *hubexport.Status {
	st := s.observabilityExport.Load()
	if st == nil {
		return nil
	}
	cp := *st
	return &cp
}
