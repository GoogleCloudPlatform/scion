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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
)

// registerSystemOneRules unconditionally registers the "system_one" dialect on
// s.rules during Hub construction (AC-13 option (b)).
//
// Registration performs no I/O and reads neither the database nor the
// operational settings store: MaxEvalTime is resolved from the already-loaded
// in-memory ServerConfig passed to hub.New, and the hub.system_one experiment
// gate is checked dynamically inside Dialect.Compile and Dialect.Evaluate
// before any I/O.
func (s *Server) registerSystemOneRules() error {
	if s.rules == nil {
		return fmt.Errorf("system_one: cannot register dialect before rule engine is initialized")
	}
	maxEvalTime := resolveSystemOneMaxEvalTime(s.config)
	dialect := systemone.NewDialect(systemone.DialectConfig{
		RuntimeFunc: s.SystemOneRuntime,
		MaxEvalTime: maxEvalTime,
		Enabled: func() bool {
			return s.experimentEnabled(experiments.SystemOne)
		},
	})
	if err := s.rules.Register(dialect); err != nil {
		return fmt.Errorf("register system_one dialect: %w", err)
	}
	return nil
}
