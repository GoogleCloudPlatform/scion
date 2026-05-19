# UTF-16 Mention Extraction Fix

**Date**: 2026-05-14
**Type**: Bug fix
**Scope**: `extras/scion-telegram/internal/telegram/`

## Problem

Telegram Bot API entity offsets and lengths are measured in UTF-16 code units, not bytes or Go runes. The existing code used `len(msg.Text)` (byte length) for bounds checking and direct byte-offset slicing (`msg.Text[ent.Offset : ent.Offset+ent.Length]`), which produces incorrect substrings when the message contains characters outside the ASCII range:

- **Emoji** (e.g. 🎉 U+1F389): 4 bytes in UTF-8, 2 UTF-16 code units — offset mismatch of 2 per emoji.
- **CJK characters** (e.g. 你): 3 bytes in UTF-8, 1 UTF-16 code unit — offset mismatch of 2 per character.

This caused `isBotMentioned` and `resolveUserMentions` to extract garbled text or skip valid mentions whenever non-ASCII characters appeared before the mention entity.

## Fix

Added a `utf16Extract(s string, offset, length int) (string, bool)` helper that walks the string rune-by-rune, counting UTF-16 code units (BMP = 1 unit, supplementary plane >= U+10000 = 2 units), and maps the UTF-16 offset+length to Go byte positions.

Updated two call sites:
1. `isBotMentioned` in `mentions.go`
2. `resolveUserMentions` in `broker_v2.go`

## Testing

Added 15 new test cases covering:
- ASCII-only extraction (regression)
- Emoji before mention (1 and 2 supplementary emoji)
- CJK characters before mention
- Mixed emoji + CJK
- Edge cases: out-of-bounds, negative offset/length, zero-length, empty string, entire string
- Integration tests: `isBotMentioned` with emoji-prefixed messages

All existing tests continue to pass.
