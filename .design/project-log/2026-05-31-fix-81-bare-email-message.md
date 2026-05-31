# Fix #81: Bare email recipient in message command fails silently

**Date:** 2026-05-31
**Issue:** #81

## Problem

`scion message someone@example.com "hello"` (without the `user:` prefix) silently converted the recipient to `user:someone@example.com` and reported "Message sent" with exit code 0, even when the message was never delivered.

## Fix

Changed the bare email detection branch in `cmd/message.go` to return an explicit error with guidance instead of silently converting:

```
Error: recipient "someone@example.com" looks like an email address but is missing the "user:" prefix.

Did you mean?
  scion message user:someone@example.com "hello"
```

## Files Changed

- `cmd/message.go` — replaced silent auto-conversion with error + suggestion
- `cmd/message_test.go` — added `TestBareEmailRecipientReturnsError` with 3 sub-tests

## Design Decision

Chose Option 2 (explicit error) over Option 1 (auto-fix) because:
- It teaches users the correct syntax rather than hiding complexity
- It avoids the ambiguity of silent conversions that may silently fail downstream
- It follows the principle of least surprise for CLI tools
