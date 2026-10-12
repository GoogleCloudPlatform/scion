package sub

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
)

type service struct{ logger *slog.Logger }

// Start logs through a package-level slog function.
func Start() { slog.Info("start") }

func (s *service) stop() { s.logger.Error("stop", "err", fmt.Errorf("x")) }

func legacy() { log.Printf("legacy %d", 1) }

func viaDefault() { slog.Default().Warn("w") }

func stdLogger(l *log.Logger) { l.Println("x") }

func quiet() string { return fmt.Sprint("no logging") }

func closure() func() { return func() { slog.Debug("in closure") } }

var onError = func(err error) { slog.Error("failed", "err", err) }

var plain = os.Getpid

// logf logs from wherever it is called: a func value of log.Printf.
var logf = log.Printf

// buildOnly only builds attributes and loggers: no record is emitted, so no WARN.
func buildOnly() *slog.Logger {
	attr := slog.String("k", "v")
	l := slog.New(slog.NewTextHandler(os.Stderr, nil)).With(attr).WithGroup("g")
	_ = l.Enabled(context.Background(), slog.LevelInfo)
	_ = l.Handler()
	_ = log.Default().Writer()
	return slog.Default().With(slog.Int("n", 1))
}

// valueOf passes emitting funcs and a method value along.
func valueOf(l *slog.Logger) []func(string, ...any) {
	f := slog.Error
	return []func(string, ...any){f, l.Info}
}

// Wire uses every function so the fixture compiles.
func Wire() {
	s := &service{logger: slog.Default()}
	s.stop()
	legacy()
	viaDefault()
	stdLogger(log.Default())
	_ = quiet()
	closure()()
	onError(nil)
	_ = plain
	logf("x")
	_ = buildOnly()
	_ = valueOf(slog.Default())
}
