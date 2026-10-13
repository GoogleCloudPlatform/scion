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

package plugin

import (
	"io"
	"log"
	"log/slog"

	hclog "github.com/hashicorp/go-hclog"
)

// hclogAdapter wraps slog.Logger to implement hashicorp/go-hclog.Logger.
// This allows go-plugin to use scion's existing slog-based logging.
type hclogAdapter struct {
	logger *slog.Logger
	name   string
	args   []interface{}

	// exitExpected reports whether the plugin's owner is shutting down,
	// so a plugin process exit is expected. May be nil.
	exitExpected func() bool
}

// newHclogAdapter returns an hclog.Logger that writes to logger.
// exitExpected (may be nil) reports whether a plugin process exit is
// expected; see processExitLevel.
func newHclogAdapter(logger *slog.Logger, exitExpected func() bool) hclog.Logger {
	return &hclogAdapter{logger: logger, exitExpected: exitExpected}
}

// goPluginProcessExitedMsg is the message go-plugin logs when a plugin
// process ends: at ERROR when the process exited with an error (including
// a signal), at INFO otherwise.
const goPluginProcessExitedMsg = "plugin process exited"

// processExitLevel is the level for go-plugin's ERROR "plugin process
// exited" record (ptone/scion#4869). An exit while the owner shuts down is
// expected and logs at INFO: the process was stopped by Manager.Shutdown,
// or by the same SIGTERM that stopped the hub, which service managers
// such as systemd send to every process of the service. An exit caused by
// SIGTERM or SIGINT at any other time is a stop requested from outside
// the hub, not a plugin crash, and logs at WARN. Every other exit stays at
// ERROR.
func (h *hclogAdapter) processExitLevel(args []interface{}) hclog.Level {
	if h.exitExpected != nil && h.exitExpected() {
		return hclog.Info
	}
	for i := 0; i+1 < len(args); i += 2 {
		if k, _ := args[i].(string); k != "error" {
			continue
		}
		var text string
		switch v := args[i+1].(type) {
		case string:
			text = v
		case error:
			text = v.Error()
		}
		if text == "signal: terminated" || text == "signal: interrupt" {
			return hclog.Warn
		}
	}
	return hclog.Error
}

func (h *hclogAdapter) Log(level hclog.Level, msg string, args ...interface{}) {
	allArgs := append(h.args, args...)
	if level == hclog.Error && msg == goPluginProcessExitedMsg {
		level = h.processExitLevel(allArgs)
	}
	switch level {
	case hclog.Trace, hclog.Debug:
		h.logger.Debug(msg, allArgs...)
	case hclog.Info:
		h.logger.Info(msg, allArgs...)
	case hclog.Warn:
		h.logger.Warn(msg, allArgs...)
	case hclog.Error:
		h.logger.Error(msg, allArgs...)
	}
}

func (h *hclogAdapter) Trace(msg string, args ...interface{}) {
	h.Log(hclog.Trace, msg, args...)
}

func (h *hclogAdapter) Debug(msg string, args ...interface{}) {
	h.Log(hclog.Debug, msg, args...)
}

func (h *hclogAdapter) Info(msg string, args ...interface{}) {
	h.Log(hclog.Info, msg, args...)
}

func (h *hclogAdapter) Warn(msg string, args ...interface{}) {
	h.Log(hclog.Warn, msg, args...)
}

func (h *hclogAdapter) Error(msg string, args ...interface{}) {
	h.Log(hclog.Error, msg, args...)
}

func (h *hclogAdapter) IsTrace() bool { return false }
func (h *hclogAdapter) IsDebug() bool { return true }
func (h *hclogAdapter) IsInfo() bool  { return true }
func (h *hclogAdapter) IsWarn() bool  { return true }
func (h *hclogAdapter) IsError() bool { return true }

func (h *hclogAdapter) ImpliedArgs() []interface{} {
	return h.args
}

func (h *hclogAdapter) With(args ...interface{}) hclog.Logger {
	return &hclogAdapter{
		logger:       h.logger,
		name:         h.name,
		args:         append(h.args, args...),
		exitExpected: h.exitExpected,
	}
}

func (h *hclogAdapter) Name() string {
	return h.name
}

func (h *hclogAdapter) Named(name string) hclog.Logger {
	newName := name
	if h.name != "" {
		newName = h.name + "." + name
	}
	return &hclogAdapter{
		logger:       h.logger.With("subsystem", newName),
		name:         newName,
		args:         h.args,
		exitExpected: h.exitExpected,
	}
}

func (h *hclogAdapter) ResetNamed(name string) hclog.Logger {
	return &hclogAdapter{
		logger:       h.logger.With("subsystem", name),
		name:         name,
		args:         h.args,
		exitExpected: h.exitExpected,
	}
}

func (h *hclogAdapter) SetLevel(hclog.Level)  {}
func (h *hclogAdapter) GetLevel() hclog.Level { return hclog.Debug }
func (h *hclogAdapter) StandardLogger(opts *hclog.StandardLoggerOptions) *log.Logger {
	return log.Default()
}
func (h *hclogAdapter) StandardWriter(opts *hclog.StandardLoggerOptions) io.Writer {
	return io.Discard
}
