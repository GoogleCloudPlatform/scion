# Risk Register — tempconv

## P1: Floating-point precision
**Category:** Data
**Risk:** Floating-point arithmetic may produce results like 211.99999999 instead of 212.00.
**Mitigation:** Use `fmt.Sprintf("%.2f", value)` for output formatting. Use tolerance-based comparison in tests (epsilon of 0.01).

## P1: Absolute zero boundary
**Category:** Data
**Risk:** Accepting temperatures below absolute zero produces physically meaningless results.
**Mitigation:** Validate input against absolute zero for each scale before conversion. Return a clear error message.
