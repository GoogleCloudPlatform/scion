// Package clean is input for TestFindingsReportsAgentMutations: it uses
// pkg/ent without mutating agents.
package clean

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/google/uuid"
)

func read(ctx context.Context, c *ent.Client, id uuid.UUID) (*ent.Agent, error) {
	return c.Agent.Get(ctx, id)
}

func otherEntity(ctx context.Context, c *ent.Client, id uuid.UUID) error {
	return c.Project.DeleteOneID(id).Exec(ctx)
}
