/*
Copyright 2026 The Scion Authors.
*/

package telemetry

// usage_analytics.go builds agent.usage analytics events (WP2 design
// sections 3 and 4) from the accepted usage increments of both producers --
// the native UsageDeriver and the hook TelemetryHandler -- and appends them
// to the analytics outbox. Only allowlisted fields are read: never bodies,
// prompts, raw attributes, environment beyond identity, or credentials.

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// UsageRuleSchemaVersion is source_schema_version for every normalization
// rule at introduction. Bump a rule's value when its mapping changes.
const UsageRuleSchemaVersion = "1"

// UsageObservation is one accepted usage increment in event form. Token
// pointers are nil when the source did not report the category.
type UsageObservation struct {
	SourceKind    string
	Identity      string // telemetrycontract.SourceIdentity*
	SourceEventID string // "" for SourceIdentityNone
	SourceTime    time.Time
	EventTime     time.Time
	Model         string // the payload's own model, "" if none
	SessionID     string
	Status        string
	Calls         int64

	Input, Output, CacheRead, CacheWrite, Reasoning *int64
	ReasoningOverlapsOutput                         bool
	InputDerivation                                 string
	TokenStatus                                     string
	MalformedFields                                 []string

	IntervalStart, IntervalEnd time.Time
	Temporality                string

	ReportedCostMicros *int64
	HarnessVersion     string
}

// UsageAnalytics appends usage observations to the outbox. A nil
// *UsageAnalytics is a valid no-op (analytics disabled).
type UsageAnalytics struct {
	outbox      *AnalyticsOutbox
	usageSource string
	now         func() time.Time

	// openOnce/open defer opening the outbox to the first Record (hook
	// processes: only a model-end event pays for the open and flock).
	// failDir is the outbox directory, used to persist append_failed
	// lock-free when the open itself fails.
	openOnce sync.Once
	open     func() (*AnalyticsOutbox, error)
	failDir  string

	droppedNoIdentity, invalid atomic.Int64
}

// NewUsageAnalytics returns a producer for usageSource
// (telemetrycontract.UsageSourceNative or UsageSourceHooks), or nil if
// outbox is nil.
func NewUsageAnalytics(outbox *AnalyticsOutbox, usageSource string) *UsageAnalytics {
	if outbox == nil {
		return nil
	}
	return &UsageAnalytics{outbox: outbox, usageSource: usageSource, now: time.Now}
}

// NewLazyUsageAnalytics returns a producer that opens the outbox at dir
// (owned by owner, see OpenAnalyticsOutboxOwned) only on the first Record.
// If the open fails (for example a lock timeout while another process
// holds the outbox), every Record still leaves persisted, lock-free
// append_failed evidence in dir when dir exists, and returns the error.
func NewLazyUsageAnalytics(dir string, owner *OutboxOwner, usageSource string) *UsageAnalytics {
	return &UsageAnalytics{
		open:        func() (*AnalyticsOutbox, error) { return OpenAnalyticsOutboxOwned(dir, owner) },
		failDir:     dir,
		usageSource: usageSource,
		now:         time.Now,
	}
}

// newFailedUsageAnalytics is a producer whose outbox could not be opened
// (pipeline start): it records nothing but leaves the same evidence per
// accepted increment as a failed lazy open.
func newFailedUsageAnalytics(dir string, openErr error, usageSource string) *UsageAnalytics {
	return &UsageAnalytics{
		open:        func() (*AnalyticsOutbox, error) { return nil, openErr },
		failDir:     dir,
		usageSource: usageSource,
		now:         time.Now,
	}
}

// recordOpenFailure persists one append_failed byte in failDir (lock-free;
// the lock may be what failed). Without an existing directory nothing can
// be persisted and only the per-process invalid counter remains.
func (a *UsageAnalytics) recordOpenFailure() {
	if a.failDir == "" {
		return
	}
	if info, err := os.Stat(a.failDir); err != nil || !info.IsDir() {
		return
	}
	newOutboxHandle(a.failDir, nil).bumpAppendFailed()
}

func (a *UsageAnalytics) ensureOpen() error {
	a.openOnce.Do(func() {
		if a.outbox == nil && a.open != nil {
			o, err := a.open()
			if err != nil {
				log.Debug("Usage analytics outbox unavailable: %v", err)
				return
			}
			a.outbox = o
		}
	})
	if a.outbox == nil {
		return errors.New("analytics: outbox unavailable")
	}
	return nil
}

// AnalyticsEnabledForHooks reports whether a hook process should write
// analytics events: only when the pipeline's GCP exporter (and so the
// drainer) is configured. Otherwise analytics is disabled
// (AnalyticsDisabledNoGCPExporter) and nothing is written.
func AnalyticsEnabledForHooks(cfg *Config) bool {
	return cfg != nil && cfg.Enabled && cfg.IsCloudConfigured() && cfg.IsGCP()
}

// errNoAgentIdentity is returned when SCION_AGENT_ID is unset: the event
// would have no stable producer namespace, so it is not written.
var errNoAgentIdentity = errors.New("analytics: SCION_AGENT_ID is unset; usage event not written")

// Record builds the event for obs and appends it. Safe on nil.
func (a *UsageAnalytics) Record(obs UsageObservation) (AppendOutcome, error) {
	if a == nil {
		return 0, nil
	}
	ev, err := a.build(obs)
	if err != nil {
		if errors.Is(err, errNoAgentIdentity) {
			a.droppedNoIdentity.Add(1)
		} else {
			a.invalid.Add(1)
		}
		return 0, err
	}
	if err := a.ensureOpen(); err != nil {
		a.invalid.Add(1)
		a.recordOpenFailure()
		return 0, err
	}
	outcome, err := a.outbox.AppendUsage(ev)
	if err != nil {
		a.invalid.Add(1)
	}
	return outcome, err
}

// DroppedNoIdentity and Invalid are per-process counters for diagnostics.
func (a *UsageAnalytics) DroppedNoIdentity() int64 {
	if a == nil {
		return 0
	}
	return a.droppedNoIdentity.Load()
}

// Invalid counts events that could not be appended (outbox unavailable or
// an append error); the outbox's own counters break the latter down.
func (a *UsageAnalytics) Invalid() int64 {
	if a == nil {
		return 0
	}
	return a.invalid.Load()
}

// Outbox returns the producer's outbox, opening it if lazy; nil if it
// cannot be opened.
func (a *UsageAnalytics) Outbox() *AnalyticsOutbox {
	if a == nil || a.ensureOpen() != nil {
		return nil
	}
	return a.outbox
}

func (a *UsageAnalytics) build(obs UsageObservation) (*telemetrycontract.UsageEvent, error) {
	agentID := strings.TrimSpace(os.Getenv("SCION_AGENT_ID"))
	if agentID == "" {
		return nil, errNoAgentIdentity
	}
	projectID := projectkeys.ProjectIDFromEnv(os.Getenv)
	now := a.now()
	eventTime := obs.EventTime
	if eventTime.IsZero() {
		eventTime = now
	}
	// event_id, epoch, seq, occurrence and hub_id are assigned by the
	// outbox under its lock.
	env := telemetrycontract.NewEnvelope(telemetrycontract.EventTypeUsage, "pending", usageProducerNamespace(projectID, agentID), eventTime, now)
	env.ProjectID = projectID
	env.AgentID = agentID
	env.BrokerID = os.Getenv("SCION_BROKER_ID")
	env.LaunchID = os.Getenv("SCION_LAUNCH_ID")
	env.ProducerBuild = version.Short()
	env.HarnessVersion = obs.HarnessVersion

	model, modelSource := resolveModelWithSource(obs.Model)
	ev := &telemetrycontract.UsageEvent{
		Envelope:            env,
		UsageSource:         a.usageSource,
		SourceKind:          obs.SourceKind,
		SourceSchemaVersion: UsageRuleSchemaVersion,
		SourceEventID:       obs.SourceEventID,
		SourceIdentity:      obs.Identity,
		Harness:             os.Getenv("SCION_HARNESS"),
		Model:               model,
		ModelSource:         modelSource,
		SessionID:           obs.SessionID,
		Status:              obs.Status,
		Calls:               obs.Calls,
		Input:               obs.Input,
		Output:              obs.Output,
		CacheRead:           obs.CacheRead,
		CacheWrite:          obs.CacheWrite,
		Reasoning:           obs.Reasoning,
		TokenStatus:         obs.TokenStatus,
		MalformedFields:     append([]string{}, obs.MalformedFields...), // always written
		ReportedCostMicros:  obs.ReportedCostMicros,
	}
	if ev.Harness == "" {
		ev.Harness = "unknown"
	}
	if obs.Input != nil {
		ev.InputDerivation = obs.InputDerivation
	}
	if obs.Reasoning != nil && obs.ReasoningOverlapsOutput {
		t := true
		ev.ReasoningOverlapsOutput = &t
	}
	if !obs.SourceTime.IsZero() {
		ev.SourceTime = obs.SourceTime.UTC().Format(time.RFC3339Nano)
	}
	if obs.Temporality != "" {
		if !obs.IntervalStart.IsZero() {
			ev.IntervalStart = telemetrycontract.FormatTS(obs.IntervalStart)
		}
		ev.IntervalEnd = telemetrycontract.FormatTS(obs.IntervalEnd)
		ev.Temporality = obs.Temporality
	}
	if obs.ReportedCostMicros != nil {
		ev.ReportedCostCurrency = "USD"
		ev.ReportedCostSource = "harness"
	}
	if ev.SourceIdentity == "" {
		ev.SourceIdentity = telemetrycontract.SourceIdentityNone
	}
	if ev.SourceIdentity == telemetrycontract.SourceIdentityNone {
		ev.SourceEventID = ""
	}
	return ev, nil
}

// usageProducerNamespace is the stable producer namespace for usage events
// (design section 3): project and agent scoped, never host or process.
func usageProducerNamespace(projectID, agentID string) string {
	if projectID == "" {
		return "sciontool/agent/" + agentID
	}
	return "sciontool/project/" + projectID + "/agent/" + agentID
}

// resolveModelWithSource applies telemetrycontract.ResolveModelLabel's
// precedence and reports which input won.
func resolveModelWithSource(native string) (model, source string) {
	if strings.TrimSpace(native) != "" {
		return telemetrycontract.ResolveModelLabel(native, ""), telemetrycontract.ModelSourceEvent
	}
	if configured := os.Getenv("SCION_MODEL"); strings.TrimSpace(configured) != "" {
		return telemetrycontract.ResolveModelLabel("", configured), telemetrycontract.ModelSourceConfigured
	}
	return telemetrycontract.UnknownModel, telemetrycontract.ModelSourceUnknown
}

// anthropicRequestID is the documented Anthropic request ID shape. Only an
// ID of this shape is trusted as a unique source identity; any other value
// (empty, redacted, a placeholder) is folded into the composite identity.
var anthropicRequestID = regexp.MustCompile(`^req_[A-Za-z0-9]{16,}$`)

// trustedRequestID returns the record's request_id if it passes the
// source-specific shape check for scope, else "".
func trustedRequestID(scopeName string, attrs []*commonpb.KeyValue) string {
	if scopeName != claudeUsageScope {
		return ""
	}
	if id := logAttrString(attrs, "request_id"); anthropicRequestID.MatchString(id) {
		return id
	}
	return ""
}

// logRecordIdentity derives the source identity of a native log record
// (design section 4): a verified request ID, else a composite over scope,
// event name, record time (time_unix_nano, else observed_time_unix_nano)
// and the canonical attribute key, else none.
func logRecordIdentity(scopeName, eventName string, record *logspb.LogRecord) (identity, sourceEventID string) {
	if id := trustedRequestID(scopeName, record.GetAttributes()); id != "" {
		return telemetrycontract.SourceIdentityRequestID, "rid1:" + lengthPrefixedHash(scopeName, eventName, id)
	}
	recordTime := record.GetTimeUnixNano()
	if recordTime == 0 {
		recordTime = record.GetObservedTimeUnixNano()
	}
	attrs, err := canonicalAttrs(record.GetAttributes())
	if err != nil || (recordTime == 0 && len(record.GetAttributes()) == 0) {
		return telemetrycontract.SourceIdentityNone, ""
	}
	return telemetrycontract.SourceIdentityComposite, "cmp1:" + lengthPrefixedHash(scopeName, eventName, strconv.FormatUint(recordTime, 10), attrs)
}

// logRecordSourceTime is the record's own time at full precision:
// time_unix_nano, else observed_time_unix_nano, else the event.timestamp
// attribute.
func logRecordSourceTime(record *logspb.LogRecord) time.Time {
	if ns := record.GetTimeUnixNano(); ns != 0 {
		return time.Unix(0, int64(ns)).UTC()
	}
	if ns := record.GetObservedTimeUnixNano(); ns != 0 {
		return time.Unix(0, int64(ns)).UTC()
	}
	if s := logAttrString(record.GetAttributes(), "event.timestamp"); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// tokenField is one source token attribute read with presence.
type tokenField struct {
	n         int64
	present   bool
	malformed bool
}

func readTokenField(attrs []*commonpb.KeyValue, key string) tokenField {
	if !logAttrPresent(attrs, key) {
		return tokenField{}
	}
	n, err := logAttrInt(attrs, key)
	if err != nil {
		return tokenField{present: true, malformed: true}
	}
	return tokenField{n: n, present: true}
}

func (f tokenField) ok() bool { return f.present && !f.malformed }

func (f tokenField) ptr() *int64 {
	if !f.ok() {
		return nil
	}
	n := f.n
	return &n
}

func int64Ptr(n int64) *int64 { return &n }

// tokenStatusOf classifies the source's defined token fields.
func tokenStatusOf(fields ...tokenField) string {
	present, malformed := 0, false
	for _, f := range fields {
		if f.present {
			present++
		}
		if f.malformed {
			malformed = true
		}
	}
	switch {
	case malformed:
		return telemetrycontract.TokenStatusMalformed
	case present == len(fields):
		return telemetrycontract.TokenStatusReported
	case present == 0:
		return telemetrycontract.TokenStatusNotReported
	default:
		return telemetrycontract.TokenStatusPartial
	}
}

func malformedNames(keys []string, fields []tokenField) []string {
	var out []string
	for i, f := range fields {
		if f.malformed {
			out = append(out, keys[i])
		}
	}
	return out
}

// errorObservation is the event form of a failed call: no tokens.
func errorObservation(sourceKind string) UsageObservation {
	return UsageObservation{
		SourceKind:  sourceKind,
		Status:      telemetrycontract.StatusError,
		Calls:       1,
		TokenStatus: telemetrycontract.TokenStatusNotReported,
	}
}

// claudeObservation: Claude reports exclusive counts, so input is
// "reported"; no reasoning field exists, so reasoning is unknown.
func claudeObservation(eventName string, attrs []*commonpb.KeyValue) UsageObservation {
	if eventName == "api_error" {
		obs := errorObservation("claude.api_error")
		obs.SessionID = logAttrString(attrs, "session.id")
		return obs
	}
	keys := []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens"}
	fields := make([]tokenField, len(keys))
	for i, k := range keys {
		fields[i] = readTokenField(attrs, k)
	}
	obs := UsageObservation{
		SourceKind:      "claude.api_request",
		SessionID:       logAttrString(attrs, "session.id"),
		Status:          telemetrycontract.StatusSuccess,
		Calls:           1,
		Input:           fields[0].ptr(),
		Output:          fields[1].ptr(),
		CacheRead:       fields[2].ptr(),
		CacheWrite:      fields[3].ptr(),
		InputDerivation: telemetrycontract.InputDerivationReported,
		TokenStatus:     tokenStatusOf(fields...),
		MalformedFields: malformedNames(keys, fields),
	}
	if cost := readTokenField(attrs, "cost_usd_micros"); cost.ok() {
		obs.ReportedCostMicros = cost.ptr()
	}
	return obs
}

// codexObservation: input_token_count is inclusive of cached and
// cache-write tokens, so input is derived only when all three are present
// and consistent; output includes reasoning.
func codexObservation(attrs []*commonpb.KeyValue, kind string) UsageObservation {
	switch kind {
	case "response_failed":
		return errorObservation("codex.response_failed")
	case "api_request_failed":
		return errorObservation("codex.api_request_failed")
	}
	keys := []string{"input_token_count", "output_token_count", "cached_token_count", "cache_write_token_count", "reasoning_token_count"}
	fields := make([]tokenField, len(keys))
	for i, k := range keys {
		fields[i] = readTokenField(attrs, k)
	}
	input, output, cached, cacheWrite, reasoning := fields[0], fields[1], fields[2], fields[3], fields[4]
	if input.ok() && cached.ok() && cacheWrite.ok() && (cached.n > input.n || cacheWrite.n > input.n-cached.n) {
		// Inconsistent subsets: the three related fields are unusable.
		input.malformed, cached.malformed, cacheWrite.malformed = true, true, true
		fields[0], fields[2], fields[3] = input, cached, cacheWrite
	}
	obs := UsageObservation{
		SourceKind:              "codex.response_completed",
		Status:                  telemetrycontract.StatusSuccess,
		Calls:                   1,
		Output:                  output.ptr(),
		CacheRead:               cached.ptr(),
		CacheWrite:              cacheWrite.ptr(),
		Reasoning:               reasoning.ptr(),
		ReasoningOverlapsOutput: true,
		TokenStatus:             tokenStatusOf(fields...),
		MalformedFields:         malformedNames(keys, fields),
	}
	if input.ok() && cached.ok() && cacheWrite.ok() {
		obs.Input = int64Ptr(input.n - cached.n - cacheWrite.n)
		obs.InputDerivation = telemetrycontract.InputDerivationDerived
	}
	return obs
}

// geminiObservation: prompt includes cached content; output is candidates
// plus thoughts (canonical output includes reasoning); tool-use prompt
// tokens are input.
func geminiObservation(eventName string, attrs []*commonpb.KeyValue) UsageObservation {
	if eventName == geminiCLIAPIErrorEvent {
		obs := errorObservation("gemini-cli.api_error")
		obs.SessionID = logAttrString(attrs, "session.id")
		return obs
	}
	keys := []string{"input_token_count", "output_token_count", "cached_content_token_count", "thoughts_token_count", "tool_token_count"}
	fields := make([]tokenField, len(keys))
	for i, k := range keys {
		fields[i] = readTokenField(attrs, k)
	}
	prompt, candidates, cached, thoughts, tool := fields[0], fields[1], fields[2], fields[3], fields[4]
	if prompt.ok() && cached.ok() && cached.n > prompt.n {
		prompt.malformed, cached.malformed = true, true
		fields[0], fields[2] = prompt, cached
	}
	obs := UsageObservation{
		SourceKind:              "gemini-cli.api_response",
		SessionID:               logAttrString(attrs, "session.id"),
		Status:                  telemetrycontract.StatusSuccess,
		Calls:                   1,
		CacheRead:               cached.ptr(),
		Reasoning:               thoughts.ptr(),
		ReasoningOverlapsOutput: true,
		TokenStatus:             tokenStatusOf(fields...),
		MalformedFields:         malformedNames(keys, fields),
	}
	if prompt.ok() && cached.ok() && tool.ok() {
		obs.Input = int64Ptr(prompt.n - cached.n + tool.n)
		obs.InputDerivation = telemetrycontract.InputDerivationDerived
	}
	if candidates.ok() && thoughts.ok() {
		obs.Output = int64Ptr(candidates.n + thoughts.n)
	}
	return obs
}

// harnessVersionFromAttrs reads the harness version a native source
// reports: codex's app.version attribute, else the resource's
// service.version (Claude Code).
func harnessVersionFromAttrs(recordAttrs, resourceAttrs []*commonpb.KeyValue) string {
	if v := logAttrString(recordAttrs, "app.version"); v != "" {
		return v
	}
	return logAttrString(resourceAttrs, "service.version")
}

// logObservation builds the event form of a native log increment accepted
// by the rule for harness.
func logObservation(harness, scopeName, eventName string, record *logspb.LogRecord, resourceAttrs []*commonpb.KeyValue, inc usageIncrement) UsageObservation {
	attrs := record.GetAttributes()
	var obs UsageObservation
	switch harness {
	case "claude":
		obs = claudeObservation(eventName, attrs)
	case "codex":
		kind := "response_completed"
		switch {
		case eventName == codexAPIRequestEventName:
			kind = "api_request_failed"
		case inc.Status == telemetrycontract.StatusError:
			kind = "response_failed"
		}
		obs = codexObservation(attrs, kind)
		obs.SessionID = logAttrString(attrs, "conversation.id")
	case "gemini-cli":
		obs = geminiObservation(eventName, attrs)
	default:
		obs = UsageObservation{SourceKind: harness + "." + eventName, Status: inc.Status, Calls: inc.Calls, TokenStatus: telemetrycontract.TokenStatusNotReported}
	}
	obs.Model = inc.Model
	obs.Identity, obs.SourceEventID = logRecordIdentity(scopeName, eventName, record)
	obs.SourceTime = logRecordSourceTime(record)
	obs.EventTime = obs.SourceTime
	obs.HarnessVersion = harnessVersionFromAttrs(attrs, resourceAttrs)
	return obs
}

// metricObservation builds the event form of a metric-sourced increment
// (copilot). Calls and tokens arrive as separate increments, so they are
// separate events with distinct source kinds. A token category absent from
// the batch is unknown, and input (inclusive at the source) is derived only
// when both cache categories were reported.
func metricObservation(inc usageIncrement, fingerprint string) UsageObservation {
	obs := UsageObservation{
		Identity:      telemetrycontract.SourceIdentityMetricInterval,
		SourceEventID: "met1:" + fingerprint,
		Model:         inc.Model,
		Status:        inc.Status,
		Calls:         inc.Calls,
		Temporality:   inc.temporality,
	}
	if inc.intervalEndNanos != 0 {
		obs.IntervalEnd = time.Unix(0, int64(inc.intervalEndNanos)).UTC()
		obs.SourceTime = obs.IntervalEnd
		obs.EventTime = obs.IntervalEnd
	}
	// An unknown interval start is omitted (interval_end and temporality
	// still mark the event as metric-derived, never per-request).
	if inc.intervalStartNanos != 0 {
		obs.IntervalStart = time.Unix(0, int64(inc.intervalStartNanos)).UTC()
	}
	if obs.IntervalEnd.IsZero() {
		obs.Temporality = ""
	} else if obs.Temporality == "" {
		obs.Temporality = telemetrycontract.TemporalityDelta
	}
	if inc.rawTokens == nil {
		obs.SourceKind = "copilot.metrics.calls"
		obs.TokenStatus = telemetrycontract.TokenStatusNotReported
		return obs
	}
	obs.SourceKind = "copilot.metrics.tokens"
	get := func(tokenType string) tokenField {
		n, ok := inc.rawTokens[tokenType]
		return tokenField{n: n, present: ok}
	}
	input, output := get(telemetrycontract.TokenTypeInput), get(telemetrycontract.TokenTypeOutput)
	cacheRead, cacheWrite := get(telemetrycontract.TokenTypeCacheRead), get(telemetrycontract.TokenTypeCacheWrite)
	reasoning := get(telemetrycontract.TokenTypeReasoning)
	fields := []tokenField{input, output, cacheRead, cacheWrite, reasoning}
	keys := []string{"input_tokens", "output_tokens", "cache_read.input_tokens", "cache_write.input_tokens", "reasoning.output_tokens"}
	if input.ok() && cacheRead.ok() && cacheWrite.ok() && cacheRead.n+cacheWrite.n > input.n {
		fields[0].malformed, fields[2].malformed, fields[3].malformed = true, true, true
	}
	obs.Output = fields[1].ptr()
	obs.CacheRead = fields[2].ptr()
	obs.CacheWrite = fields[3].ptr()
	obs.Reasoning = fields[4].ptr()
	obs.ReasoningOverlapsOutput = true
	obs.TokenStatus = tokenStatusOf(fields...)
	obs.MalformedFields = malformedNames(keys, fields)
	if fields[0].ok() && fields[2].ok() && fields[3].ok() {
		obs.Input = int64Ptr(input.n - cacheRead.n - cacheWrite.n)
		obs.InputDerivation = telemetrycontract.InputDerivationDerived
	}
	return obs
}

// CompositeSourceEventID returns a versioned composite source_event_id over
// parts (length-prefixed, so no two part lists collide). Producers outside
// this package (the hook handler) use it for vetted composite identities.
func CompositeSourceEventID(parts ...string) string {
	return "cmp1:" + lengthPrefixedHash(parts...)
}
