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

// Package writerguardproof is input for TestAgentWriterGuardProof: every
// function here mutates agents in a way the writer guard must report.
package writerguardproof

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/google/uuid"
)

func deleteThroughTx(ctx context.Context, tx *ent.Tx) error {
	_, err := tx.Agent.Delete().Where(agent.PhaseEQ("stopped")).Exec(ctx)
	return err
}

func deleteThroughParam(ctx context.Context, ac *ent.AgentClient, id uuid.UUID) error {
	return ac.DeleteOneID(id).Exec(ctx)
}

func rawSQL() string { return `UPDATE agents SET phase = 'x'` }

func entityUpdate(a *ent.Agent) *ent.AgentUpdateOne { return a.Update() }

func observedButSilent(ctx context.Context, c *ent.Client, id uuid.UUID) error {
	return c.Agent.DeleteOneID(id).Exec(ctx)
}

func shared(ctx context.Context, c *ent.Client, id uuid.UUID) error {
	return c.Agent.UpdateOneID(id).SetPhase("stopped").Exec(ctx)
}

func callerA(ctx context.Context, c *ent.Client, id uuid.UUID) error { return shared(ctx, c, id) }

func callerB(ctx context.Context, c *ent.Client, id uuid.UUID) error { return shared(ctx, c, id) }

// offPathWithoutLoad is classified observer_off by the proof test, but
// neither it nor its caller loads an observer slot.
func offPathWithoutLoad(ctx context.Context, c *ent.Client, id uuid.UUID) error {
	return c.Agent.UpdateOneID(id).SetPhase("running").Exec(ctx)
}

// setSingleWriterColumns sets the single-writer columns outside their
// writers; TestAgentColumnSetterGuardProof must report both calls.
func setSingleWriterColumns(u *ent.AgentUpdateOne) *ent.AgentUpdateOne {
	return u.SetEffectiveConfig("{}").ClearLaunchRunID()
}
