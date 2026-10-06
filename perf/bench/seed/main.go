// Command seed populates a fresh hub SQLite database with one project, a
// non-admin project-member principal, and N synthetic agents with a
// realistic mix of appliedConfig sizes, statuses, and ancestry.
//
// Refuses to run against an existing, non-empty --db by default:
// re-seeding the same file is not supported (it fails partway through with
// "already exists" errors after having already mutated hub bootstrap
// state), so this fails fast up front instead. The check normalizes --db
// the same way openSQLiteForBench does (stripping a "file:" prefix and
// "?query" suffix) before checking the filesystem, so a "file:" DSN form
// cannot bypass the check. There is no override flag -- every documented
// use of a hypothetical --force-existing would still fail partway through
// anyway, so it would only add a path to a known-broken outcome rather
// than a real capability. Use a fresh path.
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
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
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
// TestDeriveSharedSigningKeyMatchesHub in main_test.go pins the two
// together by minting a token with this function's output and validating
// it through hub.NewUserTokenService the same way ensureSigningKey does.
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

	dbPathClean, err := resolveDBPathArg(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(2)
	}

	if err := checkDBNotExists(dbPathClean); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(2)
	}

	// Quiet hub.New()'s bootstrap logging (role/group/limit reconciliation
	// info lines) -- this tool's own progress output is what matters here.
	slog.SetLogLoggerLevel(slog.LevelError)

	if err := run(dbPathClean, *agents, *secret, *projectSlug, *projectName, *randSeed, *outPath); err != nil {
		log.Fatalf("seed: %v", err)
	}
}

// normalizeDBPathForStat strips the "file:" prefix and any "?query" suffix
// openSQLiteForBench accepts, so a stat-based existence check inspects the
// same filesystem path the DSN actually resolves to.
//
// Calling os.Stat directly on a raw --db value would not work: since
// openSQLiteForBench accepts (and this tool's own callers may pass) a
// "file:" DSN with a "?cache=shared"-style suffix, e.g.
// "file:/tmp/hub.db?cache=shared", os.Stat on that literal string looks for
// a path starting with the 5 characters "file:" and containing a literal
// "?", which essentially never exists -- so an unnormalized check would
// always pass, regardless of whether the real underlying file existed and
// had data.
func normalizeDBPathForStat(dbPath string) string {
	p := strings.TrimPrefix(dbPath, "file:")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p
}

// validateDBPathArg rejects any --db value this tool did not expect a
// caller to supply: a "file:" DSN prefix, a leading "//", or any character
// with special meaning to the URI parsing `openSQLiteForBench`'s DSN
// eventually goes through ("?", "#", "%"), none of which this tool needs
// the caller to supply.
//
// This guards against several ways a --db value can resolve to a
// DIFFERENT filesystem path than the one `checkDBNotExists` stats, letting
// an existing, non-empty database slip past the existence guard:
//
//   - normalizeDBPathForStat's bare `TrimPrefix(dbPath, "file:")` handles
//     "file:/abs/path" and "file:/abs/path?cache=shared", but NOT the
//     "file://localhost/<path>" URI form (a valid, if unusual, sqlite3 DSN
//     authority): trimming only the "file:" prefix leaves
//     "//localhost/<path>", which os.Stat never finds on this host.
//     Rejecting any "file:"-prefixed --db value outright closes this.
//   - `openSQLiteForBench` builds `"file:" + dbPath + "?cache=shared"` by
//     string concatenation (to exactly match
//     `cmd/server_foreground.go`'s production DSN construction --
//     deliberately NOT changed here, since the whole point of this
//     function is to agree with the real `scion server start` subprocess
//     on how a path is opened) and hands the result to Go's
//     `net/url`-based sqlite DSN parser, which gives "#" and "%" their own
//     URI meaning: a trailing `#frag` is parsed as a fragment and silently
//     dropped from the path, and `%XX` sequences are percent-decoded. Both
//     let `--db victim.db%2e` (or similar) resolve to a path different
//     from the literal string `checkDBNotExists` confirmed doesn't exist.
//     Rejecting "#" and "%" outright, like "?", closes both.
//   - `--db //localhost/<path-to-an-existing-db>` has none of
//     "file:"/"?"/"#"/"%", but is the same bypass class without the
//     "file:" prefix: os.Stat never finds a literal "//localhost/<path>"
//     (reports "does not exist"), while the "file:" DSN's URI parsing
//     treats "localhost" as an empty host and resolves to the real,
//     possibly EXISTING, "<path>". Rejecting a leading "//" closes this.
//
// This is not an exhaustive enumeration of every possible DSN-vs-filesystem
// disagreement; `resolveDBPathArg` below additionally canonicalizes the
// path with `filepath.Clean` before using it for anything, which closes
// the slash-count class on its own, independent of this character
// blocklist. Rejecting a leading "//" here as well costs nothing and makes
// the intent explicit at the validation layer, not just the
// canonicalization layer.
func validateDBPathArg(dbPath string) error {
	if strings.HasPrefix(dbPath, "file:") {
		return fmt.Errorf(
			"--db %q must be a plain filesystem path, not a \"file:\" DSN -- this tool adds the "+
				"\"file:\" prefix itself (see openSQLiteForBench)", dbPath)
	}
	if strings.HasPrefix(dbPath, "//") {
		return fmt.Errorf(
			"--db %q must not start with \"//\" -- a leading double slash can be reinterpreted "+
				"as a URI authority (e.g. \"//localhost/...\") by the DSN parsing this tool's "+
				"sqlite DSN goes through (openSQLiteForBench), resolving to a DIFFERENT "+
				"filesystem path than a plain existence check sees; use a single-slash absolute "+
				"path or a relative path", dbPath)
	}
	for _, special := range []string{"?", "#", "%"} {
		if strings.Contains(dbPath, special) {
			return fmt.Errorf(
				"--db %q must not contain %q -- it has special meaning to the URI parsing this "+
					"tool's sqlite DSN goes through (openSQLiteForBench), and this tool adds "+
					"\"?cache=shared\" itself; use a path without it", dbPath, special)
		}
	}
	return nil
}

// resolveDBPathArg validates raw (see validateDBPathArg) and, if valid,
// returns the canonical path main() must use for BOTH the existence check
// and the actual DSN open (run -> openSQLiteForBench) -- the same value
// passed to both, never two independently-computed ones.
//
// This is extracted into its own function specifically so main() and its
// test call the exact same code path for canonicalization: a test that
// instead called `filepath.Clean` and `checkDBNotExists` directly could
// not tell the difference between "main() applies this canonicalization"
// and "main() does not", since it would not exercise main()'s own call
// sequence. Routing both through resolveDBPathArg means removing the
// canonicalization step from main() breaks the test too.
//
// filepath.Clean collapses any number of consecutive slashes ANYWHERE in
// the path to exactly one, so the existence check and the DSN parser agree
// for slash-count variants specifically: a leading "//" can otherwise be
// reinterpreted as a URI authority by the DSN parsing this tool's sqlite
// DSN goes through, resolving to a DIFFERENT filesystem path than a plain
// existence check sees. It does not, on its own, guarantee agreement for
// every possible DSN-vs-filesystem disagreement -- the other URI-special
// characters ("?", "#", "%", a "file:" prefix) are validateDBPathArg's job,
// above, not Clean's. For every ordinary single-slash or relative path
// (the only kind this tool's docs ever recommended), Clean is a no-op, so
// normal usage and `openSQLiteForBench`'s deliberately-unchanged
// production-matching construction are unaffected.
func resolveDBPathArg(raw string) (string, error) {
	if err := validateDBPathArg(raw); err != nil {
		return "", err
	}
	return filepath.Clean(raw), nil
}

// checkDBNotExists refuses an already-existing, non-empty dbPath.
// Extracted from main() so it is directly unit-testable: re-seeding an
// existing file is not supported (run() fails partway through, after
// already mutating hub bootstrap state, with an "already exists" error on
// the owner/member user), so this fails fast before opening the database
// at all. No override flag: a hypothetical --force-existing would still
// end in that same failure for every documented use, so it would only
// offer a path to a known-broken outcome rather than a real capability.
func checkDBNotExists(dbPath string) error {
	statPath := normalizeDBPathForStat(dbPath)
	fi, statErr := os.Stat(statPath)
	if statErr != nil {
		return nil // does not exist (or unreadable, which run() will fail on anyway) -- fine
	}
	if fi.Size() == 0 {
		return nil // e.g. a freshly `touch`ed placeholder; nothing to lose
	}
	return fmt.Errorf(
		"--db %s (resolved path %s) already exists and is non-empty (%d bytes). Re-seeding "+
			"an existing file is not supported: it will fail partway through (after already "+
			"mutating hub bootstrap state) with an \"already exists\" error on the owner/member "+
			"user. Use a fresh path or remove the file first", dbPath, statPath, fi.Size())
}

// openSQLiteForBench opens dbPath the same way cmd/server_foreground.go's
// production sqlite DSN construction does, so this tool and the real
// `scion server start --db <path>` subprocess agree on how the path is
// opened. Extracted so main_test.go can re-open a seeded file the same way.
func openSQLiteForBench(dbPath string) (*ent.Client, error) {
	sqliteDSN := dbPath
	if !strings.HasPrefix(sqliteDSN, "file:") {
		sqliteDSN = "file:" + sqliteDSN
	}
	if !strings.Contains(sqliteDSN, "?") {
		sqliteDSN += "?cache=shared"
	} else if !strings.Contains(sqliteDSN, "cache=") {
		sqliteDSN += "&cache=shared"
	}
	return entc.OpenSQLite(sqliteDSN, entc.PoolConfig{MaxOpenConns: 1})
}

func run(dbPath string, agentCount int, secret, projectSlug, projectName string, randSeed int64, outPath string) (err error) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(randSeed))

	client, err := openSQLiteForBench(dbPath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	s := entadapter.NewCompositeStore(client)
	// Close on every return path, not just the success path: Migrate,
	// hub.New, or any later step below can fail and return early, and an
	// un-closed store would otherwise leak the sqlite file handle.
	defer func() {
		if cerr := s.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close store: %w", cerr)
		}
	}()
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
