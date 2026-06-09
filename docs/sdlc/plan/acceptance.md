# Acceptance Criteria: hello-sdlc

## Feature: Greeting Output

```gherkin
Feature: hello-sdlc greeting

  @smoke
  Scenario: Default greeting with no flags
    Given the hello-sdlc binary is built
    When the user runs hello-sdlc with no arguments
    Then stdout contains exactly "Hello, World! Built by the SDLC pipeline."
    And the exit code is 0

  @smoke
  Scenario: Custom name via --name flag
    Given the hello-sdlc binary is built
    When the user runs hello-sdlc with --name Alice
    Then stdout contains exactly "Hello, Alice! Built by the SDLC pipeline."
    And the exit code is 0

  @edge
  Scenario: Empty string name
    Given the hello-sdlc binary is built
    When the user runs hello-sdlc with --name ""
    Then stdout contains exactly "Hello, ! Built by the SDLC pipeline."
    And the exit code is 0

  @edge
  Scenario: Name with spaces
    Given the hello-sdlc binary is built
    When the user runs hello-sdlc with --name "Jane Doe"
    Then stdout contains exactly "Hello, Jane Doe! Built by the SDLC pipeline."
    And the exit code is 0
```

## Acceptance Criteria Index

| ID     | Scenario                        | Tag    |
|--------|---------------------------------|--------|
| AC-001 | Default greeting with no flags  | @smoke |
| AC-002 | Custom name via --name flag     | @smoke |
| AC-003 | Empty string name               | @edge  |
| AC-004 | Name with spaces                | @edge  |
