# Acceptance Criteria — Hello-World CLI

## Feature: Greeting Output

```gherkin
Feature: Hello greeting with current time

  @smoke
  Scenario: Default greeting without flags
    Given the hello CLI is built
    When the user runs the hello command with no arguments
    Then the output contains "Hello, World!"
    And the output contains "The current time is"
    And the time is in HH:MM:SS format

  @smoke
  Scenario: Personalized greeting with --name flag
    Given the hello CLI is built
    When the user runs the hello command with "--name Alice"
    Then the output contains "Hello, Alice!"
    And the output contains "The current time is"

  @edge
  Scenario: Empty name flag defaults to World
    Given the hello CLI is built
    When the user runs the hello command with "--name ''"
    Then the output contains "Hello, World!"

  @edge
  Scenario: Name with spaces
    Given the hello CLI is built
    When the user runs the hello command with "--name 'Jane Doe'"
    Then the output contains "Hello, Jane Doe!"
```

## Feature: Greeting Package Unit Tests

```gherkin
Feature: Greeting function with injected time

  @smoke
  Scenario: Greet returns formatted string with given name and time
    Given a fixed time of 2026-06-09 14:30:45
    When Greet is called with name "World"
    Then the result is "Hello, World! The current time is 14:30:45."

  @smoke
  Scenario: Greet uses provided name
    Given a fixed time of 2026-06-09 09:00:00
    When Greet is called with name "Alice"
    Then the result is "Hello, Alice! The current time is 09:00:00."

  @edge
  Scenario: Greet with empty name defaults to World
    Given a fixed time of 2026-06-09 12:00:00
    When Greet is called with name ""
    Then the result is "Hello, World! The current time is 12:00:00."
```

## Acceptance Criteria Index

| ID     | Scenario                          | Tag    |
|--------|-----------------------------------|--------|
| AC-001 | Default greeting without flags    | @smoke |
| AC-002 | Personalized greeting with --name | @smoke |
| AC-003 | Empty name defaults to World      | @edge  |
| AC-004 | Name with spaces                  | @edge  |
| AC-005 | Greet function with fixed time    | @smoke |
| AC-006 | Greet uses provided name          | @smoke |
| AC-007 | Greet empty name defaults         | @edge  |
