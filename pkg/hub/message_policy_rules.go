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

	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
)

// registerMessagePolicyRules attaches the "message_policy" v1 CEL environment
// to the Hub Server's shared CELDialect, grants the "message_policy" consumer
// pure CEL plus I/O-bearing System One admission, and registers the
// "message_policy" v1 System One ContentEnvironment on the shared System One
// dialect (ptone/scion#8048).
//
// Registration performs no I/O and reads neither the database nor the
// operational settings store. Any error fails Hub startup closed.
func (s *Server) registerMessagePolicyRules() error {
	if s == nil || s.rules == nil || s.celDialect == nil {
		return fmt.Errorf("message_policy: cannot register rules before rule engine and CEL dialect are initialized")
	}
	celEng, err := messagepolicy.NewCELEngine()
	if err != nil {
		return fmt.Errorf("build message_policy CEL environment: %w", err)
	}
	if err := s.celDialect.RegisterEnvironment(celEng); err != nil {
		return fmt.Errorf("register message_policy CEL environment: %w", err)
	}
	chainCELEng, err := messagepolicy.NewChainCELEngine()
	if err != nil {
		return fmt.Errorf("build message_policy_chain CEL environment: %w", err)
	}
	if err := s.celDialect.RegisterEnvironment(chainCELEng); err != nil {
		return fmt.Errorf("register message_policy_chain CEL environment: %w", err)
	}
	if err := s.rules.AllowConsumerDialects(
		messagepolicy.ConsumerName,
		ruleengine.Grant(ruleengine.DialectCEL),
		ruleengine.GrantIO(systemone.DialectName),
	); err != nil {
		return fmt.Errorf("allow message_policy rule dialects: %w", err)
	}
	if err := systemone.RegisterConsumerEnvironment(
		s.rules,
		messagepolicy.ConsumerName,
		messagepolicy.SystemOneContentEnv(),
	); err != nil {
		return fmt.Errorf("register message_policy system_one content environment: %w", err)
	}
	return nil
}
