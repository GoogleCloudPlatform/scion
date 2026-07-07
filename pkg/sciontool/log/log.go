/*
Copyright 2025 The Scion Authors.
*/

package log

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

var (
	logPath     string
	debug       bool
	quiet       atomic.Bool
	mu          sync.Mutex
	initialized bool
)

// SetQuiet suppresses stderr log output (but preserves file logging).
// Used when running as a hook/status subprocess where stderr is captured by the host.
func SetQuiet(enabled bool) {
	quiet.Store(enabled)
}

// Init initializes the logging system.
func Init() {
	mu.Lock()
	defer mu.Unlock()

	// If already initialized, we might still want to re-init slog if logPath changed
	// but for now let's just allow re-setting slog default to our handler

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

	if os.Getenv("SCION_DEBUG") != "" {
		debug = true
	}

	// Set as default slog handler to capture all debug lines from shared packages
	slog.SetDefault(slog.New(newHandler()))

	initialized = true
}

// SetDebug enables or disables debug logging.
func SetDebug(enabled bool) {
	mu.Lock()
	defer mu.Unlock()
	debug = enabled
}

// Chown changes the ownership of the log file.
func Chown(uid, gid int) error {
	mu.Lock()
	defer mu.Unlock()
	if logPath == "" {
		return nil
	}
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		return nil
	}
	return os.Chown(logPath, uid, gid)
}

// SetLogPath sets the path to the log file. Primarily for testing.
func SetLogPath(path string) {
	mu.Lock()
	defer mu.Unlock()
	logPath = path
	initialized = true // Consider it initialized if path is explicitly set
}

// Info logs an informational message.
func Info(format string, args ...interface{}) {
	write("INFO", "", format, args...)
}

// TaggedInfo logs an informational message with an additional tag.
func TaggedInfo(tag string, format string, args ...interface{}) {
	write("INFO", tag, format, args...)
}

// Error logs an error message.
func Error(format string, args ...interface{}) {
	write("ERROR", "", format, args...)
}

// Debug logs a debug message if SCION_DEBUG is set.
func Debug(format string, args ...interface{}) {
	if !debug {
		return
	}
	write("DEBUG", "", format, args...)
}

func write(level, tag, format string, args ...interface{}) {
	if !initialized {
		Init()
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05")
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

	// Write to agent.log
	mu.Lock()
	// Use more permissive 0666 so that if created as root, it can be written to by others
	// (subject to directory permissions and umask).
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		// If we can't write to agent.log, try to fall back to /tmp and enable debug
		if logPath != "/tmp/agent.log" {
			debug = true
			oldPath := logPath
			logPath = "/tmp/agent.log"

			// Get system info for debugging
			uid := os.Getuid()
			gid := os.Getgid()
			username := "unknown"
			if u, err := user.Current(); err == nil {
				username = u.Username
			}
			sysInfo := fmt.Sprintf("UID=%d, GID=%d, USER=%s, HOME=%s, SCION_HOST_UID=%s, SCION_HOST_GID=%s",
				uid, gid, username, os.Getenv("HOME"), os.Getenv("SCION_HOST_UID"), os.Getenv("SCION_HOST_GID"))

			fallbackMsg := fmt.Sprintf("[sciontool] WARNING: Failed to write to %s: %v. Falling back to /tmp/agent.log and enabling debug mode. %s\n", oldPath, err, sysInfo)
			fmt.Fprint(os.Stderr, fallbackMsg)

			// Retry with new path
			f, err = os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
			if err != nil {
				// Total failure
				mu.Unlock()
				return
			}
			// Write the fallback message to the new log file too
			_, _ = f.WriteString(timestamp + " " + fallbackMsg)
		} else {
			// Already at /tmp/agent.log and it failed
			mu.Unlock()
			return
		}
	}
	_, _ = f.WriteString(fileEntry)
	_ = f.Close()
	mu.Unlock()
}

// slogHandler implements slog.Handler by bridging to our write function.
type slogHandler struct {
	attrs []slog.Attr
}

func newHandler() *slogHandler {
	return &slogHandler{}
}

func (h *slogHandler) Enabled(_ context.Context, level slog.Level) bool {
	if level >= slog.LevelError {
		return true
	}
	if level >= slog.LevelInfo {
		return true
	}
	if level >= slog.LevelDebug {
		mu.Lock()
		d := debug
		mu.Unlock()
		return d
	}
	return false
}

func (h *slogHandler) Handle(_ context.Context, r slog.Record) error {
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
	return &slogHandler{attrs: append(h.attrs, attrs...)}
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	// Not implemented
	return h
}
