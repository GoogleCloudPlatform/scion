/*
Copyright 2026 The Scion Authors.
*/

package log

import (
	stdlog "log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// resetUninitializedForTest puts the package back into its pre-Init state,
// with logPath pre-set to a temp file so the lazy Init never touches $HOME,
// and restores the previous state when the test ends.
func resetUninitializedForTest(t *testing.T) {
	t.Helper()
	mu.Lock()
	origPath, origFile := logPath, logFile
	origInitialized, origDebug, origQuiet := initialized.Load(), debug.Load(), quiet.Load()
	origDefault := slog.Default()
	// slog.SetDefault also redirects the std log package, so save its
	// writer and flags too and restore them after slog.SetDefault below.
	origStdWriter, origStdFlags := stdlog.Writer(), stdlog.Flags()
	logPath = filepath.Join(t.TempDir(), "agent.log")
	logFile = nil
	initialized.Store(false)
	debug.Store(false)
	quiet.Store(true)
	initRuns.Store(0)
	mu.Unlock()
	t.Setenv("SCION_DEBUG", "")

	t.Cleanup(func() {
		mu.Lock()
		if logFile != nil {
			_ = logFile.Close()
		}
		logPath, logFile = origPath, origFile
		initialized.Store(origInitialized)
		debug.Store(origDebug)
		quiet.Store(origQuiet)
		mu.Unlock()
		slog.SetDefault(origDefault)
		stdlog.SetOutput(origStdWriter)
		stdlog.SetFlags(origStdFlags)
	})
}

// fanOut runs fn on n goroutines released at the same moment.
func fanOut(n int, fn func(i int)) {
	var start, done sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			fn(i)
		}(i)
	}
	start.Done()
	done.Wait()
}

// TestWrite_ConcurrentBeforeInit logs from many goroutines before any
// explicit Init; under -race this must not report a race on the lazy-init
// state.
func TestWrite_ConcurrentBeforeInit(t *testing.T) {
	resetUninitializedForTest(t)
	fanOut(32, func(i int) {
		Info("line %d", i)
		Warn("line %d", i)
	})
	if !initialized.Load() {
		t.Fatal("expected lazy init to have run")
	}
}

// TestWrite_ConcurrentFirstCallsInitOnce checks that concurrent first log
// calls run the lazy init exactly once. It relies on -race for a
// deterministic signal (the race-detection CI workflow runs this package).
func TestWrite_ConcurrentFirstCallsInitOnce(t *testing.T) {
	resetUninitializedForTest(t)
	fanOut(32, func(i int) { Error("line %d", i) })
	if got := initRuns.Load(); got != 1 {
		t.Fatalf("init ran %d times, want 1", got)
	}
}

// TestDebug_RacesWithSetDebug toggles the debug flag while other goroutines
// call Debug and the slog handler's Enabled; -race must stay quiet.
func TestDebug_RacesWithSetDebug(t *testing.T) {
	resetUninitializedForTest(t)
	SetLogPath(logPath)
	h := newHandler()
	fanOut(32, func(i int) {
		if i%2 == 0 {
			SetDebug(i%4 == 0)
			return
		}
		Debug("line %d", i)
		_ = h.Enabled(t.Context(), slog.LevelDebug)
	})
}

// TestSetLogPathBeforeInitStillRunsLazyInit pins ptone/scion#2657: calling
// SetLogPath before any Init must not skip the lazy init's SCION_DEBUG read
// and slog default handler installation, and the lazy init must keep the
// explicitly set path.
func TestSetLogPathBeforeInitStillRunsLazyInit(t *testing.T) {
	resetUninitializedForTest(t)
	t.Setenv("SCION_DEBUG", "1")
	path := filepath.Join(t.TempDir(), "explicit.log")
	slog.SetDefault(slog.New(slog.DiscardHandler))

	SetLogPath(path)
	if initialized.Load() {
		t.Fatal("SetLogPath must not mark the package initialized")
	}
	Info("first line")

	if got := initRuns.Load(); got != 1 {
		t.Fatalf("lazy init ran %d times, want 1", got)
	}
	if !debug.Load() {
		t.Fatal("lazy init did not read SCION_DEBUG")
	}
	if _, ok := slog.Default().Handler().(*slogHandler); !ok {
		t.Fatalf("slog default handler = %T, want *slogHandler", slog.Default().Handler())
	}
	mu.Lock()
	gotPath := logPath
	mu.Unlock()
	if gotPath != path {
		t.Fatalf("logPath = %q, want %q", gotPath, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "first line") {
		t.Fatalf("log file = %q, want it to contain the first line", data)
	}
}
