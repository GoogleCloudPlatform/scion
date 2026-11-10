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

// Command delivery-census reports, READ-ONLY, which live agents on a hub
// database the launch material delivery admission would refuse at start or
// restart, which of those the fallback still serves (legacy_* cohorts: no
// edge, sentinel edge, unadopted edge, adopted; see
// pkg/hub/material_delivery_legacy.go), and which stay
// withheld (deleted or held parent, held agent, duplicate edges, recorded
// ceiling, ...), with counts by cohort and by deny cause with and without
// the fallback. It runs hub.(*AuthzService).LaunchDeliveryCensus, the same
// admission and fallback a start runs, and reads no secret value.
// For agents created from a user access token it also reports the stored
// role against the role their chain allows, and how many of their owner's
// progeny-shared user secrets the start-time check admits (counts only, no
// names).
//
// It also lists every agent whose owner's user-scope rows not shared with
// progeny are withheld at start because the agent's delegation chain is
// not recorded to its owner, with the reason, the agent's depth, the names
// of those rows and the remedy. Its runtime read section lists every agent
// whose runtime user-scope reads are refused because its chain is not
// recorded, with the chain's shape, the names of the owner's shared
// secrets it is refused and the remedy. Env var rows are listed through
// the store, which returns their values; only the keys are kept and
// printed. The output names secret rows, so it is operator-only: keep it
// out of issues, chats and other shared places. Not part of the product.
//
// The database is opened read-only: sqlite with mode=ro and query_only(1)
// (entc.OpenSQLiteReadOnly), postgres with default_transaction_read_only=on
// (entc.OpenPostgresReadOnly). A write attempt fails instead of writing.
//
//	go run ./hack/delivery-census -driver sqlite -db ~/.scion/hub.db [-dev-auth] [-json]
//	go run ./hack/delivery-census -driver postgres -db "$DATABASE_URL" [-json]
//
// Pass -dev-auth when the hub runs with dev auth (the workstation server),
// because the admission refuses local-development provenance without it.
// Output: a summary (counts by cohort and cause) on stdout, then one TSV row
// per agent not served by the strict admission (fallback and withheld) and
// per agent created from a user access token; -all adds the other agents
// that deliver; -json prints the
// whole report as JSON instead.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

func main() {
	driver := flag.String("driver", "sqlite", "database driver: sqlite or postgres")
	db := flag.String("db", "", "sqlite file path or DSN, or postgres URL")
	devAuth := flag.Bool("dev-auth", false, "the hub runs with dev auth (local-development provenance is usable)")
	asJSON := flag.Bool("json", false, "print the whole report as JSON")
	all := flag.Bool("all", false, "also list agents that deliver")
	flag.Parse()
	if *db == "" {
		fmt.Fprintln(os.Stderr, "delivery-census: -db is required")
		os.Exit(2)
	}
	if err := run(context.Background(), os.Stdout, *driver, *db, *devAuth, *asJSON, *all); err != nil {
		fmt.Fprintln(os.Stderr, "delivery-census:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, w io.Writer, driver, db string, devAuth, asJSON, all bool) error {
	st, err := openReadOnly(driver, db)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// Admission logging goes to stderr so stdout stays the report.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rep, err := hub.NewAuthzService(st, logger).LaunchDeliveryCensus(ctx, devAuth)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return writeText(w, rep, devAuth, all)
}

// openReadOnly opens the hub database read-only.
func openReadOnly(driver, db string) (store.Store, error) {
	switch driver {
	case "sqlite", "":
		dsn := db
		if !strings.HasPrefix(dsn, "file:") {
			dsn = "file:" + dsn
		}
		dsn = withQueryParam(dsn, "mode", "ro")
		ec, err := entc.OpenSQLiteReadOnly(dsn)
		if err != nil {
			return nil, fmt.Errorf("open sqlite read-only: %w", err)
		}
		return entadapter.NewCompositeStore(ec), nil
	case "postgres":
		ec, err := entc.OpenPostgresReadOnly(db, entc.PoolConfig{})
		if err != nil {
			return nil, fmt.Errorf("open postgres read-only: %w", err)
		}
		return entadapter.NewCompositeStore(ec), nil
	default:
		return nil, fmt.Errorf("unknown driver %q (sqlite or postgres)", driver)
	}
}

// withQueryParam sets key=value in a DSN's query, dropping any existing key
// so the DSN never carries two conflicting values.
func withQueryParam(dsn, key, value string) string {
	base, query, _ := strings.Cut(dsn, "?")
	var kept []string
	for _, p := range strings.Split(query, "&") {
		if p == "" || p == key || strings.HasPrefix(p, key+"=") {
			continue
		}
		kept = append(kept, p)
	}
	kept = append(kept, key+"="+value)
	return base + "?" + strings.Join(kept, "&")
}

func writeText(w io.Writer, rep *hub.LaunchDeliveryCensusReport, devAuth, all bool) error {
	o := &textOut{w: w}
	o.printf("# launch delivery census (read-only), dev_auth=%t\n", devAuth)
	o.printf("agents\t%d\nwithheld\t%d\nlegacy_fallback\t%d\nstrict_withheld\t%d\n",
		rep.Agents, rep.Withheld, rep.Fallback, rep.StrictWithheld)
	o.printf("%s\n", "# agents created from a user access token (intrinsic set)")
	o.printf("uat_created\t%d\nuat_role_filtered\t%d\nuat_owner_shared_rows\t%d\nuat_owner_shared_admitted\t%d\nuat_owner_shared_intrinsic\t%d\nuat_root_not_admitted\t%d\nscheduler_uat_initiated\t%d\n",
		rep.UATCreated, rep.UATRoleFiltered, rep.UATOwnerSharedRows, rep.UATOwnerSharedAdmitted, rep.UATOwnerSharedIntrinsic,
		rep.UATRootNotAdmitted, rep.SchedulerUATInitiated)
	o.printf("%s\n", "# by cohort")
	for _, k := range sortedKeys(rep.ByCohort) {
		o.printf("cohort\t%s\t%d\n", k, rep.ByCohort[k])
	}
	o.printf("%s\n", "# by permission:cause (refusals)")
	for _, k := range sortedKeys(rep.ByCause) {
		o.printf("cause\t%s\t%d\n", k, rep.ByCause[k])
	}
	o.printf("%s\n", "# by permission:cause without the legacy fallback")
	for _, k := range sortedKeys(rep.ByStrictCause) {
		o.printf("strict_cause\t%s\t%d\n", k, rep.ByStrictCause[k])
	}
	o.printf("%s\n", "# rows")
	cols := []string{"agent_id", "project_id", "owner_id", "phase", "cohort", "strict_cohort", "edges", "delegator", "role", "prov_version", "source_kind", "ceiling_kind",
		"initiator_kind", "uat_stored_role", "uat_filtered_role", "uat_owner_shared", "uat_owner_admitted", "uat_owner_intrinsic"}
	cols = append(cols, hub.LaunchDeliveryCensusPermissions...)
	cols = append(cols, "first_reason")
	o.printf("%s\n", strings.Join(cols, "\t"))
	for _, r := range rep.Rows {
		if r.Cohort == "delivers" && r.UAT == nil && !all {
			continue
		}
		e := r.Edge
		delegator := ""
		if e.DelegatorType != "" {
			delegator = e.DelegatorType + ":" + e.DelegatorID
		}
		f := []string{r.AgentID, r.ProjectID, r.OwnerID, r.Phase, r.Cohort, r.StrictCohort, fmt.Sprint(e.ActiveEdges), delegator, e.Role,
			fmt.Sprint(e.ProvenanceVersion), e.SourceCredentialKind, e.CeilingKind, e.InitiatorCredentialKind}
		if u := r.UAT; u != nil {
			f = append(f, u.StoredRole, u.FilteredRole, fmt.Sprint(u.OwnerSharedRows), fmt.Sprint(u.OwnerSharedAdmitted), fmt.Sprint(u.OwnerSharedIntrinsic))
		} else {
			f = append(f, "-", "-", "-", "-", "-")
		}
		reason := r.MintError
		for _, p := range hub.LaunchDeliveryCensusPermissions {
			v, ok := r.Verdicts[p]
			switch {
			case !ok:
				f = append(f, "-")
			case v.Allowed && v.LegacyCohort != "":
				f = append(f, "legacy:"+v.LegacyCohort)
			case v.Allowed:
				f = append(f, "allow")
			default:
				f = append(f, "deny:"+string(v.Cause))
				if reason == "" {
					reason = v.Reason
				}
			}
		}
		f = append(f, reason)
		o.printf("%s\n", strings.Join(f, "\t"))
	}
	writeOwnerMaterialChain(o, rep.OwnerMaterialChain)
	writeRuntimeRead(o, rep.RuntimeRead)
	return o.err
}

// writeRuntimeRead prints the runtime read section: the counts by cohort
// and by shape, and one row per agent whose chain is not recorded (secret
// names only).
func writeRuntimeRead(o *textOut, rr hub.RuntimeReadCensus) {
	o.printf("%s\n", "# runtime read")
	for _, k := range sortedKeys(rr.ByCohort) {
		o.printf("cohort\t%s\t%d\n", k, rr.ByCohort[k])
	}
	for _, k := range sortedKeys(rr.ByShape) {
		o.printf("shape\t%s\t%d\n", k, rr.ByShape[k])
	}
	o.printf("%s\n", strings.Join([]string{"agent_id", "project_id", "owner_id", "cohort", "shape", "depth", "ancestor_agent_id",
		"root_user_id", "shared_secrets", "secrets_error", "remedy"}, "\t"))
	for _, r := range rr.Rows {
		o.printf("%s\n", strings.Join([]string{r.AgentID, r.ProjectID, r.OwnerID, r.Cohort, r.Shape, strconv.Itoa(r.Depth),
			r.AncestorAgentID, r.RootUserID, strings.Join(r.SharedSecrets, ","), r.SecretsError, r.Remedy}, "\t"))
	}
}

// writeOwnerMaterialChain prints the owner material chain section: the
// counts, the remedy, and one row per listed agent (row names only),
// including the owner's shared secrets that start also withholds.
func writeOwnerMaterialChain(o *textOut, omc hub.OwnerMaterialChainCensus) {
	o.printf("%s\n", "# owner material chain")
	o.printf("withheld\t%d\naffected\t%d\n", omc.Withheld, omc.Affected)
	for _, k := range sortedKeys(omc.ByReason) {
		o.printf("reason\t%s\t%d\n", k, omc.ByReason[k])
	}
	o.printf("remedy\t%s\n", omc.Remedy)
	o.printf("%s\n", strings.Join([]string{"agent_id", "project_id", "owner_id", "reason", "depth", "secrets", "env_vars", "rows_error", "shared_secrets"}, "\t"))
	for _, r := range omc.Rows {
		o.printf("%s\n", strings.Join([]string{r.AgentID, r.ProjectID, r.OwnerID, r.Reason, strconv.Itoa(r.Depth),
			strings.Join(r.Secrets, ","), strings.Join(r.EnvVars, ","), r.RowsError, strings.Join(r.SharedSecrets, ",")}, "\t"))
	}
}

// textOut writes formatted lines and keeps the first write error.
type textOut struct {
	w   io.Writer
	err error
}

func (o *textOut) printf(format string, a ...any) {
	if o.err == nil {
		_, o.err = fmt.Fprintf(o.w, format, a...)
	}
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
