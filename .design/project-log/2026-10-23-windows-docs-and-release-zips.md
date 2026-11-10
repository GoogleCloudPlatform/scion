# Windows CLI: docs page and release zips

Date: 2026-10-23

## What

- New page `docs-site/src/content/docs/hosted/user/windows.md` (sidebar entry
  under the hosted user guide; pointer note in `getting-started/install.md`).
  It covers `scion ssh`, `scion ssh-config` and `scion vscode` first, then the
  supported command set (matches `windowsAllowed` in `cmd/platform_gate.go`),
  the refusal for other commands, how private files are protected on Windows,
  and each refusal message with its fix.
- `build-release.yml` builds `windows/amd64` and `windows/arm64`; each release
  asset is a zip holding `scion.exe` (`scion-windows-amd64.zip`,
  `scion-windows-arm64.zip`). `docs/release-process.md` lists both.

## Checked against the code

Every quoted message on the page was matched to its source string
(`pkg/util/privatefs`, `pkg/credentials`, `pkg/tunnelclient`, `cmd/ssh*.go`,
`cmd/vscode.go`, `cmd/platform_gate.go`). The page also states:

- the characters a `ProxyCommand` word may hold on Windows (`sshCmdQuote`):
  the `scion.exe` path, and for `scion vscode` the known-hosts path under the
  user profile;
- the Hub endpoint character set for ssh, ssh-config and vscode, on every OS
  (`sshEndpointUnsafeRune`);
- the argument check for a `.cmd`/`.bat` VS Code launcher
  (`checkVSCodeBatchArgs`), which runs after the vscode files are written;
- `scion ssh` exit 143 when the console closes (`sshEndSignals` on Windows).
- the `Include` line counts only before the first `Host` or `Match` line
  (`hasSSHIncludeLine`), the Hub label rule for an IP address or `localhost`
  (`sshHubLabelCandidates`), and the non-ASCII characters the word allowlists
  refuse (`isNonASCIIWordRune`).
- shell completion on Windows: an unsupported command completes to nothing,
  not even file names, and `scion project <TAB>` offers no subcommands while
  `scion project use` still runs (`completeGated` in `cmd/platform_gate.go`).
- verifying a downloaded zip against the release's `SHA256SUMS`, which the
  release job writes over every file it builds with `sha256sum` (`build-release.yml`).

## Open

- The docs site build runs in the upstream docs CI; links and anchors on the
  page were checked by hand.
