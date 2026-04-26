/*
Copyright 2025 The Scion Authors.
*/

package hooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// LifecycleManager handles Scion lifecycle hooks.
// These are container-level events managed by sciontool init.
type LifecycleManager struct {
	// HooksDirs is the ordered list of directories containing hook scripts.
	// Discovery walks each directory in order; per-agent staged hooks (e.g.
	// $HOME/.scion/hooks for container-script harnesses) are appended after
	// the system default and run after the system hooks.
	HooksDirs []string

	// Handlers are the registered handlers for lifecycle events.
	Handlers map[string][]Handler
}

// NewLifecycleManager creates a new lifecycle manager.
//
// The default discovery list is built from $SCION_HOOKS_DIR (colon-separated;
// each entry is honored in order) and falls back to /etc/scion/hooks. Callers
// can append per-agent hook directories with AddHooksDir.
func NewLifecycleManager() *LifecycleManager {
	return &LifecycleManager{
		HooksDirs: defaultHooksDirs(),
		Handlers:  make(map[string][]Handler),
	}
}

func defaultHooksDirs() []string {
	if envDir := os.Getenv("SCION_HOOKS_DIR"); envDir != "" {
		var dirs []string
		for _, d := range strings.Split(envDir, ":") {
			d = strings.TrimSpace(d)
			if d != "" {
				dirs = append(dirs, d)
			}
		}
		if len(dirs) > 0 {
			return dirs
		}
	}
	return []string{"/etc/scion/hooks"}
}

// AddHooksDir appends a hooks directory to the discovery list. Per-agent
// hook directories (e.g. $HOME/.scion/hooks for container-script harnesses)
// are appended after the system default so system hooks run first. Duplicate
// paths are ignored.
func (m *LifecycleManager) AddHooksDir(dir string) {
	if dir == "" {
		return
	}
	for _, d := range m.HooksDirs {
		if d == dir {
			return
		}
	}
	m.HooksDirs = append(m.HooksDirs, dir)
}

// RegisterHandler adds a handler for a lifecycle event.
func (m *LifecycleManager) RegisterHandler(eventName string, handler Handler) {
	m.Handlers[eventName] = append(m.Handlers[eventName], handler)
}

// RunPreStart executes pre-start lifecycle hooks.
// Called after container setup but before the child process starts.
func (m *LifecycleManager) RunPreStart() error {
	event := &Event{
		Name: EventPreStart,
	}
	return m.runHooks(event)
}

// RunPostStart executes post-start lifecycle hooks.
// Called after the child process is confirmed running.
func (m *LifecycleManager) RunPostStart() error {
	event := &Event{
		Name: EventPostStart,
	}
	return m.runHooks(event)
}

// RunPreStop executes pre-stop lifecycle hooks.
// Called when a termination signal (SIGTERM/SIGINT) is received,
// before starting the graceful shutdown process.
func (m *LifecycleManager) RunPreStop() error {
	event := &Event{
		Name: EventPreStop,
	}
	return m.runHooks(event)
}

// RunSessionEnd executes session-end lifecycle hooks.
// Called on graceful shutdown before child termination.
func (m *LifecycleManager) RunSessionEnd() error {
	event := &Event{
		Name: EventSessionEnd,
	}
	return m.runHooks(event)
}

// runHooks executes both script-based and registered handlers for an event.
func (m *LifecycleManager) runHooks(event *Event) error {
	var errs []string

	// Run script-based hooks
	if err := m.runScriptHooks(event.Name); err != nil {
		errs = append(errs, fmt.Sprintf("script hooks: %v", err))
	}

	// Run registered handlers
	if handlers, ok := m.Handlers[event.Name]; ok {
		for _, handler := range handlers {
			if err := handler(event); err != nil {
				errs = append(errs, fmt.Sprintf("handler: %v", err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("hook errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// runScriptHooks looks for and executes script files across all configured
// hooks directories in order. Within each directory, single-file hooks
// (eventName, eventName.sh) execute before the corresponding eventName.d/
// directory entries (sorted alphabetically).
func (m *LifecycleManager) runScriptHooks(eventName string) error {
	for _, dir := range m.HooksDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}

		// Single-file hooks (legacy form)
		patterns := []string{
			filepath.Join(dir, eventName),
			filepath.Join(dir, eventName+".sh"),
		}
		for _, pattern := range patterns {
			if info, err := os.Stat(pattern); err == nil && !info.IsDir() {
				if err := m.executeScript(pattern); err != nil {
					return fmt.Errorf("script %s: %w", pattern, err)
				}
			}
		}

		// .d directory with multiple scripts
		dirPath := filepath.Join(dir, eventName+".d")
		if info, err := os.Stat(dirPath); err == nil && info.IsDir() {
			entries, err := os.ReadDir(dirPath)
			if err != nil {
				return fmt.Errorf("reading hooks dir %s: %w", dirPath, err)
			}
			// os.ReadDir already returns entries sorted by name, but be
			// explicit: stable lexical order matters because scripts use the
			// numeric prefix convention (10-foo, 20-harness-provision).
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].Name() < entries[j].Name()
			})
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				scriptPath := filepath.Join(dirPath, entry.Name())
				if err := m.executeScript(scriptPath); err != nil {
					return fmt.Errorf("script %s: %w", scriptPath, err)
				}
			}
		}
	}

	return nil
}

// executeScript runs a hook script.
func (m *LifecycleManager) executeScript(path string) error {
	// Check if executable
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&0111 == 0 {
		// Not executable, skip with warning
		fmt.Fprintf(os.Stderr, "[sciontool] Warning: hook script %s is not executable, skipping\n", path)
		return nil
	}

	cmd := exec.Command(path)
	cmd.Stdout = os.Stderr // Redirect hook output to stderr
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("execution failed: %w", err)
	}
	return nil
}
