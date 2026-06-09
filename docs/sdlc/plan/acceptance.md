# Acceptance Criteria — tempconv

## Feature: Temperature Conversion

### AC-001: Celsius to Fahrenheit
```gherkin
@smoke
Scenario: Convert Celsius to Fahrenheit
  Given the user runs tempconv with --from celsius --to fahrenheit 100
  When the conversion is performed
  Then the output is "212.00"
```

### AC-002: Fahrenheit to Celsius
```gherkin
@smoke
Scenario: Convert Fahrenheit to Celsius
  Given the user runs tempconv with --from fahrenheit --to celsius 32
  When the conversion is performed
  Then the output is "0.00"
```

### AC-003: Celsius to Kelvin
```gherkin
@smoke
Scenario: Convert Celsius to Kelvin
  Given the user runs tempconv with --from celsius --to kelvin 0
  When the conversion is performed
  Then the output is "273.15"
```

### AC-004: Kelvin to Fahrenheit
```gherkin
Scenario: Convert Kelvin to Fahrenheit
  Given the user runs tempconv with --from kelvin --to fahrenheit 373.15
  When the conversion is performed
  Then the output is "212.00"
```

### AC-005: Same-scale conversion (identity)
```gherkin
Scenario: Same scale returns input unchanged
  Given the user runs tempconv with --from celsius --to celsius 42
  When the conversion is performed
  Then the output is "42.00"
```

### AC-006: Below absolute zero error
```gherkin
@edge
Scenario: Reject temperature below absolute zero
  Given the user runs tempconv with --from kelvin --to celsius -1
  When the conversion is performed
  Then the tool exits with an error indicating the temperature is below absolute zero
```

### AC-007: Invalid scale name
```gherkin
@edge
Scenario: Reject unknown temperature scale
  Given the user runs tempconv with --from rankine --to celsius 100
  When the conversion is performed
  Then the tool exits with an error indicating an unknown scale
```

### AC-008: Missing value argument
```gherkin
@edge
Scenario: Error when no temperature value provided
  Given the user runs tempconv with --from celsius --to fahrenheit but no value
  When the tool is invoked
  Then the tool exits with a usage error
```

### AC-009: Negative Celsius values
```gherkin
Scenario: Convert negative Celsius to Fahrenheit
  Given the user runs tempconv with --from celsius --to fahrenheit -40
  When the conversion is performed
  Then the output is "-40.00"
```

### AC-010: Decimal precision
```gherkin
Scenario: Output has two decimal places
  Given the user runs tempconv with --from fahrenheit --to celsius 100
  When the conversion is performed
  Then the output is "37.78"
```
