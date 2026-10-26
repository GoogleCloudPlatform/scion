---
title: Apple Container DNS Setup
description: Manual steps to configure DNS for Apple Container runtime on macOS.
---

When using Apple Container as your container runtime, Scion agents need to reach the Hub server. This requires a DNS rule that maps `host.containers.internal` to the loopback address.

Apple Container's DNS rules persist across sessions but the underlying PF (packet filter) rules do not survive macOS reboots. You need to re-run this command after each reboot:

```bash
sudo container system dns create host.containers.internal --localhost 203.0.113.1
```

## Why sudo is required

The DNS setup modifies macOS PF (packet filter) rules, which require root access. This is an Apple Container limitation — there is no rootless alternative for PF rules.

## Automating after reboot

Because the command requires root, it must be installed as a **system-level LaunchDaemon** (not a user LaunchAgent). User-level agents run without a terminal, so `sudo` cannot prompt for a password.

Create `/Library/LaunchDaemons/org.scion.apple-container-dns.plist` as root:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>org.scion.apple-container-dns</string>
    <key>ProgramArguments</key>
    <array>
        <string>/opt/homebrew/bin/container</string>
        <string>system</string>
        <string>dns</string>
        <string>create</string>
        <string>host.containers.internal</string>
        <string>--localhost</string>
        <string>203.0.113.1</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>
```

:::note
Adjust the path `/opt/homebrew/bin/container` if Apple Container is installed elsewhere on your system.
:::

Load the daemon (runs immediately and on every subsequent boot):

```bash
sudo launchctl bootstrap system /Library/LaunchDaemons/org.scion.apple-container-dns.plist
```

## Verification

To check if the DNS rule is configured:

```bash
container system dns list
```

You should see `host.containers.internal` in the output.

## Git Workspace Restrictions

Because the Apple `container` runtime mounts directories with VirtioFS and cannot bind-mount single files, git-backed workspaces have the following rules on Apple `container` (see [Git that Scion runs on the host](/scion/local/workspace/#git-that-scion-runs-on-the-host)):

- **Read-only `.git` and `.git/config` credentials**: In `worktree-per-agent` and registered outside-root worktree layouts, the repository's `.git` root is mounted read-only and agents read `.git/config` directly from that mount. If `.git/config` contains credential settings (`credential.*`, `http.<url>.extraheader` or `http.<url>.cookiefile`, `sendemail.smtppass`, or a remote, `insteadOf`, proxy or LFS URL with an embedded user or password), the runtime refuses to start the agent; remove those settings from `.git/config` or use Docker or Podman.
- **Git-backed shared workspaces**: Git projects that share one working tree across agents (`shared-plain` with a git repository) are refused at start on the Apple `container` runtime, because the runtime cannot mask single files under a writable `.git` root. Use `worktree-per-agent` or `clone-per-agent`, or run the project on Docker or Podman.
- **`.scion` marker files**: A `.scion` project marker file in a shared workspace or an outside-root registered worktree is refused at start; commit a `.scion/` directory to the repository or use a non-shared workspace.

