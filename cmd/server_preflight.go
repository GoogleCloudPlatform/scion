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

package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/internal/writesonread"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/preflight"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// preflightDBEnv carries the hub's database DSN to an executor-invoked
// preflight. It is an environment variable, not a flag, because a Postgres
// DSN can hold a password and argv is visible to other users.
const preflightDBEnv = "SCION_PREFLIGHT_DB"

// preflightDeadline bounds the whole command, inside the rebuild
// executor's 120 s kill, so a slow open or connect ends with an error
// rather than a kill. Each check has its own, shorter timeout.
const preflightDeadline = 90 * time.Second

var (
	preflightColocatedBrokerID string
	preflightDBFlag            string
	preflightConfigPath        string
)

var serverPreflightCmd = &cobra.Command{
	Use:   "preflight",
	Short: "Check the hub's stored state against this binary, read only",
	Long: `Check, read only, whether the settings the hub would hold after a restart
on this binary still resolve for the agents and projects that use them. The
admin rebuild runs it with the newly built binary before installing it.

Exit status: 0 when no finding blocks, 3 when at least one does, 1 when the
preflight could not run (no report is printed then).

Database:
  With --colocated-broker-id (the rebuild executor always passes it, even
  empty), the DSN is read only from SCION_PREFLIGHT_DB, which is then
  required, and the driver from the server config. Without the flag, the DSN
  is --db, else the server config, else ~/.scion/hub.db, and
  SCION_PREFLIGHT_DB is ignored. Without the flag the profile check does not
  apply, so a run by hand only shows that the database opens.

The database is opened read only, in one snapshot transaction, and no
migration runs. The command writes no file. Put flags after
'server preflight'.`,
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runServerPreflight(cmd.Context(), preflightOptions{
			executor:   cmd.Flags().Changed("colocated-broker-id"),
			brokerID:   preflightColocatedBrokerID,
			dbFlag:     preflightDBFlag,
			configPath: preflightConfigPath,
			envDSN:     os.Getenv(preflightDBEnv),
			format:     outputFormat,
			stdout:     cmd.OutOrStdout(),
			stderr:     cmd.ErrOrStderr(),
		})
	},
}

func init() {
	serverCmd.AddCommand(serverPreflightCmd)
	f := serverPreflightCmd.Flags()
	f.StringVar(&preflightColocatedBrokerID, "colocated-broker-id", "", "The hub's legacy co-located Runtime Broker ID (may be empty); marks an executor-invoked run")
	f.StringVar(&preflightDBFlag, "db", "", "Database DSN for a run by hand (ignored with --colocated-broker-id)")
	f.StringVarP(&preflightConfigPath, "config", "c", "", "Path to the server configuration file the hub was started with")
}

// isNoWriteCommand reports whether cmd is "scion server preflight", which
// must not write: the root hook turns internal/writesonread on for it.
func isNoWriteCommand(cmd *cobra.Command) bool {
	if cmd == nil || cmd.Name() != "preflight" {
		return false
	}
	server := cmd.Parent()
	return server != nil && server.Name() == "server" && server.Parent() != nil && !server.Parent().HasParent()
}

// applyNoWriteGuard turns internal/writesonread on when cmd must not
// write. Execute calls it before any settings load, and the root hook at
// its top; every other command is unaffected.
func applyNoWriteGuard(cmd *cobra.Command) {
	if isNoWriteCommand(cmd) {
		writesonread.Set()
	}
}

// preflightOptions are the inputs of one preflight run.
type preflightOptions struct {
	executor   bool // --colocated-broker-id given
	brokerID   string
	dbFlag     string
	configPath string
	envDSN     string
	format     string
	stdout     io.Writer
	stderr     io.Writer
}

// runServerPreflight runs the preflight and prints its report.
func runServerPreflight(ctx context.Context, o preflightOptions) error {
	writesonread.Set()

	// The check logs through package-level slog. When the rebuild
	// executor runs this, stderr becomes the run log, so error text, which
	// can quote a settings value, is withheld there.
	prev := slog.Default()
	slog.SetDefault(slog.New(&withholdErrorsHandler{
		inner:    slog.NewTextHandler(o.stderr, nil),
		withhold: o.executor,
	}))
	defer slog.SetDefault(prev)

	if !o.executor && o.envDSN != "" {
		_, _ = fmt.Fprintln(o.stderr, preflightDBEnv+" is ignored without --colocated-broker-id")
	}

	ctx, cancel := context.WithTimeout(ctx, preflightDeadline)
	defer cancel()
	rep, err := preflightRun(ctx, o)
	if err != nil {
		return err
	}
	return writePreflightReport(o.stdout, rep, o.format)
}

// preflightRun opens the hub database read only, in one snapshot, and
// runs the registered checks.
func preflightRun(ctx context.Context, o preflightOptions) (preflight.Report, error) {
	if o.executor && o.envDSN == "" {
		return preflight.Report{}, fmt.Errorf("%s is required when --colocated-broker-id is given", preflightDBEnv)
	}
	cfg, err := config.LoadGlobalConfig(o.configPath)
	if err != nil {
		slog.Warn("preflight: the server config does not load", "error", err)
		return preflight.Report{}, errors.New("the server config does not load")
	}
	driver, dsn := resolvePreflightDSN(o.executor, o.envDSN, o.dbFlag, cfg)

	client, err := openPreflightClient(driver, dsn)
	if err != nil {
		return preflight.Report{}, fmt.Errorf("opening the %s hub database read only: %s", driver, redactDSNIn(err.Error(), driver, dsn))
	}
	defer func() { _ = client.Close() }()

	if err := entadapter.CheckSchemaShadowing(ctx, client, cfg.Database.AllowShadowedSchema, slog.Default()); err != nil {
		var shadow *entadapter.SchemaShadowError
		if errors.As(err, &shadow) {
			return preflight.Report{}, fmt.Errorf("the restarted hub would refuse to start: PostgreSQL schema %q, which has no hub tables, is ahead of schema %q on the search_path; fix the search_path or set server.database.allow_shadowed_schema",
				shadow.CurrentSchema, shadow.ShadowedSchema)
		}
		return preflight.Report{}, fmt.Errorf("checking the PostgreSQL search_path: %s", redactDSNIn(err.Error(), driver, dsn))
	}

	tx, err := client.BeginTx(ctx, snapshotTxOptions(client.Driver().Dialect()))
	if err != nil {
		return preflight.Report{}, fmt.Errorf("starting the read-only snapshot: %s", redactDSNIn(err.Error(), driver, dsn))
	}
	defer func() { _ = tx.Rollback() }() // read only: never committed

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return preflight.Report{}, errors.New("the hub's global directory cannot be resolved")
	}
	env := preflight.Env{
		GlobalDir:         globalDir,
		ServerConfigPath:  o.configPath,
		ColocatedBrokerID: o.brokerID,
		DB:                newPreflightReader(tx.Client(), o.brokerID), // every read inside the one snapshot
	}
	return preflight.NewDefaultRegistry().RunAll(ctx, env), nil
}

// resolvePreflightDSN returns the driver and DSN to open. Executor-invoked,
// they are SCION_PREFLIGHT_DB and the server config's driver (the hub's
// own choice; a DSN prefix cannot tell a keyword-form Postgres DSN from a
// path). By hand: --db (driver from its prefix), else the server config
// (which defaults to ~/.scion/hub.db).
func resolvePreflightDSN(executor bool, envDSN, dbFlag string, cfg *config.GlobalConfig) (driver, dsn string) {
	if executor {
		return cfg.Database.Driver, envDSN
	}
	maintenanceDBOverride(cfg, dbFlag)
	return cfg.Database.Driver, cfg.Database.URL
}

// openPreflightClient opens the hub database read only. A variable so a
// test can wrap the driver to observe the queries.
var openPreflightClient = func(driver, dsn string) (*ent.Client, error) {
	switch strings.ToLower(driver) {
	case "sqlite", "":
		return entc.OpenSQLiteReadOnly(preflightSQLiteDSN(dsn))
	case "postgres":
		return entc.OpenPostgresReadOnly(dsn, preflightPostgresPool())
	default:
		return nil, fmt.Errorf("unsupported database driver %q", driver)
	}
}

// preflightSQLiteDSN makes dsn a read-only SQLite URI: SQLite honours URI
// parameters only on a "file:" URI (modernc strips the query from any
// other DSN), so the prefix comes first; then any mode is replaced by
// mode=ro, so a missing database is an error and is never created; and a
// busy timeout lets the read wait out a writer's checkpoint.
func preflightSQLiteDSN(dsn string) string {
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}
	dsn = stripDSNQueryParam(dsn, "mode")
	dsn = appendDSNQueryParam(dsn, "mode=ro")
	dsn = appendDSNQueryParam(dsn, "_pragma=busy_timeout(5000)")
	return dsn
}

// appendDSNQueryParam appends one "key=value" query parameter to dsn.
func appendDSNQueryParam(dsn, param string) string {
	if strings.Contains(dsn, "?") {
		return dsn + "&" + param
	}
	return dsn + "?" + param
}

// preflightPostgresPool is the preflight's Postgres pool: at most two
// connections (the shadow-guard query and the snapshot).
func preflightPostgresPool() entc.PoolConfig {
	return entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 2}
}

// snapshotTxOptions are the options of the one read-only snapshot. On
// Postgres, REPEATABLE READ READ ONLY makes every read see one snapshot.
// On SQLite (nil: a plain BEGIN on the only connection), the first read
// takes one WAL snapshot, held until the transaction ends.
func snapshotTxOptions(d string) *sql.TxOptions {
	if d == dialect.Postgres {
		return &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	}
	return nil
}

// dsnPasswordRE finds a libpq keyword/value credential (password= or
// sslpassword=), quoted or not.
var dsnPasswordRE = regexp.MustCompile(`(?i)\b(?:ssl)?password\s*=\s*('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|\S+)`)

// dsnSecrets returns the credentials dsn carries: a postgres:// URL's
// userinfo password and password/sslpassword query parameters (also with
// an empty host, as in a Unix-socket URL such as
// "postgres://u:p@/db?host=/cloudsql/..."), or a keyword-form DSN's
// password values.
func dsnSecrets(dsn string) []string {
	var out []string
	if u, err := url.Parse(dsn); err == nil && (strings.EqualFold(u.Scheme, "postgres") || strings.EqualFold(u.Scheme, "postgresql")) {
		if pw, ok := u.User.Password(); ok {
			out = append(out, pw)
		}
		for k, vs := range u.Query() {
			if strings.EqualFold(k, "password") || strings.EqualFold(k, "sslpassword") {
				out = append(out, vs...)
			}
		}
		return out
	}
	for _, m := range dsnPasswordRE.FindAllStringSubmatch(dsn, -1) {
		v := m[1]
		if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
			v = strings.NewReplacer(`\'`, `'`, `\"`, `"`, `\\`, `\`).Replace(v[1 : len(v)-1])
		}
		out = append(out, v)
	}
	return out
}

// redactDSNIn returns text with dsn replaced by its redacted form and every
// credential it carries masked in every rendering (redactSecretForms).
func redactDSNIn(text, driver, dsn string) string {
	if dsn == "" {
		return text
	}
	text = strings.ReplaceAll(text, dsn, config.RedactDatabaseURL(driver, dsn))
	text = strings.ReplaceAll(text, quotedBody(strconv.Quote(dsn)), config.RedactDatabaseURL(driver, dsn))
	for _, s := range dsnSecrets(dsn) {
		text = redactSecretForms(text, s)
	}
	return text
}

// preflightExit is the error of a run whose report has blocking findings:
// exit status 3. With JSON output the report was already printed, so it
// unwraps to a jsonReportedError and Execute prints nothing more.
type preflightExit struct {
	code int
	msg  string
	json bool
}

func (e *preflightExit) Error() string { return e.msg }

// ExitCode implements exitCoder.
func (e *preflightExit) ExitCode() int { return e.code }

func (e *preflightExit) Unwrap() error {
	if e.json {
		return &jsonReportedError{msg: e.msg}
	}
	return nil
}

// writePreflightReport prints rep as JSON (--format json) or text, and
// returns a preflightExit with status 3 when a finding blocks.
func writePreflightReport(w io.Writer, rep preflight.Report, format string) error {
	var err error
	if format == "json" {
		err = writeJSONTo(w, rep)
	} else {
		err = writePreflightText(w, rep)
	}
	if err != nil {
		return fmt.Errorf("writing the preflight report: %w", err)
	}
	if rep.Blocking > 0 {
		return &preflightExit{code: 3, msg: fmt.Sprintf("preflight found %d blocking finding(s)", rep.Blocking), json: format == "json"}
	}
	return nil
}

// writePreflightText renders rep for a person.
func writePreflightText(w io.Writer, rep preflight.Report) error {
	info := 0
	for _, f := range rep.Findings {
		if f.Severity == preflight.SeverityInfo {
			info++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "preflight: %d blocking, %d warning(s), %d info (checks: %s)\n",
		rep.Blocking, rep.Warnings, info, strings.Join(rep.Checks, ", "))
	for _, f := range rep.Findings {
		fmt.Fprintf(&b, "[%s] %s: %s\n", strings.ToUpper(string(f.Severity)), f.Code, f.Message)
		if len(f.Subjects) > 0 {
			fmt.Fprintf(&b, "  subjects: %s", strings.Join(f.Subjects, ", "))
			if more := f.SubjectCount - len(f.Subjects); more > 0 {
				fmt.Fprintf(&b, " (+%d more)", more)
			}
			b.WriteString("\n")
		}
		if f.Remediation != "" {
			fmt.Fprintf(&b, "  fix: %s\n", f.Remediation)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// withheldNote replaces the text of an error in an executor-invoked run's
// log records.
const withheldNote = "text withheld; run 'scion server preflight' by hand to see it"

// withholdErrorsHandler is the preflight's slog handler. When withhold is
// set (an executor-invoked run, whose stderr becomes the persisted run
// log), every attribute whose value is an error, at any depth inside
// groups, is replaced by the error's type and withheldNote: a settings
// loader's error can quote the value it could not convert. It matches on
// the value's type, not the attribute's key.
type withholdErrorsHandler struct {
	inner    slog.Handler
	withhold bool
}

func (h *withholdErrorsHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *withholdErrorsHandler) Handle(ctx context.Context, r slog.Record) error {
	if !h.withhold {
		return h.inner.Handle(ctx, r)
	}
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(withholdErrorAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *withholdErrorsHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if h.withhold {
		mapped := make([]slog.Attr, 0, len(attrs))
		for _, a := range attrs {
			mapped = append(mapped, withholdErrorAttr(a))
		}
		attrs = mapped
	}
	return &withholdErrorsHandler{inner: h.inner.WithAttrs(attrs), withhold: h.withhold}
}

func (h *withholdErrorsHandler) WithGroup(name string) slog.Handler {
	return &withholdErrorsHandler{inner: h.inner.WithGroup(name), withhold: h.withhold}
}

// withholdErrorAttr replaces an error value in a, recursing into groups.
func withholdErrorAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		group := v.Group()
		out := make([]slog.Attr, 0, len(group))
		for _, ga := range group {
			out = append(out, withholdErrorAttr(ga))
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, fmt.Sprintf("<%T>; %s", err, withheldNote))
		}
	}
	return a
}
