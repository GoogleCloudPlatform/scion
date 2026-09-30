/*
Copyright 2025 The Scion Authors.
*/

// Package procreap is the single, dependency-free home for the managed-PID
// reaper primitives used by every package that runs exec.Cmd subprocesses
// from inside sciontool init's PID-1 process: pkg/sciontool/supervisor (the
// supervised harness child), cmd/sciontool/commands (git subprocesses,
// privilege-drop helpers), pkg/sciontool/hooks (lifecycle hook scripts),
// pkg/sciontool/services (managed services), and pkg/sciontool/metadata
// (iptables setup). It has no internal scion dependencies beyond log, so any
// of those packages can import it without creating an import cycle — several
// of them (e.g. supervisor and hooks) already depend on each other.
package procreap
