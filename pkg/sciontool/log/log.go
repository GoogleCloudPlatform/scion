/*
Copyright 2025 The Scion Authors.
*/

package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

var (
	logPath string
	// debug and initialized are atomic so the hot paths (Debug, write,
	// slogHandler.Enabled) can read them without taking mu. Writers still
	// hold mu where they also touch logPath/logFile, so that Init and the
	// lazy init in write() stay mutually exclusive.
	// debug governs Debug lines. It is synced from the shared level state
	// (SCION_LOG_LEVEL, --log-level) at Init and by ApplyLogLevel, and can
	// be forced with SetDebug. Info/Warn/Error lines read the shared level
	// state directly.
	debug       atomic.Bool
	quiet       atomic.Bool
	mu          sync.Mutex
	initialized atomic.Bool
	// initRuns counts executions of initLocked. It is test-only, with no
	// production purpose: tests use it to check that concurrent first log
	// calls run the lazy init exactly once.
	initRuns atomic.Int64
	// subsystemScans counts record attribute scans for a subsystem in
	// slogHandler.Handle. It is test-only: tests use it to check that the
	// scan is skipped when the spec has no per-component levels.
	subsystemScans atomic.Int64
	// logFile is the cached, already-opened handle for logPath, guarded by
	// mu, and reused for every log line for the life of the process:
	// reopening logPath from scratch on each line would give a workload a
	// fresh chance, on every single line, to swap in a hardlink to a
	// root-owned file (a regular file, so it passes the open regular-file
	// check) between one line and the next. Only the very first open can be
	// raced this way, and it's checked (Nlink==1, single-link regular file)
	// before ever being cached; every later line reuses that already-open,
	// already-verified fd. SetLogPath and the /tmp/agent.log fallback both
	// close and nil this so the next line reopens against the new path.
	logFile *os.File
)

// Timestamp formats t the way sciontool stamps the log files it writes for
// the system (agent.log and the service lifecycle logs): a UTC RFC 3339
// instant with nanoseconds. The agent process keeps its own TZ for the
// workload; only the log written for the system is UTC, so lines from
// agents in different zones name the same instant the same way. The
// fraction is variable width (time.RFC3339Nano trims trailing zeros), so
// parse timestamps before comparing or ordering them; do not compare the
// strings.
func Timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// SetQuiet suppresses stderr log output (but preserves file logging).
// Used when running as a hook/status subprocess where stderr is captured by the host.
// It also silences the shared one-time level warnings (such as the
// SCION_DEBUG deprecation notice), which would otherwise repeat on every
// short-lived hook invocation.
func SetQuiet(enabled bool) {
	quiet.Store(enabled)
	if enabled {
		loglevel.SetWarningOutput(io.Discard)
	} else {
		loglevel.SetWarningOutput(os.Stderr)
	}
}

// Init initializes the logging system. It may be called more than once
// (each call re-installs the slog default handler), so lazy init in write()
// uses a double-checked lock rather than sync.Once.
func Init() {
	mu.Lock()
	defer mu.Unlock()
	initLocked()
}

// ensureInit runs the init logic (initLocked) once if nothing has initialized the package yet.
// Concurrent first callers serialize on mu; only the first one runs
// initLocked, the rest see initialized set and return.
func ensureInit() {
	if initialized.Load() {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if initialized.Load() {
		return
	}
	initLocked()
}

// initLocked does the work of Init. Callers must hold mu.
func initLocked() {
	initRuns.Add(1)

	if logPath == "" {
		// Priority 1: Check if /home/scion exists (standard agent home)
		if _, err := os.Stat("/home/scion"); err == nil {
			logPath = "/home/scion/agent.log"
		} else {
			// Priority 2: Use HOME env var
			home := os.Getenv("HOME")
			if home == "" {
				home = "/home/scion"
			}
			logPath = filepath.Join(home, "agent.log")
		}
	}

	// Levels come from the shared parser: SCION_LOG_LEVEL, or the
	// deprecated SCION_DEBUG alias. A --log-level flag applied later via
	// ApplyLogLevel takes precedence.
	syncLevelFromShared()

	// Set as default slog handler to capture all debug lines from shared packages
	slog.SetDefault(slog.New(newHandler()))

	initialized.Store(true)
}

// SetDebug enables or disables debug logging.
func SetDebug(enabled bool) {
	debug.Store(enabled)
}

// syncLevelFromShared sets the debug switch from the shared default level.
func syncLevelFromShared() {
	debug.Store(loglevel.DebugEnabled(""))
}

// ApplyLogLevel applies a --log-level flag value (a level spec such as
// "warn" or "info,hooks=debug") through the shared parser at flag
// precedence, so it overrides SCION_LOG_LEVEL and SCION_DEBUG. On a parse
// problem the fallback spec (info for an invalid default) is still applied
// and the error is returned for the caller to report.
func ApplyLogLevel(spec string) error {
	_, err := loglevel.ApplyString(spec, loglevel.SourceFlag)
	syncLevelFromShared()
	return err
}

// levelValue maps the level names used by write to slog levels.
func levelValue(level string) slog.Level {
	switch level {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// enabled reports whether a line at level for component (a tag or slog
// subsystem; may be empty) should be written. An explicit per-component
// level wins; otherwise debug lines follow the debug switch and the rest
// follow the shared default level.
func enabled(level slog.Level, component string) bool {
	if component != "" {
		if lvl, ok := loglevel.ComponentLevel(component); ok {
			return level >= lvl
		}
	}
	if level < slog.LevelInfo {
		return debug.Load()
	}
	return level >= loglevel.Effective("")
}

// Chown changes the ownership of the log file. It chowns the already-open
// cached fd (fchown) rather than the path, so a symlink or hardlink swapped
// in at logPath after the fact can't redirect it. If no line has been
// logged yet, there's no fd to chown; the next write() call opens and
// caches one under the current (already correct, post-drop) ownership
// path, so this is a no-op rather than a gap.
func Chown(uid, gid int) error {
	mu.Lock()
	defer mu.Unlock()
	if logFile == nil {
		return nil
	}
	return logFile.Chown(uid, gid)
}

// SetLogPath sets the path to the log file. Primarily for testing. Closes
// and drops any cached fd for the previous path so the next line opens
// (and re-validates) the new one.
//
// It does not mark the package initialized: if it is called before Init,
// the lazy init on the first log line still resolves the log level
// (SCION_LOG_LEVEL, or the deprecated SCION_DEBUG) and installs the slog
// default handler, and keeps this path (initLocked only picks a default
// path when none is set).
func SetLogPath(path string) {
	mu.Lock()
	defer mu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	logPath = path
}

// Info logs an informational message.
func Info(format string, args ...interface{}) {
	logf("INFO", "", format, args...)
}

// TaggedInfo logs an informational message with an additional tag. The tag
// is also a component name for per-component levels.
func TaggedInfo(tag string, format string, args ...interface{}) {
	logf("INFO", tag, format, args...)
}

// Error logs an error message.
func Error(format string, args ...interface{}) {
	logf("ERROR", "", format, args...)
}

// Warn logs a warning: something an operator should notice and act on, but
// that does not itself abort whatever operation triggered it (unlike Error,
// which this codebase's convention reserves for a failure the caller is
// already handling as one). Always emitted, the same as Error — never
// gated behind the debug level the way Debug is. Suppressed only when the
// level is explicitly raised to error.
func Warn(format string, args ...interface{}) {
	logf("WARN", "", format, args...)
}

// WarnAlways logs a warning that bypasses level filtering. It is for
// problems with the logging configuration itself (such as an invalid
// --log-level), which must be reported even when the level is raised to
// error. Quiet mode still suppresses the stderr copy.
func WarnAlways(format string, args ...interface{}) {
	write("WARN", "", format, args...)
}

// Debug logs a debug message if the debug level is enabled
// (SCION_LOG_LEVEL=debug, --log-level debug, or the deprecated SCION_DEBUG).
func Debug(format string, args ...interface{}) {
	if !debug.Load() {
		return
	}
	logf("DEBUG", "", format, args...)
}

// logf initializes the package if needed, applies level filtering and
// writes the line.
func logf(level, tag, format string, args ...interface{}) {
	ensureInit()
	if !enabled(levelValue(level), tag) {
		return
	}
	write(level, tag, format, args...)
}

func write(level, tag, format string, args ...interface{}) {
	ensureInit()

	timestamp := Timestamp(time.Now())
	message := fmt.Sprintf(format, args...)

	tagStr := ""
	if tag != "" {
		tagStr = fmt.Sprintf(" [%s]", tag)
	}

	// Format for agent.log: timestamp [sciontool] [LEVEL] [TAG] message
	fileEntry := fmt.Sprintf("%s [sciontool] [%s]%s %s\n", timestamp, level, tagStr, message)

	// Format for stderr: [sciontool] LEVEL: [TAG] message
	stderrEntry := fmt.Sprintf("[sciontool] %s:%s %s\n", level, tagStr, message)

	// Write to stderr (suppressed in quiet mode for hook/status subcommands)
	if !quiet.Load() {
		fmt.Fprint(os.Stderr, stderrEntry)
	}

	// Write to agent.log, through the cached fd (opened and validated once;
	// see logFile's doc comment).
	mu.Lock()
	f, err := getLogFileLocked()
	if err == nil {
		_, _ = f.WriteString(fileEntry)
	}
	mu.Unlock()
}

// getLogFileLocked returns the cached log file, opening (and validating)
// one against logPath if none is cached yet, falling back to
// /tmp/agent.log on failure exactly as the historical per-line open did.
// Callers must hold mu.
func getLogFileLocked() (*os.File, error) {
	if logFile != nil {
		return logFile, nil
	}

	// Use more permissive 0666 so that if created as root, it can be written to by others
	// (subject to directory permissions and umask).
	f, err := openLogFileNoFollow(logPath, 0666)
	if err == nil {
		logFile = f
		return logFile, nil
	}

	// If we can't write to agent.log, try to fall back to /tmp and enable debug
	if logPath == "/tmp/agent.log" {
		// Already at /tmp/agent.log and it failed
		return nil, err
	}

	debug.Store(true)
	oldPath := logPath
	logPath = "/tmp/agent.log"

	// Get system info for debugging
	uid := os.Getuid()
	gid := os.Getgid()
	username := "unknown"
	if u, uerr := user.Current(); uerr == nil {
		username = u.Username
	}
	sysInfo := fmt.Sprintf("UID=%d, GID=%d, USER=%s, HOME=%s, SCION_HOST_UID=%s, SCION_HOST_GID=%s",
		uid, gid, username, os.Getenv("HOME"), os.Getenv("SCION_HOST_UID"), os.Getenv("SCION_HOST_GID"))

	fallbackMsg := fmt.Sprintf("[sciontool] WARNING: Failed to write to %s: %v. Falling back to /tmp/agent.log and enabling debug mode. %s\n", oldPath, err, sysInfo)
	fmt.Fprint(os.Stderr, fallbackMsg)

	// Retry with new path
	f, err = openLogFileNoFollow(logPath, 0666)
	if err != nil {
		// Total failure
		return nil, err
	}
	logFile = f
	// Write the fallback message to the new log file too
	_, _ = logFile.WriteString(Timestamp(time.Now()) + " " + fallbackMsg)
	return logFile, nil
}

// openLogFileNoFollow opens path for appending, the same way the historical
// os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, mode) call did,
// except:
//   - it never follows a symlink at any component of path, not just the
//     final one (see the dirfd package doc comment) — a workload that owns
//     an intermediate directory such as $HOME can otherwise redirect the
//     open into an arbitrary directory even though the leaf itself is
//     never a symlink;
//   - O_NONBLOCK keeps a FIFO planted at path from blocking this open (and
//     therefore every later log line, since write() holds mu while this
//     runs) forever waiting for a reader that will never come;
//   - it refuses to write to anything that isn't a regular file with
//     exactly one link, so a hardlink to a root-owned file (which passes a
//     bare "is this a regular file" check, since a hardlink IS a regular
//     file) is refused instead of silently appended to.
//
// This process runs as root for its whole lifetime on Substrate (see
// fixupRootfsForScion's doc comment in cmd/sciontool/commands), and logPath
// normally lives under $HOME, which the scion user can write to. A symlink,
// hardlink, FIFO, or other non-regular entry here now surfaces as an
// open/stat error instead, which callers already treat the same way a
// missing/unwritable log path has always been treated: fall back to
// /tmp/agent.log, or (if that also fails) drop the line silently rather
// than block startup on logging.
func openLogFileNoFollow(path string, mode os.FileMode) (*os.File, error) {
	dirFd, leaf, err := dirfd.OpenParentNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = syscall.Close(dirFd) }()

	f, err := dirfd.OpenAt(dirFd, leaf,
		syscall.O_APPEND|syscall.O_CREAT|syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, mode)
	if err != nil {
		return nil, err
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 {
		_ = f.Close()
		return nil, fmt.Errorf("log path %s is not a single-link regular file", path)
	}
	return f, nil
}

// slogHandler implements slog.Handler by bridging to our write function.
type slogHandler struct {
	attrs []slog.Attr
	// subsystem is the value of a "subsystem" attr added via WithAttrs,
	// used for per-component levels.
	subsystem string
}

func newHandler() *slogHandler {
	return &slogHandler{}
}

func (h *slogHandler) Enabled(_ context.Context, level slog.Level) bool {
	ensureInit()
	if level < slog.LevelDebug {
		return false
	}
	if h.subsystem != "" {
		return enabled(level, h.subsystem)
	}
	// A record-level subsystem attribute may carry its own level, so admit
	// anything a configured component could want; Handle decides.
	if level >= loglevel.MinLevel().Level() {
		return true
	}
	return enabled(level, "")
}

func (h *slogHandler) Handle(_ context.Context, r slog.Record) error {
	sub := h.subsystem
	// A record-level subsystem attribute only matters when some component
	// has its own level.
	if sub == "" && loglevel.HasComponents() {
		subsystemScans.Add(1)
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "subsystem" {
				sub = a.Value.String()
				return false
			}
			return true
		})
	}
	if !enabled(r.Level, sub) {
		return nil
	}
	level := r.Level.String()
	msg := r.Message
	if r.NumAttrs() > 0 || len(h.attrs) > 0 {
		var buf []byte
		buf = append(buf, msg...)
		for _, a := range h.attrs {
			buf = append(buf, ' ')
			buf = append(buf, a.Key...)
			buf = append(buf, '=')
			buf = append(buf, a.Value.String()...)
		}
		r.Attrs(func(a slog.Attr) bool {
			buf = append(buf, ' ')
			buf = append(buf, a.Key...)
			buf = append(buf, '=')
			buf = append(buf, a.Value.String()...)
			return true
		})
		msg = string(buf)
	}
	write(level, "slog", "%s", msg)
	return nil
}

func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &slogHandler{attrs: append(h.attrs[:len(h.attrs):len(h.attrs)], attrs...), subsystem: h.subsystem}
	for _, a := range attrs {
		if a.Key == "subsystem" {
			next.subsystem = a.Value.String()
		}
	}
	return next
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	// Not implemented
	return h
}
