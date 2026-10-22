---
name: scion-port-serving
description: >-
  How to serve anything that listens on a port inside your agent (a web page
  or app, an API, a dashboard, a docs preview) so a person or program can
  reach it through the Hub's port proxy: scion expose and unexpose, the URL to
  hand over, what the proxy carries, and scion tunnel for full apps. Use
  before running scion expose. For web pages and apps, also read
  references/web-apps.md.
---

# Serving from your agent through the port proxy

Services inside your agent are not reachable from outside it. `scion expose`
registers a local port with the Hub, which then serves it at a URL that
people with access to your agent can open.

## Steps

1. Run the service on `127.0.0.1` on a free port (1024 or above; 9810 and
   18380 cannot be exposed). Only loopback services can be exposed.
2. Run: `scion expose <port> --label "<what it is>"`
3. Check it works the way the person will use it (see What the proxy
   carries; for pages, references/web-apps.md).
4. Give the person the URL **exactly as printed**, including the trailing
   `/`. Do not hand out tokens, and do not build the URL by hand.

## Commands

- `scion expose <port> --label "<what it is>"` exposes a port and prints its
  URL. Exposing a port that is already exposed succeeds and prints the same
  URL.
- `scion expose --list` lists your exposed ports with their labels and URLs,
  and whether auto-expose is on.
- `scion unexpose <port>` stops exposing a port. It succeeds if the port was
  not exposed. If auto-expose is on, a port that is still listening may be
  exposed again on its next scan.
- An agent can have at most 10 ports exposed at once. Unexpose ports you no
  longer need.
- These commands work only inside an agent with a Hub connection.

## What the proxy carries

- HTTP requests of any method, with the path below the URL and the query
  string passed through to your service. Programs such as scripts and `curl`
  can call the URL too, with the Hub credentials of someone who has access to
  the agent.
- Your service does not learn who is calling. Do not build a login on top
  of the link.
- WebSockets and streamed responses (for example server-sent events) work
  only when the Hub runs its conduit transport, the same setting
  `scion tunnel` needs. Without it, a WebSocket request is refused with an
  error, each response is delivered whole once your service has finished
  it, and request bodies are limited to 32 MiB.
- Your service must start answering within 60 seconds (the Hub's default),
  or the caller gets a timeout error. Without the conduit transport, the
  whole response must arrive within that time.
- Pages opened in a browser through the link can only use what is inside the
  page itself. See references/web-apps.md before serving a web page or app.

## Full apps: scion tunnel

For a full multi-file web app, a dev server, or anything the proxy does not
carry, the person can run `scion tunnel` on their own machine with the scion
CLI, if their Hub has `scion tunnel` turned on and they have port access to
the agent. Keep the port exposed with `scion expose <port>` (the tunnel only
reaches exposed ports), then suggest:

    scion tunnel <agent> --port <port>

The service then opens on a local port on their machine and works as it does
inside the agent, for any protocol. See
https://googlecloudplatform.github.io/scion/hosted/user/port-forwarding/

## Web pages and apps

For anything a person opens in a browser through the link, read
references/web-apps.md: build settings for one self-contained HTML file,
what does not work in a page, routing, and how to verify it honestly.
