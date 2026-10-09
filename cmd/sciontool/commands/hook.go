/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"go.opentelemetry.io/otel"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	hookDialect string
	hookData    string
)

// hookCmd represents the hook command
var hookCmd = &cobra.Command{
	Use:   "hook [event]",
	Short: "Process harness hook events",
	Long: `The hook command processes events from agent harnesses (Claude Code, Gemini CLI, Codex).

It normalizes events from different harness formats (dialects) and updates agent
status, logs events, and performs other hook-related actions.

Events are received via stdin as JSON data. The --dialect flag specifies which
harness format to use for parsing.

Examples:
  # Process a Claude Code event from stdin
  echo '{"hook_event_name": "PreToolUse", "tool_name": "Bash"}' | sciontool hook --dialect=claude

  # Process a Gemini CLI event
  echo '{"hook_event_name": "BeforeTool", "tool_name": "shell"}' | sciontool hook --dialect=gemini

  # Use the ask_user subcommand
  sciontool hook ask_user "What should I do next?"

  # Use the task_completed subcommand
  sciontool hook task_completed "Implemented feature X"`,
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) > 0 {
			// Handle subcommands: ask_user, task_completed
			switch args[0] {
			case "ask_user":
				message := "Input requested"
				if hookData != "" {
					message = hookData
				}
				runAskUser(message)
				return
			case "task_completed":
				message := "Task completed"
				if hookData != "" {
					message = hookData
				}
				runTaskCompleted(message)
				return
			default:
				// Treat as event name (for legacy compatibility)
				runHookWithEvent(args[0])
				return
			}
		}

		// Default: process JSON from stdin
		if err := runHookFromStdin(); err != nil {
			log.Error("Hook processing failed: %v", err)
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(hookCmd)

	hookCmd.Flags().StringVar(&hookDialect, "dialect", "claude",
		"Harness dialect for event parsing (claude, gemini, codex)")
	hookCmd.Flags().StringVar(&hookData, "data", "",
		"Additional data for subcommands")

	// Add subcommands for direct invocation
	hookCmd.AddCommand(askUserCmd)
	hookCmd.AddCommand(taskCompletedCmd)
}

// askUserCmd represents the ask_user subcommand
var askUserCmd = &cobra.Command{
	Use:   "ask_user [message]",
	Short: "Signal that the agent is waiting for user input",
	Args:  cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		message := "Input requested"
		if len(args) > 0 {
			message = args[0]
		}
		runAskUser(message)
	},
}

// taskCompletedCmd represents the task_completed subcommand
var taskCompletedCmd = &cobra.Command{
	Use:   "task_completed [message]",
	Short: "Signal that the agent has completed its task",
	Args:  cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		message := "Task completed"
		if len(args) > 0 {
			message = args[0]
		}
		runTaskCompleted(message)
	},
}

// runHookFromStdin processes hook events from stdin.
func runHookFromStdin() error {
	// Check if stdin has data
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		// Stdin is a terminal, no data to process
		return nil
	}

	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}

	if len(data) == 0 {
		return nil
	}

	return processHookData(data)
}

// runHookWithEvent creates and processes a synthetic event.
func runHookWithEvent(eventName string) {
	data := map[string]interface{}{
		"hook_event_name": eventName,
	}
	jsonData, _ := json.Marshal(data)
	if err := processHookData(jsonData); err != nil {
		log.Error("Hook processing failed: %v", err)
		os.Exit(1)
	}
}

// hookHubBudget bounds the total time one hook process spends on Hub
// calls: the status update, the limits count report, and the session-end
// metrics report all share it. Each call is also capped on its own, but the
// caps used to add up (10-15s with a black-holed Hub) past harness hook
// timeouts (antigravity 10s; opencode and grok-build 5s). Together with
// telemetry's own bound (about 0.75s) this keeps a hook under 5s.
// The calls run in order, so a slow earlier call (the status update) can use
// up the budget and the later ones (limits counts, session-end metrics) are
// then dropped. That is acceptable: those are informational, and
// limits_exceeded and the final stop status are reported by sciontool init.
const hookHubBudget = 3 * time.Second

// shutdownSessionReportTimeout bounds the Hub call with which sciontool init
// reports a session still open at shutdown (reportOpenSessionAtShutdown,
// session_backstop.go): the session-end metrics report that a hook process
// never made because session-end did not fire. It is the init-side
// counterpart of hookHubBudget. Together with the session state lock wait
// (2s) it keeps that backstop well inside the runtime's default 10s stop
// grace period. A variable only so tests can shorten it.
var shutdownSessionReportTimeout = 3 * time.Second

// processHookData parses and handles hook event data.
func processHookData(data []byte) error {
	var rawData map[string]interface{}
	if err := json.Unmarshal(data, &rawData); err != nil {
		return fmt.Errorf("parsing JSON: %w", err)
	}

	// One Hub client and one deadline for every Hub call this process makes.
	hubBudget, cancelHubBudget := context.WithTimeout(context.Background(), hookHubBudget)
	defer cancelHubBudget()
	hubClient := hub.NewClient()

	// Create processor with handlers
	processor := hooks.NewHarnessProcessor()

	// Register built-in dialects first, then let a harness-bundled dialect.yaml
	// override the requested dialect by name if one is present.
	dialects.RegisterBuiltins(processor)
	var mappingDialect *dialects.MappingDialect
	if md, err := dialects.DiscoverMappingDialect(hookDialect); err == nil {
		processor.RegisterDialect(md)
		mappingDialect = md
	}

	// Register handlers
	statusHandler := handlers.NewStatusHandler()
	loggingHandler := handlers.NewLoggingHandler()
	promptHandler := handlers.NewPromptHandler()
	hubHandler := handlers.NewHubHandlerForClient(hubClient).WithBudget(hubBudget)
	limitsHandler := handlers.NewLimitsHandler(hubHandler)

	processor.AddHandler(statusHandler.Handle)
	processor.AddHandler(loggingHandler.Handle)
	processor.AddHandler(promptHandler.Handle)

	// Add Hub handler if configured
	if hubHandler != nil {
		processor.AddHandler(hubHandler.Handle)
	}

	// Add limits handler if any limits are configured
	if limitsHandler != nil {
		processor.AddHandler(limitsHandler.Handle)
	}

	event, err := processor.ParseRaw(rawData, hookDialect)
	if err != nil {
		return err
	}
	if err := processor.Dispatch(event); err != nil {
		return err
	}

	// Emit the response JSON that the harness expects on stdout BEFORE any
	// telemetry export. The response depends only on the dialect, and some
	// harnesses (antigravity: 10s) kill a hook that has not answered in time,
	// which fails the tool call. Telemetry must never delay the answer.
	if err := writeHookResponse(hookStdout, mappingDialect, rawData); err != nil {
		return err
	}

	runHookTelemetry(hubBudget, event, hubClient)
	return nil
}

// hookStdout is where hook responses are written. Tests replace it to
// observe when the response is written relative to telemetry export.
var hookStdout io.Writer = os.Stdout

// writeHookResponse writes the response declared by the dialect for this
// event, if any. Dialects that don't declare a responses section (e.g.
// claude) produce no output, preserving backward compatibility. os.Stdout is
// unbuffered, so the response reaches the harness as soon as this returns.
func writeHookResponse(w io.Writer, mappingDialect *dialects.MappingDialect, rawData map[string]interface{}) error {
	if mappingDialect == nil {
		return nil
	}
	rawEventName := mappingDialect.EventName(rawData)
	if rawEventName == "" {
		return nil
	}
	resp := mappingDialect.Response(rawEventName)
	if resp == nil {
		return nil
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return fmt.Errorf("writing hook response: %w", err)
	}
	return nil
}

// hookOTelErrorHandler logs OTel SDK errors (failed exports) at debug level.
var hookOTelErrorHandler = otel.ErrorHandlerFunc(func(err error) {
	log.Debug("Hook telemetry export dropped: %v", err)
})

// runHookTelemetry exports the hook event as telemetry to the loopback
// receiver, if telemetry is enabled. It runs after the response is written
// and is best effort: exports are bounded by telemetry.HookExportTimeout and
// telemetry.HookShutdownTimeout, are not retried, and a failure is logged at
// debug level only. It never writes to stdout and never fails the hook.
// When the Hub client is configured it also persists session counts and, on
// session-end, reports the session summary to the Hub (bounded by
// sessionMetricsReportTimeout and by hubBudget, the hook's shared Hub
// deadline).
func runHookTelemetry(hubBudget context.Context, event *hooks.Event, hubClient *hub.Client) {
	cfg := telemetry.LoadConfig()
	if cfg == nil || !cfg.Enabled {
		return
	}

	// The OTel SDK reports failed exports to its global error handler, which
	// by default logs to stderr. In the hook a missing receiver is expected
	// (for example when the pipeline failed to start), so keep it at debug.
	// This is process-wide and deliberately not restored: otel pins the
	// default handler's delegate to the first handler ever set, so a restore
	// would not take effect anyway. The hook subcommands run in their own
	// short-lived process, so sciontool init is unaffected.
	otel.SetErrorHandler(hookOTelErrorHandler)

	redactor := telemetry.NewRedactor(cfg.Redaction)

	// Synchronous, bounded providers for the short-lived hook process.
	ctx := context.Background()
	providers, err := telemetry.NewHookProviders(ctx, cfg)
	if err != nil {
		log.Error("Failed to create telemetry providers: %v", err)
	}
	if providers != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), telemetry.HookShutdownTimeout)
			defer cancel()
			if err := providers.Shutdown(shutdownCtx); err != nil {
				log.Debug("Hook telemetry shutdown incomplete: %v", err)
			}
		}()
	}

	var tp trace.TracerProvider
	var lp otellog.LoggerProvider
	var mp metric.MeterProvider
	if providers != nil {
		tp = providers.TracerProvider
		lp = providers.LoggerProvider
		if providers.MeterProvider != nil {
			mp = providers.MeterProvider
		}
	}

	telemetryHandler := handlers.NewTelemetryHandler(tp, lp, redactor, mp)
	wireSessionMetrics(hubBudget, telemetryHandler, hubClient, hookHomeDir())
	if err := telemetryHandler.Handle(event); err != nil {
		log.Debug("Hook telemetry handler: %v", err)
	}
}

// runAskUser updates status to waiting for input.
func runAskUser(message string) {
	statusHandler := handlers.NewStatusHandler()
	loggingHandler := handlers.NewLoggingHandler()
	hubHandler := handlers.NewHubHandler()

	// Update activity to waiting_for_input (sticky)
	if err := statusHandler.UpdateActivity(state.ActivityWaitingForInput, ""); err != nil {
		log.Error("Failed to update status: %v", err)
	}

	// Log the event
	logMessage := fmt.Sprintf("Agent requested input: %s", message)
	if err := loggingHandler.LogEvent(string(state.ActivityWaitingForInput), logMessage); err != nil {
		log.Error("Failed to log event: %v", err)
	}

	// Send status to Hub
	if hubHandler != nil {
		if err := hubHandler.ReportWaitingForInput(message); err != nil {
			log.Error("Failed to report to Hub: %v", err)
		}
	}

	fmt.Fprintf(os.Stderr, "[sciontool] Agent asked: %s\n", message)
}

// runTaskCompleted updates status to completed.
func runTaskCompleted(message string) {
	statusHandler := handlers.NewStatusHandler()
	loggingHandler := handlers.NewLoggingHandler()
	hubHandler := handlers.NewHubHandler()

	// Update activity to completed (sticky)
	if err := statusHandler.UpdateActivity(state.ActivityCompleted, ""); err != nil {
		log.Error("Failed to update status: %v", err)
	}

	// Log the event
	logMessage := fmt.Sprintf("Agent completed task: %s", message)
	if err := loggingHandler.LogEvent(string(state.ActivityCompleted), logMessage); err != nil {
		log.Error("Failed to log event: %v", err)
	}

	// Send status to Hub
	if hubHandler != nil {
		if err := hubHandler.ReportTaskCompleted(message); err != nil {
			log.Error("Failed to report to Hub: %v", err)
		}
	}

	fmt.Fprintf(os.Stderr, "[sciontool] Agent completed: %s\n", message)
}

// sessionMetricsReportTimeout bounds the session summary report a hook
// process sends to the Hub on session-end.
const sessionMetricsReportTimeout = 5 * time.Second

// wireSessionMetrics connects a hook process's telemetry handler to Hub
// session-metrics reporting. Each hook event runs in a new process, so the
// session's counts are kept in a state file under home between events (see
// handlers.FileSessionState), and the process that handles session-end
// reports the summary. The report's deadline is derived from parent (the
// hook's shared Hub budget). Nothing is wired when the Hub client is not
// configured or no home directory is known.
func wireSessionMetrics(parent context.Context, h *handlers.TelemetryHandler, client *hub.Client, home string) {
	if h == nil || client == nil || !client.IsConfigured() || home == "" {
		return
	}
	h.SessionState = handlers.NewFileSessionState(home)
	h.OnSessionEnd = func(summary telemetry.SessionSummary) {
		ctx, cancel := context.WithTimeout(parent, sessionMetricsReportTimeout)
		defer cancel()
		if err := client.ReportMetrics(ctx, hub.SummaryToMetricsPayload(summary)); err != nil {
			log.Error("Failed to report session metrics to hub: %v", err)
			return
		}
		log.Info("Session metrics reported to hub for session %s", summary.SessionID)
	}
}

// hookHomeDir returns the agent's home directory for hook state files.
func hookHomeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	home, _ := os.UserHomeDir()
	return home
}
