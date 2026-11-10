/*
Copyright 2026 The Scion Authors.
*/

// Package netcap removes CAP_NET_ADMIN from sciontool init (PID 1) and
// from every process it starts afterwards.
//
// init sets up metadata interception as root and then no longer needs
// network administration. DropNetAdmin clears the capability from the
// bounding, ambient, inheritable, permitted and effective sets of every
// thread in the process, so processes started afterwards (including
// setuid-root programs, which can only gain capabilities that are still in
// the bounding set) do not have it. It then reads every thread's sets back
// from /proc and fails if any thread still holds it.
//
// Errors name only the capability and the failed step; they carry no
// environment or configuration data.
package netcap
