/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
	"golang.org/x/sys/unix"
)

// maxMuseTrackedTurns bounds the number of (session_id, turn_id) high-water
// marks retained for Muse Code's cumulative PostLLMCall.usage delta tracking.
// Each UUID-keyed entry is ~220 bytes across Order and Turns, so 1024 entries
// cap the state file at ~225 KiB (well under the 1 MiB read limit). Eviction
// is LRU (least-recently-touched), so an active main turn stays resident even
// when more than maxMuseTrackedTurns subagent/reminder turns interleave with
// it. Eviction trades a bounded state file for a possible over-count (if more
// than maxMuseTrackedTurns other turns run between two consecutive steps of
// one idle turn, its next step re-emits its cumulative total), never an
// under-count.
const maxMuseTrackedTurns = 1024

type museTurnUsageTotals struct {
	Input      int64 `json:"input,omitempty"`
	Output     int64 `json:"output,omitempty"`
	Cached     int64 `json:"cached,omitempty"`
	CacheWrite int64 `json:"cache_write,omitempty"`
	Reasoning  int64 `json:"reasoning,omitempty"`
}

type museTurnUsageState struct {
	Order []string                       `json:"order,omitempty"`
	Turns map[string]museTurnUsageTotals `json:"turns,omitempty"`
}

// apply computes the per-step non-negative delta from cur against the stored
// cumulative high-water mark for key, advances the high-water mark, and marks
// key as most-recently-used so active turns are never evicted by newer idle
// subagent turns.
func (s *museTurnUsageState) apply(key string, cur museTurnUsageTotals) museTurnUsageTotals {
	if s.Turns == nil {
		s.Turns = make(map[string]museTurnUsageTotals)
	}
	prev, exists := s.Turns[key]
	delta := museTurnUsageTotals{
		Input:      nonNegDelta(cur.Input, prev.Input),
		Output:     nonNegDelta(cur.Output, prev.Output),
		Cached:     nonNegDelta(cur.Cached, prev.Cached),
		CacheWrite: nonNegDelta(cur.CacheWrite, prev.CacheWrite),
		Reasoning:  nonNegDelta(cur.Reasoning, prev.Reasoning),
	}
	s.Turns[key] = museTurnUsageTotals{
		Input:      maxInt64(prev.Input, cur.Input),
		Output:     maxInt64(prev.Output, cur.Output),
		Cached:     maxInt64(prev.Cached, cur.Cached),
		CacheWrite: maxInt64(prev.CacheWrite, cur.CacheWrite),
		Reasoning:  maxInt64(prev.Reasoning, cur.Reasoning),
	}
	if exists {
		// Move key to the tail of Order (most-recently-used).
		for i, k := range s.Order {
			if k == key {
				copy(s.Order[i:], s.Order[i+1:])
				s.Order[len(s.Order)-1] = key
				break
			}
		}
	} else {
		s.Order = append(s.Order, key)
		for len(s.Order) > maxMuseTrackedTurns {
			oldest := s.Order[0]
			s.Order = s.Order[1:]
			delete(s.Turns, oldest)
		}
	}
	return delta
}

func nonNegDelta(cur, prev int64) int64 {
	if cur <= prev {
		return 0
	}
	return cur - prev
}

func maxInt64(a, b int64) int64 {
	if a >= b {
		return a
	}
	return b
}

// MuseCodeVerifiedVersions lists the Muse Code (`muse`) CLI versions whose
// PostLLMCall turn-cumulative usage semantics have been verified against live
// captures and pinned by
// pkg/sciontool/hooks/dialects/testdata/muse-code/session-usage-*.jsonl
// fixtures.
//
// Upstream release tags carry a build suffix (e.g. "1.4.25-R9361.1"); entries
// here record the canonical semver ("1.4.25") matching the fixture filename
// and `muse --version` prefix. See
// docs-site/src/content/docs/contributing/harness-cli-version-bump.md for the
// bump and re-verification process.
var MuseCodeVerifiedVersions = []string{
	"1.4.25",
}

const (
	museVersionProbeTimeout     = 1 * time.Second
	museVersionProbeWaitDelay   = 500 * time.Millisecond
	museVersionFailureMarkerTTL = 5 * time.Minute
	museVersionMarkerFileName   = "muse-version-check.json"
	museVersionMarkerMaxBytes   = 64 << 10
	museVersionProcessMemoMax   = 64
)

// museVersionNow returns the current time for negative-marker TTL checks.
// Tests may override it to exercise TTL expiry without sleeping.
var museVersionNow = time.Now

// museProbeBinaryName is the bare command name resolved via rootexec.Resolve
// by detectLocalMuseVersion. Tests may override it (e.g. to "false") to
// exercise probe-failure metadata preservation against a real root-owned binary.
var museProbeBinaryName = "muse"

// museVersionTokenRe extracts a complete version token (optional leading "v",
// X.Y.Z, plus any extra numeric segments, prerelease "-...", or build metadata
// "+...") bounded by whitespace or parentheses/commas. Capturing the full token
// prevents prereleases ("1.4.25-rc.1"), extra segments ("1.4.25.1"), and build
// metadata ("1.4.25+build.1") from being silently normalized to "1.4.25".
var (
	museVersionTokenRe  = regexp.MustCompile(`(?:^|[\s(])v?(\d+\.\d+\.\d+(?:\.\d+)*(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)(?:$|[\s),])`)
	museReleaseSuffixRe = regexp.MustCompile(`^(\d+\.\d+\.\d+)-R\d+(?:\.\d+)*$`)
)

// normalizeMuseVersion extracts the version token from raw `muse --version`
// output (for example "Muse Code 1.4.25 (1.4.25-R9361.1)" -> "1.4.25",
// "1.4.25-R9361.1" -> "1.4.25", "v1.4.25" -> "1.4.25"). Muse's upstream
// `-R<digits>` release tag suffix is normalized to X.Y.Z, while prereleases
// ("1.4.25-rc.1"), extra numeric segments ("1.4.25.1"), and build metadata
// ("1.4.25+build.1") are preserved verbatim so they never count as verified.
// Returns "" if empty or unparseable.
func normalizeMuseVersion(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	m := museVersionTokenRe.FindStringSubmatch(s)
	if len(m) != 2 {
		return ""
	}
	ver := m[1]
	if rm := museReleaseSuffixRe.FindStringSubmatch(ver); len(rm) == 2 {
		return rm[1]
	}
	return ver
}

type museVersionCheckMarker struct {
	CheckedVersion   string   `json:"checked_version,omitempty"`
	Verified         bool     `json:"verified"`
	ProbeFailed      bool     `json:"probe_failed,omitempty"`
	CheckedAtUnixNS  int64    `json:"checked_at_unix_ns,omitempty"`
	VerifiedVersions []string `json:"verified_versions,omitempty"`
	BinaryPath       string   `json:"binary_path,omitempty"`
	BinaryModTimeNS  int64    `json:"binary_mtime_ns,omitempty"`
	BinarySize       int64    `json:"binary_size,omitempty"`
}

var (
	museVersionMemoMu sync.Mutex
	museVersionMemo   = make(map[string]museVersionCheckMarker)
)

func readMuseVersionProcessMemo(markerPath string, now time.Time) (museVersionCheckMarker, bool) {
	museVersionMemoMu.Lock()
	defer museVersionMemoMu.Unlock()
	m, ok := museVersionMemo[markerPath]
	if !ok {
		return museVersionCheckMarker{}, false
	}
	if !museVersionMarkerValid(m, now) {
		delete(museVersionMemo, markerPath)
		return museVersionCheckMarker{}, false
	}
	return m, true
}

func storeMuseVersionProcessMemo(markerPath string, m museVersionCheckMarker) {
	museVersionMemoMu.Lock()
	defer museVersionMemoMu.Unlock()
	if len(museVersionMemo) >= museVersionProcessMemoMax {
		clear(museVersionMemo)
	}
	museVersionMemo[markerPath] = m
}

func deleteMuseVersionProcessMemo(markerPath string) {
	museVersionMemoMu.Lock()
	defer museVersionMemoMu.Unlock()
	delete(museVersionMemo, markerPath)
}

func (h *TelemetryHandler) museVersionCheckPath() string {
	if h.MuseVersionCheckStatePath != "" {
		return h.MuseVersionCheckStatePath
	}
	if h.MuseTurnUsageStatePath != "" {
		return filepath.Join(filepath.Dir(h.MuseTurnUsageStatePath), museVersionMarkerFileName)
	}
	return ""
}

// museVersionMarkerValid reports whether m is still valid at now. Positive
// markers remain valid until MuseCodeVerifiedVersions or the binary's
// mtime/size changes. Negative markers (ProbeFailed) remain valid for
// museVersionFailureMarkerTTL; a non-positive or future CheckedAtUnixNS
// (elapsed < 0, e.g. from clock skew or a tampered marker) is treated as
// expired so probing is never suppressed indefinitely.
func museVersionMarkerValid(m museVersionCheckMarker, now time.Time) bool {
	if !slices.Equal(m.VerifiedVersions, MuseCodeVerifiedVersions) {
		return false
	}
	if !museBinaryMatchesMarker(m) {
		return false
	}
	if m.ProbeFailed {
		if m.CheckedAtUnixNS <= 0 {
			return false
		}
		elapsed := now.UnixNano() - m.CheckedAtUnixNS
		return elapsed >= 0 && elapsed < int64(museVersionFailureMarkerTTL)
	}
	return m.CheckedVersion != ""
}

func readMuseVersionCheckMarker(path string) (museVersionCheckMarker, bool) {
	tree, err := privatefs.OpenTree(filepath.Dir(path), privatefs.TreeOptions{})
	if err != nil {
		return museVersionCheckMarker{}, false
	}
	defer func() { _ = tree.Close() }()
	raw, err := tree.ReadPrivate(filepath.Base(path), museVersionMarkerMaxBytes)
	if err != nil || len(raw) == 0 {
		return museVersionCheckMarker{}, false
	}
	var m museVersionCheckMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		return museVersionCheckMarker{}, false
	}
	if m.CheckedVersion == "" && (!m.ProbeFailed || m.CheckedAtUnixNS <= 0) {
		return museVersionCheckMarker{}, false
	}
	return m, true
}

func writeMuseVersionCheckMarker(path string, m museVersionCheckMarker) error {
	encoded, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing %s: %w", path, privatefs.ErrReparsePoint)
	}
	tree, err := privatefs.OpenTree(filepath.Dir(path), privatefs.TreeOptions{CreateAnchor: true})
	if err != nil {
		return err
	}
	defer func() { _ = tree.Close() }()
	base := filepath.Base(path)
	tmp, tmpRel, err := tree.CreateTemp("", ".muse-version-check-*.tmp", privatefs.Private)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = tree.Remove(tmpRel)
		}
	}()
	if _, err := tmp.Write(encoded); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing %s: %w", path, privatefs.ErrReparsePoint)
	}
	if err := tree.Rename(tmpRel, base); err != nil {
		return err
	}
	ok = true
	return nil
}

func museBinaryMatchesMarker(m museVersionCheckMarker) bool {
	if m.BinaryPath == "" {
		return true
	}
	info, err := os.Stat(m.BinaryPath)
	if err != nil {
		return false
	}
	return info.ModTime().UnixNano() == m.BinaryModTimeNS && info.Size() == m.BinarySize
}

// configureMuseProbeCmd hardens a muse version-probe command with a minimal
// environment, its own process group so cancel kills any descendant holding
// stdout open, and WaitDelay so Wait() never blocks past the bound even if a
// descendant inherits the stdout pipe.
func configureMuseProbeCmd(cmd *exec.Cmd) {
	cmd.Env = rootexec.Env("MUSE_NO_AUTO_UPDATE=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = museVersionProbeWaitDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// runMuseProbeCmd executes a configured muse version-probe command via
// procreap.OutputManaged. On any error (including WaitDelay expiring when a
// descendant holds stdout open after the leader exited normally), it kills the
// probe's process group so no descendant in the group leaks. When the leader
// exited normally and only WaitDelay expired, stdout already read is preserved.
func runMuseProbeCmd(cmd *exec.Cmd) ([]byte, error) {
	out, err := procreap.OutputManaged(cmd)
	if err != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return nil, err
	}
	return out, nil
}

// detectLocalMuseVersion probes the installed `muse` binary with a bounded
// timeout + WaitDelay using rootexec (no shell, hardened env with
// MUSE_NO_AUTO_UPDATE=1) and procreap, returning the normalized version along
// with the resolved binary's path/mtime/size for cross-process marker
// invalidation. Any resolve, exec, timeout, or parse error returns "" (still
// preserving binPath/mtimeNS/size when resolved so negative markers invalidate
// on binary replacement) so the hook path never fails or blocks past the
// timeout + WaitDelay bound.
func detectLocalMuseVersion(ctx context.Context) (ver, binPath string, mtimeNS, size int64) {
	museBin, err := rootexec.Resolve(museProbeBinaryName)
	if err != nil {
		return "", "", 0, 0
	}
	if info, statErr := os.Stat(museBin); statErr == nil {
		binPath = museBin
		mtimeNS = info.ModTime().UnixNano()
		size = info.Size()
	}
	cctx, cancel := context.WithTimeout(ctx, museVersionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, museBin, "--version")
	configureMuseProbeCmd(cmd)
	out, err := runMuseProbeCmd(cmd)
	if err != nil {
		return "", binPath, mtimeNS, size
	}
	return normalizeMuseVersion(string(out)), binPath, mtimeNS, size
}

// checkMuseVersionOnce probes the installed muse CLI version across
// short-lived hook processes (persisted atomically via same-directory 0600
// temp file + rename in a marker file alongside MuseTurnUsageStatePath, or
// once per handler when no state path is configured) and logs a warning if the
// parseable version is outside MuseCodeVerifiedVersions.
//
// No cross-process or in-process lock is taken across the probe: a hook never
// waits on another hook process. On a rare first-run race before the marker is
// in place, up to N probes and N warnings may occur where N is the number of
// concurrently racing hook processes (not strictly 1); atomic temp+rename
// ensures the marker on disk is never corrupted and suppresses later hooks.
//
// Failed or unparseable probes are memoized as a negative marker with a short
// TTL (museVersionFailureMarkerTTL; future timestamps are treated as expired).
// When the marker cannot be read or written (symlink, directory, read-only
// directory), results are memoized per process (bounded at
// museVersionProcessMemoMax entries) so an unusable marker costs at most one
// probe per process and worst-case per-hook latency stays bounded by
// museVersionProbeTimeout + museVersionProbeWaitDelay (1.5s). Missing,
// corrupt, symlinked, or unwritable markers and probe errors never fail or
// panic the hook path.
func (h *TelemetryHandler) checkMuseVersionOnce() {
	h.museVersionCheckOnce.Do(func() {
		markerPath := h.museVersionCheckPath()
		now := museVersionNow()
		if markerPath != "" {
			if m, ok := readMuseVersionCheckMarker(markerPath); ok && museVersionMarkerValid(m, now) {
				return
			}
			if _, ok := readMuseVersionProcessMemo(markerPath, now); ok {
				return
			}
		}

		var ver, binPath string
		var mtimeNS, size int64
		if h.museVersionProbe != nil {
			ver = normalizeMuseVersion(h.museVersionProbe(context.Background()))
		} else {
			ver, binPath, mtimeNS, size = detectLocalMuseVersion(context.Background())
		}

		now = museVersionNow()
		if ver == "" {
			if markerPath != "" {
				neg := museVersionCheckMarker{
					ProbeFailed:      true,
					CheckedAtUnixNS:  now.UnixNano(),
					VerifiedVersions: slices.Clone(MuseCodeVerifiedVersions),
					BinaryPath:       binPath,
					BinaryModTimeNS:  mtimeNS,
					BinarySize:       size,
				}
				if err := writeMuseVersionCheckMarker(markerPath, neg); err != nil {
					storeMuseVersionProcessMemo(markerPath, neg)
				} else {
					deleteMuseVersionProcessMemo(markerPath)
				}
			}
			return
		}

		verified := slices.Contains(MuseCodeVerifiedVersions, ver)
		if !verified {
			slog.Warn(
				"muse-code CLI version differs from verified usage-decoding versions; usage reporting (turn-cumulative PostLLMCall delta normalization) may silently degrade or miscount tokens — see docs-site/src/content/docs/contributing/harness-cli-version-bump.md",
				"harness", "muse-code",
				"installed_version", ver,
				"verified_versions", strings.Join(MuseCodeVerifiedVersions, ","),
			)
		}
		if markerPath != "" {
			pos := museVersionCheckMarker{
				CheckedVersion:   ver,
				Verified:         verified,
				CheckedAtUnixNS:  now.UnixNano(),
				VerifiedVersions: slices.Clone(MuseCodeVerifiedVersions),
				BinaryPath:       binPath,
				BinaryModTimeNS:  mtimeNS,
				BinarySize:       size,
			}
			if err := writeMuseVersionCheckMarker(markerPath, pos); err != nil {
				storeMuseVersionProcessMemo(markerPath, pos)
			} else {
				deleteMuseVersionProcessMemo(markerPath)
			}
		}
	})
}

// normalizeMuseTurnUsage converts Muse Code's turn-cumulative PostLLMCall.usage
// into a per-step canonical increment keyed by (session_id, turn_id).
//
// Version note: cumulative-per-turn PostLLMCall.usage semantics were verified
// live against muse 1.4.25 (1.4.25-R9361.1; see MuseCodeVerifiedVersions).
// These semantics must be re-verified on muse upgrades: if a future release
// changes PostLLMCall.usage to per-step counts, this high-water subtraction
// would under-count steps after step 0. See
// docs-site/src/content/docs/contributing/harness-cli-version-bump.md.
//
// Out-of-order hooks: if step N+1's hook process acquires the lock before
// step N's, the monotonic high-water mark keeps summed turn totals exact,
// attributing the combined increment to N+1 and leaving N with zero/not_reported
// tokens (never producing negative deltas).
//
// Failure mode: when cross-process state persistence is configured or required
// (MuseTurnUsageStatePath != "" or MuseTurnUsageStateRequired == true) and the
// state file cannot be used (empty path, mkdir/open/flock/read/write/rename
// failure), this fails closed on token counts: it logs a warning, zeroes all
// token fields on the copied event so turn-cumulative totals are never
// published as a step delta, and keeps the model-end call (token_status =
// not_reported).
//
// Returns event unchanged for non-muse-code events, non-model-end events, or
// payloads without a turn_id. Otherwise returns a shallow copy of event with
// delta-adjusted token fields so callers' Event structs are never mutated.
func (h *TelemetryHandler) normalizeMuseTurnUsage(event *hooks.Event) *hooks.Event {
	if event == nil || event.Dialect != "muse-code" || event.Name != hooks.EventModelEnd {
		return event
	}
	h.checkMuseVersionOnce()
	turnID := rawScalar(event.Data.Raw, "turn_id")
	if turnID == "" {
		return event
	}
	key := turnID
	if event.Data.SessionID != "" {
		key = event.Data.SessionID + ":" + turnID
	}
	cur := museTurnUsageTotals{
		Input:      event.Data.InputTokens,
		Output:     event.Data.OutputTokens,
		Cached:     event.Data.CachedTokens,
		CacheWrite: event.Data.CacheWriteTokens,
		Reasoning:  event.Data.ReasoningTokens,
	}

	var delta museTurnUsageTotals
	var failedClosed bool
	switch {
	case h.MuseTurnUsageStatePath != "":
		var err error
		delta, err = applyMuseTurnUsageFile(h.MuseTurnUsageStatePath, key, cur)
		if err != nil {
			log.Warn("Muse turn usage delta state file %q failed (%v); dropping tokens for untrackable turn %s", h.MuseTurnUsageStatePath, err, key)
			failedClosed = true
		}
	case h.MuseTurnUsageStateRequired:
		log.Warn("Muse turn usage state path is unavailable (empty home); dropping tokens for untrackable turn %s", key)
		failedClosed = true
	default:
		delta = h.applyMuseTurnUsageMem(key, cur)
	}

	copied := *event
	if failedClosed {
		copied.Data.ClearTokens()
		return &copied
	}
	// Failed steps in Muse Code repeat the prior step's cumulative usage,
	// yielding all-zero deltas. Clear token presence so failed calls report
	// token_status="not_reported" with nil token pointers rather than
	// fabricating measured zeros.
	if event.Data.Error != "" && delta == (museTurnUsageTotals{}) {
		copied.Data.ClearTokens()
		return &copied
	}
	copied.Data.InputTokens = delta.Input
	copied.Data.OutputTokens = delta.Output
	copied.Data.CachedTokens = delta.Cached
	copied.Data.CacheWriteTokens = delta.CacheWrite
	copied.Data.ReasoningTokens = delta.Reasoning
	copied.Data.TokenPresence = hooks.TokenPresence{
		Input:      event.Data.HasInputTokens(),
		Output:     event.Data.HasOutputTokens(),
		Cached:     event.Data.HasCachedTokens(),
		CacheWrite: event.Data.HasCacheWriteTokens(),
		Reasoning:  event.Data.HasReasoningTokens(),
	}
	return &copied
}

func (h *TelemetryHandler) applyMuseTurnUsageMem(key string, cur museTurnUsageTotals) museTurnUsageTotals {
	h.turnUsageMu.Lock()
	defer h.turnUsageMu.Unlock()
	if h.turnUsageMem == nil {
		h.turnUsageMem = &museTurnUsageState{}
	}
	return h.turnUsageMem.apply(key, cur)
}

func applyMuseTurnUsageFile(path, key string, cur museTurnUsageTotals) (museTurnUsageTotals, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return museTurnUsageTotals{}, err
	}

	lockPath := path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return museTurnUsageTotals{}, err
	}
	defer func() { _ = lockFile.Close() }()

	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		return museTurnUsageTotals{}, err
	}
	defer func() { _ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN) }()

	var st museTurnUsageState
	dataFile, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err == nil {
		raw, readErr := io.ReadAll(io.LimitReader(dataFile, 1<<20))
		_ = dataFile.Close()
		if readErr != nil {
			return museTurnUsageTotals{}, readErr
		}
		if len(raw) > 0 {
			if unmarshalErr := json.Unmarshal(raw, &st); unmarshalErr != nil && !errors.Is(unmarshalErr, io.EOF) {
				// Corrupt state file: reset rather than failing the hook.
				log.Warn("Muse turn usage state file %q corrupt (%v); resetting", path, unmarshalErr)
				st = museTurnUsageState{}
			}
		}
	} else if !os.IsNotExist(err) {
		return museTurnUsageTotals{}, err
	}

	delta := st.apply(key, cur)

	encoded, err := json.Marshal(st)
	if err != nil {
		return museTurnUsageTotals{}, err
	}

	tmp, err := os.CreateTemp(dir, ".muse-turn-usage-*.tmp")
	if err != nil {
		return museTurnUsageTotals{}, err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return museTurnUsageTotals{}, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return museTurnUsageTotals{}, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return museTurnUsageTotals{}, err
	}
	return delta, nil
}
