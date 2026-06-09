# Risk Register: hello-sdlc

## Assessment

This is a minimal CLI tool with no external dependencies, no network access, no data storage, and no authentication. Risk surface is negligible.

## P0 (must-fix-before-shipping)

*None identified.* The tool is a pure function with string formatting — no security, data, integration, or performance risks.

## P1 (should-fix-before-shipping)

| Risk | Category | Description | Mitigation |
|------|----------|-------------|------------|
| R-001 | Integration | Missing Apache 2.0 license header on new files would fail CI lint checks | Include standard Google LLC copyright header on all new .go files |
| R-002 | Integration | Binary name collision if `hello-sdlc` conflicts with an existing target in Makefile | Check Makefile before adding build targets (if applicable) |
