// Package mutates is input for TestFindingsReportsAgentMutations.
package mutates

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/google/uuid"
)

func viaClient(ctx context.Context, c *ent.Client, id uuid.UUID) error {
	return c.Agent.UpdateOneID(id).SetPhase("x").Exec(ctx)
}

func viaParam(ctx context.Context, ac *ent.AgentClient, id uuid.UUID) error {
	return ac.DeleteOneID(id).Exec(ctx)
}

func viaEntity(a *ent.Agent) *ent.AgentUpdateOne { return a.Update() }

func readOnly(ctx context.Context, c *ent.Client) (int, error) { return c.Agent.Query().Count(ctx) }
