// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Pure fixture data shared by fixture.ts (loaded in the real browser via
 * Vite) and mock-api.ts (loaded by Playwright's Node test runner). Kept
 * import-free of anything DOM/CSS/Playwright-specific so either side can
 * import it without pulling in the other's runtime.
 */
import type { Message } from '../../src/shared/types.js';

export const SELF_USER_ID = 'self-user';
export const PEER_USER_ID = 'peer-user';
export const CONVERSATION_KEY = `dm:user:${SELF_USER_ID}:user:${PEER_USER_ID}`;
export const PROJECT_A = 'proj-alpha';
export const PROJECT_B = 'proj-beta';

export const IMAGE_ATTACHMENT_ID = 'att-image-1';
export const TEXT_ATTACHMENT_ID = 'att-text-1';
export const BINARY_ATTACHMENT_ID = 'att-bin-1';

/** A message referencing an in-prose container path, in project A. */
export const PATH_MESSAGE_A: Message = {
  id: 'msg-path-a',
  projectId: '',
  sender: 'user:peer',
  senderId: PEER_USER_ID,
  recipient: 'user:self',
  recipientId: SELF_USER_ID,
  msg: 'see /workspace/notes.md for the plan',
  type: 'chat',
  agentId: '',
  createdAt: '2026-01-01T00:00:01Z',
  senderProjectId: PROJECT_A,
};

/** The *same* file name, in a different project — proves no cross-project reuse. */
export const PATH_MESSAGE_B: Message = {
  id: 'msg-path-b',
  projectId: '',
  sender: 'user:peer',
  senderId: PEER_USER_ID,
  recipient: 'user:self',
  recipientId: SELF_USER_ID,
  msg: 'different notes: /workspace/notes.md',
  type: 'chat',
  agentId: '',
  createdAt: '2026-01-01T00:00:02Z',
  senderProjectId: PROJECT_B,
};

/** A message referencing an image path, in project A. */
export const PATH_IMAGE_MESSAGE: Message = {
  id: 'msg-path-image',
  projectId: '',
  sender: 'user:peer',
  senderId: PEER_USER_ID,
  recipient: 'user:self',
  recipientId: SELF_USER_ID,
  msg: 'screenshot: /workspace/diagram.png',
  type: 'chat',
  agentId: '',
  createdAt: '2026-01-01T00:00:04Z',
  senderProjectId: PROJECT_A,
};

/** A message referencing a path whose real content exceeds the 512 KiB inline-preview limit. */
export const PATH_OVERSIZE_MESSAGE: Message = {
  id: 'msg-path-oversize',
  projectId: '',
  sender: 'user:peer',
  senderId: PEER_USER_ID,
  recipient: 'user:self',
  recipientId: SELF_USER_ID,
  msg: 'full log: /workspace/huge.log',
  type: 'chat',
  agentId: '',
  createdAt: '2026-01-01T00:00:05Z',
  senderProjectId: PROJECT_A,
};

export const ATTACHMENT_MESSAGE: Message = {
  id: 'msg-attachments',
  projectId: '',
  sender: 'user:peer',
  senderId: PEER_USER_ID,
  recipient: 'user:self',
  recipientId: SELF_USER_ID,
  msg: 'here are the files',
  type: 'chat',
  agentId: '',
  createdAt: '2026-01-01T00:00:03Z',
  senderProjectId: PROJECT_A,
};

export const TEXT_ATTACHMENT_BODY = '# Plan\n\nShip the extracted preview.\n';
export const ALPHA_NOTES_CONTENT = 'Alpha project notes content.';
export const BETA_NOTES_CONTENT = 'Beta project notes content — a different file entirely.';

// ---------------------------------------------------------------------------
// gs:// link fixtures. AGENT_ID is a sender distinct from SELF_USER_ID so
// chat-thread's v2 "not me" heuristic reports these messages as
// fromAgent=true, matching a real agent-sent message; GCS_USER_OWN_MSG is
// sent by SELF_USER_ID itself, so it is reliably fromAgent=false regardless
// of that heuristic.
// ---------------------------------------------------------------------------

export const AGENT_ID = 'agent-1';

const gcsMsg = (id: string, msg: string, createdAt: string, senderId = AGENT_ID): Message => ({
  id,
  projectId: '',
  sender: senderId === AGENT_ID ? 'agent:agent-1' : 'user:self',
  senderId,
  recipient: senderId === AGENT_ID ? 'user:self' : 'agent:agent-1',
  recipientId: senderId === AGENT_ID ? SELF_USER_ID : AGENT_ID,
  msg,
  type: 'chat',
  agentId: AGENT_ID,
  createdAt,
});

export const GCS_BUCKET = 'gcs-bkt';

// The gcs endpoint validates the `message` param as a UUID (matching the
// real hub), so every gcs fixture message needs a real UUID id, unlike the
// plain string ids ("msg-path-a") the path/attachment fixtures above use.
export const GCS_TEXT_AND_CODE_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000001',
  `see gs://${GCS_BUCKET}/dir/file.md and inline \`gs://${GCS_BUCKET}/dir/file.md\` too`,
  '2026-02-01T00:00:01Z'
);

export const GCS_TRAILING_PUNCT_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000002',
  `end with a comma gs://${GCS_BUCKET}/dir/file.md, and a period gs://${GCS_BUCKET}/dir/file.md.`,
  '2026-02-01T00:00:02Z'
);

export const GCS_DIR_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000003',
  `directories stay plain: gs://${GCS_BUCKET}/dir/ and gs://${GCS_BUCKET}/a/b.c/`,
  '2026-02-01T00:00:03Z'
);

export const GCS_NOLINK_SCHEME_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000004',
  `not links: xgs://${GCS_BUCKET}/o and /gs://${GCS_BUCKET}/o`,
  '2026-02-01T00:00:04Z'
);

export const GCS_START_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000005',
  `gs://${GCS_BUCKET}/start.txt leads the message`,
  '2026-02-01T00:00:05Z'
);

export const GCS_AFTER_CODE_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000006',
  `\`x\`gs://${GCS_BUCKET}/after.txt`,
  '2026-02-01T00:00:06Z'
);

export const GCS_WORKSPACE_COLLISION_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000007',
  `gs://${GCS_BUCKET}/workspace/collision.md`,
  '2026-02-01T00:00:07Z'
);

export const GCS_HOSTILE_QUOTE_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000008',
  `gs://${GCS_BUCKET}/a"onmouseover=alert(1)`,
  '2026-02-01T00:00:08Z'
);

export const GCS_HOSTILE_IMG_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000009',
  `gs://${GCS_BUCKET}/a<img src=x onerror=alert(1)>`,
  '2026-02-01T00:00:09Z'
);

export const GCS_HOSTILE_AMP_MSG = gcsMsg(
  '00000000-0000-4000-8000-00000000000a',
  `gs://${GCS_BUCKET}/a&b`,
  '2026-02-01T00:00:10Z'
);

export const GCS_USER_OWN_MSG = gcsMsg(
  '00000000-0000-4000-8000-00000000000b',
  `gs://${GCS_BUCKET}/dir/file.md`,
  '2026-02-01T00:00:11Z',
  SELF_USER_ID
);

export const GCS_XPROJECT_BUCKET = 'scion-xproject-exchange';
export const GCS_XPROJECT_OBJECT = 'workspace-volumes/dev-brief.md';
export const GCS_XPROJECT_MSG = gcsMsg(
  '00000000-0000-4000-8000-00000000000c',
  `see gs://${GCS_XPROJECT_BUCKET}/${GCS_XPROJECT_OBJECT}`,
  '2026-02-01T00:00:12Z'
);

export const GCS_JSON_MSG = gcsMsg(
  '00000000-0000-4000-8000-00000000000d',
  `data file: gs://${GCS_BUCKET}/data.json`,
  '2026-02-01T00:00:13Z'
);

export const GCS_NOTFOUND_MSG = gcsMsg(
  '00000000-0000-4000-8000-00000000000e',
  `missing: gs://${GCS_BUCKET}/missing.txt`,
  '2026-02-01T00:00:14Z'
);

export const GCS_DENIED_MSG = gcsMsg(
  '00000000-0000-4000-8000-00000000000f',
  `denied: gs://${GCS_BUCKET}/denied.txt`,
  '2026-02-01T00:00:15Z'
);

export const GCS_SHORTLINK_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000010',
  `gs://${GCS_BUCKET}/o/r#1`,
  '2026-02-01T00:00:16Z'
);

// A '#', '?' or '&' immediately after the object, followed by further
// non-whitespace text, is a full reject — no link at all, not a truncation
// to the text before it.
export const GCS_FRAGMENT_HASH_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000011',
  `gs://${GCS_BUCKET}/a#frag`,
  '2026-02-01T00:00:17Z'
);

export const GCS_FRAGMENT_QUERY_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000012',
  `gs://${GCS_BUCKET}/a?x=1`,
  '2026-02-01T00:00:18Z'
);

// The quoted form is not a continuation: the quote ends the object class run
// on its own, same as a space would, so this still links normally.
export const GCS_QUOTED_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000013',
  `see "gs://${GCS_BUCKET}/o.md" for the plan`,
  '2026-02-01T00:00:19Z'
);

// A message from PEER_USER_ID — the canonical other party of this exact DM
// (CONVERSATION_KEY) — not built via gcsMsg, which only knows AGENT_ID vs.
// "self". v2's fromAgent heuristic (senderId !== currentUserId) renders this
// exactly like an agent message (left-aligned, "not me"), but the real
// sender is a user: gs:// linkification must gate on that, not on fromAgent.
export const GCS_OTHER_USER_MSG: Message = {
  id: '00000000-0000-4000-8000-000000000014',
  projectId: '',
  sender: 'user:peer',
  senderId: PEER_USER_ID,
  recipient: 'user:self',
  recipientId: SELF_USER_ID,
  msg: `gs://${GCS_BUCKET}/dir/file.md`,
  type: 'chat',
  agentId: '',
  createdAt: '2026-02-01T00:00:20Z',
};

// The messages below pin accepted client-wider mismatches: the client runs
// its linkifier over marked's rendered, tag-split, HTML-escaped text, while
// the hub scans the raw markdown body, so the two sides can extract a
// different object (or none) from the same posted text. Each assertion in
// gcs-link.pw.ts for these describes only the client's actual current
// behaviour, never that it is correct UX — the server's rule for each is
// covered by the parity rows and the standalone tests in
// pkg/hub/gcs_link_test.go.

// Emphasis consumes the underscores around the URI, so the client links
// "o". The server allows nothing for this raw body: the leading "_" is a
// word byte, so the left boundary fails.
export const GCS_EMPHASIS_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000015',
  `_gs://${GCS_BUCKET}/o_ done`,
  '2026-02-01T00:00:21Z'
);

// Strikethrough consumes both leading and trailing "~~", so the client
// links "o" here, even though "~" is itself a valid object-class character
// the raw body would extend the object with (the server allows "o~~").
export const GCS_STRIKETHROUGH_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000016',
  `~~gs://${GCS_BUCKET}/o~~ done`,
  '2026-02-01T00:00:22Z'
);

// The backtick starts an inline-code span, ending the rendered text segment
// right after '?' with no further character in that segment — so the
// client's continuation check never sees the 'x' that follows in the raw
// body, and links "a".
export const GCS_TAG_SPLIT_QUESTION_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000017',
  `gs://${GCS_BUCKET}/a?\`x\` done`,
  '2026-02-01T00:00:23Z'
);

// A literal "&amp;" already in the raw text is parsed as the entity it
// names and then re-escaped for HTML output, landing back on the same
// single "&amp;" rather than a doubled "&amp;amp;" — the same rendered
// result a bare "&" character would produce. Followed by whitespace, this
// is not a continuation, so the client links "a".
export const GCS_RAW_ENTITY_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000018',
  `gs://${GCS_BUCKET}/a&amp; done`,
  '2026-02-01T00:00:24Z'
);

// A markdown escape (raw backslash) is resolved by marked into its literal
// character before the client's linkifier ever runs, so the client links
// "secret_v2" in full; the raw body's backslash instead voids the whole
// candidate on the server (see backslash-voids-candidate in
// pkg/hub/gcs_link_test.go).
export const GCS_BACKSLASH_ESCAPE_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000019',
  `gs://${GCS_BUCKET}/secret\\_v2 done`,
  '2026-02-01T00:00:25Z'
);

// Strong emphasis ("**...**") starts a new rendered tag right after the
// '?', the same tag-split mechanism as the inline-code case above, so the
// client links "a".
export const GCS_DOUBLE_STAR_MSG = gcsMsg(
  '00000000-0000-4000-8000-00000000001a',
  `gs://${GCS_BUCKET}/a?**x** done`,
  '2026-02-01T00:00:26Z'
);

// A literal "&lt;" already in the raw text is parsed as the entity it
// names and then re-escaped for HTML output, landing back on the same
// "&lt;" rather than a doubled escape — the same mechanism as the "&amp;"
// case above. The client's continuation rule treats only "&amp;" as the
// "&" marker, so the escaped "&lt;" right after "a" (a literal "<") is not
// a continuation, and the client links "a".
export const GCS_RAW_LT_MSG = gcsMsg(
  '00000000-0000-4000-8000-00000000001b',
  `gs://${GCS_BUCKET}/a&lt;x done`,
  '2026-02-01T00:00:27Z'
);

// Same mechanism as "&lt;" above, for a literal "&quot;" in the raw text.
export const GCS_RAW_QUOT_MSG = gcsMsg(
  '00000000-0000-4000-8000-00000000001c',
  `gs://${GCS_BUCKET}/a&quot;x done`,
  '2026-02-01T00:00:28Z'
);

// A binary-looking name (by extension) that the hub denies as not found: the
// client must still fetch it — content type is the server's call for a gcs
// target, not a client-side name guess — and show the same uniform
// not-available message and Cloud Console fallback as any other gcs 404.
export const GCS_BINARY_NOTFOUND_MSG = gcsMsg(
  '00000000-0000-4000-8000-00000000001d',
  `report: gs://${GCS_BUCKET}/report.pdf`,
  '2026-02-01T00:00:29Z'
);

// ')' is a trim byte (isGCSObjectTrimByte), so a trailing, unmatched ')'
// right after the object is excluded from the link the same way the comma
// and period in GCS_TRAILING_PUNCT_MSG are. This is the real-Chromium
// counterpart to the jsdom case in chat-file-links.test.ts.
export const GCS_TRAILING_PAREN_MSG = gcsMsg(
  '00000000-0000-4000-8000-00000000001e',
  `(see gs://${GCS_BUCKET}/dir/file.md)`,
  '2026-02-01T00:00:30Z'
);

// A gs:// URI inside a fenced code block renders inside a <pre> and must not
// link, unlike the inline-code URI in GCS_TEXT_AND_CODE_MSG, which does. This
// is the real-Chromium counterpart to the jsdom case in chat-message.test.ts.
export const GCS_FENCED_BLOCK_MSG = gcsMsg(
  '00000000-0000-4000-8000-00000000001f',
  '```\n' + `gs://${GCS_BUCKET}/dir/file.md` + '\n```',
  '2026-02-01T00:00:31Z'
);

// ---------------------------------------------------------------------------
// Image sniffing, SVG-as-source, octet-stream, the Content-Length
// preview-size abort, 413, and an HTML-bodied object. Real Chromium is
// required for these (happy-dom does not actually decode image bytes or
// enforce the sandboxed-download behavior a navigation triggers).
// ---------------------------------------------------------------------------

export const GCS_IMAGE_PNG_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000020',
  `gs://${GCS_BUCKET}/photo.png`,
  '2026-02-01T00:00:32Z'
);

export const GCS_IMAGE_JPEG_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000021',
  `gs://${GCS_BUCKET}/photo.jpg`,
  '2026-02-01T00:00:33Z'
);

export const GCS_IMAGE_GIF_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000022',
  `gs://${GCS_BUCKET}/photo.gif`,
  '2026-02-01T00:00:34Z'
);

export const GCS_IMAGE_WEBP_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000023',
  `gs://${GCS_BUCKET}/photo.webp`,
  '2026-02-01T00:00:35Z'
);

// The hub never serves image/svg+xml; an SVG object's sniffed
// Content-Type is its real text/plain, same as any other text object — shown
// as source text, never as <img>, regardless of its .svg extension.
export const GCS_SVG_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000024',
  `gs://${GCS_BUCKET}/diagram.svg`,
  '2026-02-01T00:00:36Z'
);

export const GCS_SVG_BODY = '<svg xmlns="http://www.w3.org/2000/svg"><circle r="1"/></svg>';

// A .png name whose actual bytes are HTML: the hub's content sniff ignores
// both metadata and extension, so this is served as text/plain, never as an
// image and never as text/html — the client must show it as source/code
// text, not <img>, and its Download must still force a file download rather
// than a rendered page.
export const GCS_HTML_AS_PNG_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000025',
  `gs://${GCS_BUCKET}/fake.png`,
  '2026-02-01T00:00:37Z'
);

export const GCS_HTML_BODY = '<html><body><h1>not a png</h1></body></html>';

export const GCS_OCTET_STREAM_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000026',
  `gs://${GCS_BUCKET}/archive.blob`,
  '2026-02-01T00:00:38Z'
);

// The response's Content-Length is over TEXT_PREVIEW_MAX_BYTES (512 KiB) while
// its decoded text is under it, so only the Content-Length check can classify
// it as too large.
export const GCS_TOO_LARGE_INLINE_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000027',
  `gs://${GCS_BUCKET}/huge.txt`,
  '2026-02-01T00:00:39Z'
);

export const GCS_TOO_LARGE_INLINE_BYTES = 512 * 1024 + 1;

export const GCS_413_MSG = gcsMsg(
  '00000000-0000-4000-8000-000000000028',
  `gs://${GCS_BUCKET}/giant.bin`,
  '2026-02-01T00:00:40Z'
);

export const GCS_MESSAGES: Message[] = [
  GCS_TEXT_AND_CODE_MSG,
  GCS_TRAILING_PUNCT_MSG,
  GCS_DIR_MSG,
  GCS_NOLINK_SCHEME_MSG,
  GCS_START_MSG,
  GCS_AFTER_CODE_MSG,
  GCS_WORKSPACE_COLLISION_MSG,
  GCS_HOSTILE_QUOTE_MSG,
  GCS_HOSTILE_IMG_MSG,
  GCS_HOSTILE_AMP_MSG,
  GCS_USER_OWN_MSG,
  GCS_XPROJECT_MSG,
  GCS_JSON_MSG,
  GCS_NOTFOUND_MSG,
  GCS_DENIED_MSG,
  GCS_SHORTLINK_MSG,
  GCS_FRAGMENT_HASH_MSG,
  GCS_FRAGMENT_QUERY_MSG,
  GCS_QUOTED_MSG,
  GCS_OTHER_USER_MSG,
  GCS_EMPHASIS_MSG,
  GCS_STRIKETHROUGH_MSG,
  GCS_TAG_SPLIT_QUESTION_MSG,
  GCS_RAW_ENTITY_MSG,
  GCS_BACKSLASH_ESCAPE_MSG,
  GCS_DOUBLE_STAR_MSG,
  GCS_RAW_LT_MSG,
  GCS_RAW_QUOT_MSG,
  GCS_BINARY_NOTFOUND_MSG,
  GCS_TRAILING_PAREN_MSG,
  GCS_FENCED_BLOCK_MSG,
  GCS_IMAGE_PNG_MSG,
  GCS_IMAGE_JPEG_MSG,
  GCS_IMAGE_GIF_MSG,
  GCS_IMAGE_WEBP_MSG,
  GCS_SVG_MSG,
  GCS_HTML_AS_PNG_MSG,
  GCS_OCTET_STREAM_MSG,
  GCS_TOO_LARGE_INLINE_MSG,
  GCS_413_MSG,
];

export const GCS_MARKDOWN_BODY = '# Dev Brief\n\nShip the gs:// link viewer.\n';
export const GCS_JSON_BODY = '{"ok":true}';
