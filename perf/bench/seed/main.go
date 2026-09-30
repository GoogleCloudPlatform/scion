// Command seed populates a fresh (or existing) hub SQLite database with one
// project, a non-admin project-member principal, and N synthetic agents with
// a realistic mix of appliedConfig sizes, statuses, and ancestry.
//
// It is the first stage of the perf/2393-large-project-bench harness
// (ptone/scion#2393, #2374, #2367): every other bench tool consumes the
// metadata JSON this writes rather than re-deriving project/credential state.
//
// Usage:
//
//	go run ./perf/bench/seed \
//	  --db /tmp/scion-bench/hub.db \
//	  --session-secret bench-secret-1 \
//	  --agents 100 \
//	  --project-slug bench-100 \
//	  --out /tmp/scion-bench/seed-100.json
//
// The same --session-secret value must then be passed to the hub subprocess
// started against --db (e.g. `scion server start --db ... --session-secret
// bench-secret-1`): the member/owner bearer tokens minted here are signed
// with a key derived deterministically from that secret (see
// deriveSharedSigningKey below), so only a hub started with the same secret
// will accept them. See pkg/hub/server.go's ensureSigningKey /
// deriveSharedSigningKey for the production side of this derivation.
//
// Seeding is direct-to-store (bypassing HTTP) for speed at N=500+, following
// the pattern already used by pkg/hub's own SQLite-backed tests
// (pkg/hub/teststore_test.go, pkg/store/storetest/domains.go). One known
// consequence of that shortcut, called out in pkg/hub/handlers_test.go: it
// does not create delegation-edge rows the way the real agent-create HTTP
// path does. That is fine for the agent-list/graph endpoints this harness
// measures (ptone/scion#2392, #2393), which do not read delegation edges;
// it would matter for a benchmark of delegation-ceiling checks specifically.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"

	"github.com/GoogleCloudPlatform/scion/perf/bench/internal/benchout"
)

// deriveSharedSigningKey duplicates the unexported function of the same name
// in pkg/hub/server.go byte-for-byte. It is a tiny, stable primitive (one
// sha256 sum over a fixed format string); duplicating it here avoids
// exporting a new piece of hub public API purely for a benchmark tool's
// benefit. If server.go's format ever changes this must change with it --
// there is a parity check for that in perf/bench/seed/main_test.go.
func deriveSharedSigningKey(secret, keyName string) []byte {
	sum := sha256.Sum256([]byte("scion-hub-signing-key:" + keyName + ":" + secret))
	return sum[:]
}

func main() {
	dbPath := flag.String("db", "", "path to the sqlite db file to create/seed (required)")
	agents := flag.Int("agents", 100, "number of agents to seed into the project")
	secret := flag.String("session-secret", "", "shared signing secret; must match --session-secret given to the hub subprocess (required)")
	projectSlug := flag.String("project-slug", "bench-project", "project slug to create")
	projectName := flag.String("project-name", "Large Project Bench", "project display name")
	randSeed := flag.Int64("rand-seed", 42, "seed for deterministic synthetic data generation")
	outPath := flag.String("out", "", "path to write seed metadata JSON (required)")
	flag.Parse()

	if *dbPath == "" || *secret == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: seed --db <path> --session-secret <secret> --out <metadata.json> [--agents N] [--project-slug slug] [--project-name name] [--rand-seed N]")
		os.Exit(2)
	}
	if *agents < 0 {
		fmt.Fprintln(os.Stderr, "--agents must be >= 0")
		os.Exit(2)
	}

	// Quiet hub.New()'s bootstrap logging (role/group/limit reconciliation
	// info lines) -- this tool's own progress output is what matters here.
	slog.SetLogLoggerLevel(slog.LevelError)

	if err := run(*dbPath, *agents, *secret, *projectSlug, *projectName, *randSeed, *outPath); err != nil {
		log.Fatalf("seed: %v", err)
	}
}

func run(dbPath string, agentCount int, secret, projectSlug, projectName string, randSeed int64, outPath string) error {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(randSeed))

	// Matches cmd/server_foreground.go's sqlite DSN construction exactly, so
	// this tool and the real `scion server start --db <path>` subprocess
	// agree on how the path is opened.
	sqliteDSN := dbPath
	if !strings.HasPrefix(sqliteDSN, "file:") {
		sqliteDSN = "file:" + sqliteDSN
	}
	if !strings.Contains(sqliteDSN, "?") {
		sqliteDSN += "?cache=shared"
	} else if !strings.Contains(sqliteDSN, "cache=") {
		sqliteDSN += "&cache=shared"
	}

	client, err := entc.OpenSQLite(sqliteDSN, entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	s := entadapter.NewCompositeStore(client)
	if err := s.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// Trigger the same built-in role/group/limit bootstrap a real hub
	// startup performs (reconcileBuiltInRoles, seedDefaultGroupsAndBindings,
	// seedLimitDefinitions, signing key derivation) by transiently
	// constructing a Server against this store exactly the way `scion server
	// start` does. No listener is started; nothing here is served over the
	// network. Reusing hub.New() -- rather than reimplementing this
	// bootstrap -- is what keeps this tool from drifting out of sync with
	// the seeding logic it depends on existing (role definitions in
	// particular: GetRoleDefinitionByName below fails without it).
	cfg := hub.DefaultServerConfig()
	cfg.SharedSigningSecret = secret
	if _, err := hub.New(cfg, s); err != nil {
		return fmt.Errorf("hub.New (bootstrap seed): %w", err)
	}

	owner, err := createUser(ctx, s, "bench-owner@example.test", "Bench Owner")
	if err != nil {
		return fmt.Errorf("create owner user: %w", err)
	}
	member, err := createUser(ctx, s, "bench-member@example.test", "Bench Member")
	if err != nil {
		return fmt.Errorf("create member user: %w", err)
	}
	for _, u := range []*store.User{owner, member} {
		if err := addToHubMembers(ctx, s, u.ID); err != nil {
			return fmt.Errorf("add %s to hub-members group: %w", u.Email, err)
		}
	}

	project := &store.Project{
		ID:        uuid.NewString(),
		Name:      projectName,
		Slug:      projectSlug,
		CreatedBy: owner.ID,
		OwnerID:   owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		return fmt.Errorf("create project: %w", err)
	}

	if err := bindProjectRole(ctx, s, owner.ID, project.ID, store.ProjectRoleOwner); err != nil {
		return fmt.Errorf("bind owner role: %w", err)
	}
	// The requester every bench tool authenticates as: an ordinary
	// project-member, not the owner and not a hub admin. See package doc /
	// SeedMetadata.MemberToken.
	if err := bindProjectRole(ctx, s, member.ID, project.ID, store.ProjectRoleMember); err != nil {
		return fmt.Errorf("bind member role: %w", err)
	}

	phaseCounts := map[string]int{}
	activityCounts := map[string]int{}
	ancestryCounts := map[string]int{"none": 0, "single": 0, "chain": 0}
	sizeCounts := map[string]int{"small": 0, "large": 0}

	var priorAgentIDs []string
	for i := 0; i < agentCount; i++ {
		agent, ancestryKind, sizeKind := syntheticAgent(rng, project.ID, owner.ID, member.ID, i, priorAgentIDs)
		if err := s.CreateAgent(ctx, agent); err != nil {
			return fmt.Errorf("create agent %d: %w", i, err)
		}
		phaseCounts[agent.Phase]++
		if agent.Activity != "" {
			activityCounts[agent.Activity]++
		}
		ancestryCounts[ancestryKind]++
		sizeCounts[sizeKind]++
		// Every 7th agent becomes eligible as a "parent" for later
		// multi-level ancestry chains, so chains reference real prior agent
		// IDs rather than random UUIDs.
		if i%7 == 0 {
			priorAgentIDs = append(priorAgentIDs, agent.ID)
		}
	}

	if err := s.Close(); err != nil {
		return fmt.Errorf("close store: %w", err)
	}

	tokenSvc, err := hub.NewUserTokenService(hub.UserTokenConfig{
		SigningKey: deriveSharedSigningKey(secret, hub.SecretKeyUserSigningKey),
	})
	if err != nil {
		return fmt.Errorf("build user token service: %w", err)
	}
	ownerToken, _, err := tokenSvc.GenerateAccessToken(owner.ID, owner.Email, owner.DisplayName, owner.Role, hub.ClientTypeCLI)
	if err != nil {
		return fmt.Errorf("mint owner token: %w", err)
	}
	memberToken, _, err := tokenSvc.GenerateAccessToken(member.ID, member.Email, member.DisplayName, member.Role, hub.ClientTypeCLI)
	if err != nil {
		return fmt.Errorf("mint member token: %w", err)
	}

	meta := benchout.SeedMetadata{
		DBPath:                  dbPath,
		SessionSecret:           secret,
		ProjectID:               project.ID,
		ProjectSlug:             project.Slug,
		OwnerUserID:             owner.ID,
		OwnerEmail:              owner.Email,
		OwnerToken:              ownerToken,
		MemberUserID:            member.ID,
		MemberEmail:             member.Email,
		MemberToken:             memberToken,
		AgentCount:              agentCount,
		PhaseCounts:             phaseCounts,
		ActivityCounts:          activityCounts,
		AncestryCounts:          ancestryCounts,
		AppliedConfigSizeCounts: sizeCounts,
		RandSeed:                randSeed,
		SeededAt:                time.Now().UTC(),
	}
	if err := writeJSONFile(outPath, meta); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	fmt.Printf("seeded %d agents into project %q (%s); metadata written to %s\n", agentCount, project.Slug, project.ID, outPath)
	return nil
}

func createUser(ctx context.Context, s store.Store, email, displayName string) (*store.User, error) {
	u := &store.User{
		ID:          uuid.NewString(),
		Email:       email,
		DisplayName: displayName,
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// addToHubMembers mirrors pkg/hub/seed.go's unexported ensureHubMembershipTx
// using only exported store methods, since a bench tool living outside
// package hub cannot call it directly.
func addToHubMembers(ctx context.Context, s store.Store, userID string) error {
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	if err != nil {
		return fmt.Errorf("hub-members group lookup: %w", err)
	}
	err = s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    group.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       store.GroupMemberRoleMember,
		AddedAt:    time.Now(),
		AddedBy:    "perf-bench-seed",
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	return nil
}

func bindProjectRole(ctx context.Context, s store.Store, userID, projectID, roleName string) error {
	role, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	if err != nil {
		return fmt.Errorf("lookup role %q: %w", roleName, err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               uuid.NewString(),
		RoleDefinitionID: role.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "perf-bench-seed",
		CreatedAt:        time.Now(),
	})
	return err
}
