---
title: Resolved Project Settings
description: The GET /settings/resolved endpoint reports project settings alongside whether a hub default exists — and deliberately does not compute an effective value.
---

```
GET /api/v1/projects/{projectId}/settings/resolved
```

Returns every project setting together with an indication of whether the hub
has a default for it.

Authorization is `ActionRead` on the project — the same as
`GET /api/v1/projects/{projectId}/settings`. It is deliberately not admin-gated,
because the point is to let non-admin project owners see that hub defaults
exist. Only the presence of `agent_defaults` entries is exposed, never the
server configuration itself. The endpoint is read-only; there is no `PUT`.

## This endpoint does not resolve anything

The name says `resolved`. The response contains no resolved value. This is the
single most important thing to know about the endpoint, and it is not an
oversight — the effective-value field was specified, reviewed, and deliberately
removed.

Scion decides a setting's actual value by walking a precedence ladder with more
rungs than this endpoint can see: an explicit request value, the project
annotation, a template, a harness config, and the hub operational default.
That ladder is owned by the code that applies it.

If this endpoint computed an effective value, it would be a **second
implementation of that ordering**, living in a package that cannot observe
changes to the first one. Two implementations of one ordering do not stay equal.
Worse, the copy fails silently: a stale answer is still a well-formed answer, so
nothing breaks visibly when they drift apart. No test can fail when this
endpoint's idea of the ladder and the real ladder diverge, which is exactly why
the value is not computed here at all.

This applies to the hub's value too, not only to a field literally named
`value` — and that is not a theoretical concern. Scion's own agent-defaults code
carries provenance through the request path explicitly, precisely so that it
never has to compare a value back against the hub default: doing so misreports
the case where a user, a project annotation or a template happens to name the
same value the hub defaults to. A response containing `projectValue` and
`hubValue` side by side would hand every client the operands of that same
misreport, so "the client can just compare them" is not a workaround — the
comparison *is* the bug.

The shape also asserts precedence by adjacency: two fields with nothing between
them imply there is nothing between them. A client reading
`projectValue: null, hubValue: 200` concludes "I will get 200", and that is
wrong the moment a template supplies a value instead. So the response reports
whether a hub default **exists**, and never what it is.

**If you need the effective value, resolve it yourself**, against whatever
ladder exists at the time you ask. The endpoint answers "is there a hub default
here?" — it does not answer "what will I get?".

## Response

```jsonc
{
  // The existing ProjectSettings object, unchanged.
  "project": { "defaultMaxTurns": 120 },

  // Keyed by annotation key — the same keys the settings registry uses.
  "settings": {
    "scion.io/default-max-turns": {
      "projectSet": true,
      "projectValue": "120",
      "hubDefault": "present"
    },
    "scion.io/default-model": {
      "projectSet": false,
      "projectValue": null,
      "hubDefault": "absent"
    },
    "scion.io/default-template": {
      "projectSet": false,
      "projectValue": null,
      "hubDefault": "unknown"
    }
  }
}
```

Every registered project setting appears in `settings`, always. A key is never
omitted; if nothing is known about it, that is stated rather than implied by
absence. A drift guard fails the build if a setting is added to the registry
without being wired up here.

### Per-setting fields

| Field | Type | Meaning |
| --- | --- | --- |
| `projectSet` | bool | Whether the project has an annotation for this setting. |
| `projectValue` | string \| null | The annotation's raw value, or `null` when `projectSet` is false. Always a string — annotations are strings. |
| `hubDefault` | string | Whether a hub-level default exists. See below. |

### `hubDefault` is tri-state

`hubDefault` is `"present"`, `"absent"`, or `"unknown"` — not a boolean.

| Value | Meaning |
| --- | --- |
| `present` | The hub has a default for this setting. |
| `absent` | The hub has no default for this setting, and we are sure. |
| `unknown` | We could not determine it. **Not** the same as `absent`. |

The third state exists because a boolean has no room for "I did not look here",
and reporting a confident `false` for a question that was never asked is simply
a false statement. Two distinct situations produce `unknown`:

- **File-mode configuration.** When the hub runs from a config file rather than
  a settings database, the agent-defaults section is never loaded, even if the
  operator's file contains it. The hub holds a zero value that reflects the load
  path, not the operator's intent. Reporting `absent` here would claim the
  operator configured nothing, which the hub is in no position to know. This
  emptiness is deliberate — it keeps file-mode behaviour unchanged for existing
  single-node installs — so in file mode `unknown` is the permanent and correct
  answer for these settings, not a temporary gap awaiting a fix.

- **A value that cannot be distinguished from unset.** Some hub defaults are
  stored in a form where an explicitly-configured empty value and an unset value
  are byte-identical once written — the write path drops empty strings. For
  those fields, absence is genuinely ambiguous and is reported as `unknown`.
  Fields whose schema forbids the zero value (for example
  `default_max_turns`, which has a minimum of 1) are unambiguous and do report
  `absent`.

Clients should treat `unknown` as "do not display a claim about the hub", not as
`absent`. Rendering "no hub default" on `unknown` reintroduces exactly the false
statement the third state exists to prevent.

Settings with no hub-level counterpart at all — the hub simply has no such field
— report `absent`, which is a measured structural fact rather than a guess.
