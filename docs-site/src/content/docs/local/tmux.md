---
title: Interactive Sessions with Tmux
description: Learn how to use and customize the built-in tmux session management in Scion.
---

Scion uses [tmux](https://github.com/tmux/tmux), a terminal multiplexer, as the default and mandatory shell wrapper for all agent sessions. This ensures that every interactive session is persistent, collaborative, and consistent across all runtimes (Docker, Apple Virtualization, Kubernetes, etc.).

## Why Tmux?

Tmux is automatically started inside every Scion agent. This provides several critical features:

1.  **Session Persistence**: You can detach from an agent session without stopping the underlying process. If your terminal connection drops, the agent keeps working.
2.  **Reliable Attachment**: `scion attach <agent-name>` always connects you to the same persistent shell session.
3.  **Live Collaboration**: Multiple users can run `scion attach` for the same agent simultaneously. Everyone sees the same screen and can type together, creating a built-in pair-programming environment.

## Basic Operations

When you are attached to an agent, you interact with `tmux` using a **prefix key** (Default: `Ctrl-b`).

| Action | Command |
| :--- | :--- |
| **Detach from Session** | `Prefix` then `d` |
| **Enter Scroll Mode** | `Prefix` then `[` (use arrow keys, `q` to exit) |
| **Split Vertically** | `Prefix` then `%` |
| **Split Horizontally** | `Prefix` then `"` |
| **Switch Panes** | `Prefix` then `Arrow Keys` |
| **Toggle Mouse Mode** | `Prefix` then `m` (Enables scrolling and pane selection via mouse; **on by default**) |

## Web Terminal Interactivity

The Scion Web Dashboard provides a built-in terminal interface for agents that fully supports Tmux. This Web Terminal includes:
- **Toolbar Integration**: Visual buttons to easily send common Tmux sequences like detach, split panes, or enter scroll mode.
- **Window Toggles**: Redesigned, wider toolbar controls for managing tmux windows seamlessly switch between the agent session and a standard shell window. These controls use direct tmux key bindings for maximum reliability.
- **Mouse & Text Selection Together**: Tmux mouse mode stays enabled for scroll wheel and pane interactions. To select text in the browser without disabling mouse mode, hold `Shift` while dragging. On macOS, `Option`-drag also works. 
- **Persistent Copy-Paste**: Improved clipboard handling ensures reliable copy and paste interactions, including proper mouse-drag text selection within the web-based Tmux session.
- **Optional Mouse Toggle**: The toolbar still exposes explicit mouse on/off controls if you want plain browser drag selection without a modifier.
- **Automatic Window Sizing**: The web terminal automatically adjusts the tmux window size upon attachment, ensuring immediate terminal redrawing to fit your browser's dimensions.
- **Extended Key Sequences**: Native support for extended key sequences (`CSI u`), ensuring combinations like `Shift+Enter` are correctly forwarded to modern CLI tools.
- **Standardized Environment**: PTY sessions automatically enforce `TERM=xterm-256color` for consistent color and feature support across all shells.

## Customizing Your Session

Each agent's `tmux` behavior is controlled by a `.tmux.conf` file in its home directory. This file is seeded from your project's template.

### Changing Settings for New Agents

To change the default `tmux` configuration for all **new** agents in a project, modify the template file:
`.scion/templates/default/home/.tmux.conf`

### Solving Nested Sessions (Google Cloud Shell)

If you are running Scion inside another `tmux` session (such as in **Google Cloud Shell** or your own local `tmux`), the default `Ctrl-b` prefix will be intercepted by your "outer" session.

To use a different prefix (like `Ctrl-a`) for your Scion agents, add the following to your `.tmux.conf` template:

```tmux
# Set prefix to Ctrl-a
set -g prefix C-a
unbind C-b
bind C-a send-prefix
```

After updating the template, any new agents you create will use `Ctrl-a` as their prefix, allowing you to use `Ctrl-b` for your host session and `Ctrl-a` for the Scion agent session.

## Configuration Reference

While `tmux` is now mandatory, you may still see `tmux: true` in legacy `settings.yaml` files. This setting is now effectively ignored as the orchestrator always wraps sessions in `tmux`.
