# Fix: HTML Truncation Mid-Tag in Telegram Status Cards

**Date:** 2026-05-14
**Component:** extras/scion-telegram/internal/telegram/format.go
**Issue:** I6 — HTML truncation mid-tag in status cards

## Problem

`FormatStateChangeCard` and `FormatInputNeededCard` build HTML strings (e.g.,
`<b>emoji slug — label</b>`) and then call `truncateMessage()` as a final step.
`truncateMessage` is UTF-8 rune-aware but not HTML-aware — if truncation lands
inside an HTML tag or entity (e.g., `&lt;`), the resulting broken HTML could be
rejected by Telegram's API.

The previous code also used raw byte slicing (`summary[:maxTaskSummaryLength]`)
which could split multi-byte runes in the task summary text.

## Fix

1. **Added `truncatePlainText(text string, maxLen int) string`** — a reusable
   helper that truncates plain text to a configurable byte length at a rune
   boundary, appending "..." if truncation occurs. Designed to be called on text
   BEFORE HTML wrapping.

2. **Added `htmlCardOverhead` constant (300 bytes)** — generous estimate of the
   fixed byte overhead from HTML tags, emoji, project line, timestamp, and labels
   in status cards.

3. **Updated `FormatStateChangeCard`** — calculates a summary budget
   (`maxTelegramMessageLength - htmlCardOverhead`, capped at `maxTaskSummaryLength`)
   and truncates the summary with `truncatePlainText` BEFORE passing it to
   `html.EscapeString`. The final `truncateMessage` call remains as a safety net.

4. **Applied the same fix to `FormatInputNeededCard`** — identical pattern for
   the prompt text.

## Verification

All 33 existing format tests pass without modification:
```
cd extras/scion-telegram && go test ./internal/telegram/ -run TestFormat
```

## Key Insight

The root cause was truncating AFTER HTML construction. By truncating the variable
plain-text content BEFORE wrapping in HTML, truncation can never produce broken
tags or entities. The `truncateMessage` safety net still exists but should rarely
trigger now.
