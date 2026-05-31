# Make skills template-only

**Date**: 2026-05-31
**PR**: #99
**Issue**: #91

## Summary

Removed the harness-config skill-copy step from `pkg/agent/provision.go`, making templates the sole source of skills during agent provisioning. Updated documentation and tests accordingly.

## Findings

- No shipped harness-config in the repository contains a `skills` directory, confirming this change drops no existing functionality.
- The provisioning code previously copied skills from two sources (harness-config base layer, then template overlay). Now only template skills are copied.
- The documentation in `templates.md` previously described skills as coming from "both templates and harness-configs" — corrected to template-only.

## Changes

1. `pkg/agent/provision.go` — removed 11-line block copying skills from `<harness-config>/skills`
2. `docs-site/src/content/docs/advanced-local/templates.md` — rewrote "Harness Skills" section as "Skills", removed harness-config references, fixed directory structure example
3. `pkg/agent/provision_test.go` — renamed `TestProvisionAgent_SkillsDirOverlay` to `TestProvisionAgent_SkillsAreTemplateOnly` and inverted the assertion to verify harness-config skills are NOT copied
