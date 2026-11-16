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

	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

// newRuleEngine constructs the single process-wide ruleengine.Engine for a Hub
// Server and registers the shared CELDialect with no consumer environments yet
// (Stage 1 §7.0). Consumer subsystems attach their CEL environments and
// non-CEL dialects in registerRuleConsumers.
func newRuleEngine() (*ruleengine.Engine, *ruleengine.CELDialect, error) {
	e := ruleengine.NewEngine()
	cel, err := ruleengine.NewCELDialect()
	if err != nil {
		return nil, nil, fmt.Errorf("create CEL dialect: %w", err)
	}
	if err := e.Register(cel); err != nil {
		return nil, nil, fmt.Errorf("register CEL dialect: %w", err)
	}
	return e, cel, nil
}

// registerRuleConsumers wires rule dialects and consumer environments onto
// s.rules right after newRuleEngine in hub.New. Registration functions perform
// registration only: no network I/O and no database or settings-store reads.
// Later consumer stages append one call here.
func registerRuleConsumers(s *Server) error {
	if err := s.registerSystemOneRules(); err != nil {
		return err
	}
	return nil
}

// RuleEngine returns the Hub Server's shared process-wide ruleengine.Engine.
func (s *Server) RuleEngine() *ruleengine.Engine {
	if s == nil {
		return nil
	}
	return s.rules
}

// CELDialect returns the Hub Server's shared ruleengine.CELDialect.
func (s *Server) CELDialect() *ruleengine.CELDialect {
	if s == nil {
		return nil
	}
	return s.celDialect
}
