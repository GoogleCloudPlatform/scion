# Project Log: Agent-lookup error disclosure

**Date:** 2026-09-30

## Overview

Closed a disclosure gap in the broker's stop/restart handlers, where a
container-runtime listing failure's own error text reached the HTTP
response body.

## Agent-lookup error disclosure

- `stopAgent` and `restartAgent` (`pkg/runtimebroker/handlers.go`) resolve
  their target through `projectScopedTarget`, which wraps a container-runtime
  listing failure in `ErrAgentListUnavailable`. Both handlers now route that
  case through a fixed 503 response (`RuntimeUnavailable`, naming only the
  agent ID), matching the PTY attach handler's existing handling of the same
  error. Any other lookup failure (an ambiguous multi-container match, for
  instance) gets a fixed 500 body instead. In both cases the underlying error
  — which can carry a runtime's connection target, namespace, or service
  identity — reaches only the server's own log and span, never the response
  body.
- Every other caller reachable through the same lookup functions
  (`execCommand`, `resetAuth`, the PTY attach and control-channel paths) was
  checked and already maps a listing failure to a fixed body or an internal
  enum with no error text in it.
