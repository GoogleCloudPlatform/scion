# Phase 5 live runbook: scion ssh, ssh-config, tunnels and VS Code

This runbook has two parts:

- **Part A** walks through connecting to an agent with `scion ssh` and with
  VS Code Remote-SSH, starting from a workstation with no scion SSH setup.
  It is the live check for design criterion 9.
- **Part B** runs the workstation checks of criterion 25 (a) to (e) against
  a test hub.

Criterion numbers refer to the Conduit design, v2.21.8.

Both parts read as a walkthrough first. Each step ends with a **Record**
line saying what to capture. The checklists at the end of each part collect
the expected results in one place.

Run everything against a **test hub**, never a production hub.

## Conventions

| Placeholder | Meaning |
|---|---|
| `HUB_URL` | The test hub's URL, for example `https://hub.test.example.com` |
| `HUB` | The hub label in SSH host names. `scion ssh-config --write` picks it, and it is the suffix of the include file `~/.ssh/config.d/scion-HUB` |
| `PROJECT`, `AGENT` | Slugs of a project on the hub and a running agent in it |
| `HOST` | `scion.HUB.PROJECT.AGENT`, the SSH host name of that agent |

In the Record lines, "exit code" means the value of `echo $?` straight
after the command. A "one-line error" is a single line on stderr, with no
colour codes and no usage text. Piping stderr through `cat -v` shows any
terminal control codes as `^[`.

What you need:

- a scion CLI built from the version under test (`scion version` prints
  it);
- OpenSSH (`ssh`, and `ssh-agent` for check A6);
- VS Code with the Remote-SSH extension (Part A);
- an account on the test hub that may attach to and reach ports of agents
  in at least two projects;
- for Part B (d), a second non-production hub you can log in to.

---

## Part A: scion ssh and VS Code Remote-SSH (criterion 9)

### A1. Start from a clean workstation

Move any earlier scion SSH files out of the way, and remove an existing
`Include` line for them, so that the run starts from nothing:

```sh
mkdir -p ~/scion-ssh-backup
mv ~/.ssh/config.d/scion-* ~/scion-ssh-backup/ 2>/dev/null
mv ~/.scion/ssh ~/scion-ssh-backup/ 2>/dev/null
cp ~/.ssh/config ~/scion-ssh-backup/ssh-config 2>/dev/null
sed -i.bak '/config\.d\/scion/d' ~/.ssh/config   # remove an existing Include line
grep -n 'config.d/scion' ~/.ssh/config    # expect no output
scion version
```

**Record:** the `scion version` output, and that the `grep` printed
nothing.

### A2. Log in

```sh
scion hub auth login --hub HUB_URL
```

The browser login completes, and the command returns 0.

**Record:** the exit code.

### A3. Write the SSH configuration

```sh
scion ssh-config --write --hub HUB_URL
echo "exit $?"
head -1 ~/.ssh/config
ls -ld ~/.ssh/config.d ~/.scion/ssh
ls -l ~/.ssh/config.d/scion-HUB ~/.scion/ssh/known_hosts-HUB
grep -A6 "^Host scion\.HUB\.PROJECT\.AGENT$" ~/.ssh/config.d/scion-HUB
```

Expected:

- exit 0;
- the first line of `~/.ssh/config` is `Include ~/.ssh/config.d/scion-*`;
- one include file `~/.ssh/config.d/scion-HUB` and one known-hosts file
  `~/.scion/ssh/known_hosts-HUB`, both mode `-rw-------`;
- a directory that the command created has mode `drwx------`
  (`~/.scion/ssh` always, after A1; `~/.ssh/config.d` only if it did not
  exist before). A directory that already existed keeps its mode;
- a `Host scion.HUB.PROJECT.AGENT` block with these lines:
  - `ProxyCommand <absolute path of scion> tunnel --stdio --ssh --hub HUB_URL --project PROJECT AGENT`
  - `HostKeyAlias scion.HUB.PROJECT.AGENT`
  - `UserKnownHostsFile ~/.scion/ssh/known_hosts-HUB`
  - `StrictHostKeyChecking yes`
- the known-hosts file has one line for `scion.HUB.PROJECT.AGENT` that
  holds a public key (`ssh-ed25519 AAAA...`), not a fingerprint.

**Record:** the command output, the first line of `~/.ssh/config`, the
`ls -ld` and `ls -l` output, the agent's Host block, and its known-hosts line.

### A4. Open a shell with scion ssh

```sh
scion ssh --hub HUB_URL PROJECT/AGENT
```

A shell prompt in the agent appears. In it:

```sh
whoami; pwd; ls /workspace
exit
```

The command returns the shell's exit status (0 after `exit`).

Check that `scion ssh` creates no tmux session. Run this before and while
another `scion ssh` session is open:

```sh
scion ssh --hub HUB_URL PROJECT/AGENT tmux list-sessions
```

The two outputs are the same.

**Record:** the `whoami` and `pwd` output, the exit code, and both
`tmux list-sessions` outputs.

### A5. Connect VS Code

1. In VS Code, run **Remote-SSH: Connect to Host...**. The list includes
   `scion.HUB.PROJECT.AGENT`. Choose it.
2. VS Code installs and starts its server in the agent. The status bar
   shows `SSH: scion.HUB.PROJECT.AGENT`.
3. **File > Open Folder...**, then choose a folder under `/workspace`.
4. Open an existing file, and check that its content shows.
5. Edit the file and save it. Then check the change from a terminal on the
   workstation:

   ```sh
   scion ssh --hub HUB_URL PROJECT/AGENT cat /workspace/<that file>
   ```

6. Open a terminal in VS Code (**Terminal > New Terminal**) and run
   `hostname; pwd`.
7. Force an interruption. End the tunnel process on the workstation:

   ```sh
   pkill -f 'tunnel --stdio --ssh'
   ```

   VS Code shows that it is reconnecting, then reconnects without user
   action. The open folder and editor come back, and a new terminal works.

**Record:** for each of steps 2 to 7, a pass or fail note. Also record a
screenshot or the Remote-SSH output log showing the connection and the
reconnect, the `cat` output after the save, and the terminal output.
(Share screenshots in the review thread. Don't commit them.)

### A6. Forwarding checks

Run these from a workstation terminal. Each check states only the
expected result. For the refused checks, `ssh` reports that the channel or
request failed, and with `ExitOnForwardFailure=yes` it exits non-zero.

First start a listener in the agent for the checks that should succeed:

```sh
scion ssh --hub HUB_URL PROJECT/AGENT 'nohup python3 -m http.server 8000 --bind 127.0.0.1 >/dev/null 2>&1 &'
```

| Check | Command | Expected |
|---|---|---|
| direct-tcpip to `localhost` | `ssh -N -o ExitOnForwardFailure=yes -L 18000:localhost:8000 HOST &` then `curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18000/` | succeeds (`200`) |
| direct-tcpip to `127.0.0.1` | `ssh -W 127.0.0.1:8000 HOST </dev/null` | succeeds (no channel error) |
| direct-tcpip to a non-loopback address | `ssh -W 10.0.0.1:80 HOST </dev/null` | refused |
| direct-tcpip to a public name | `ssh -W example.com:80 HOST </dev/null` | refused |
| direct-tcpip to `localhost.localdomain` | `ssh -W localhost.localdomain:8000 HOST </dev/null` | refused |
| direct-tcpip to `foo.localhost` | `ssh -W foo.localhost:8000 HOST </dev/null` | refused |
| direct-tcpip to `localhost.` | `ssh -W localhost.:8000 HOST </dev/null` | refused |
| remote forwarding (`-R`) | `ssh -N -o ExitOnForwardFailure=yes -R 9001:localhost:22 HOST` | refused (`remote port forwarding failed`, non-zero exit) |
| agent forwarding (`-A`) | with a local `ssh-agent` holding a key, first `ssh HOST 'echo "sock=$SSH_AUTH_SOCK"'`, then `ssh -A HOST 'echo "sock=$SSH_AUTH_SOCK"'` | refused (the two outputs match: `sock=` is empty unless the container environment sets it) |

Streamlocal connections are made with the container user's permissions.

When done, stop the `-L` forward (`kill %1`) and the listener in the
agent.

**Record:** each command and its output, with the exit code for each
refused check.

### A7. Re-incarnate the agent

1. Note the agent's current known-hosts line:
   `grep scion.HUB.PROJECT.AGENT ~/.scion/ssh/known_hosts-HUB`.
2. Re-incarnate the agent: `scion reincarnate --hub HUB_URL AGENT` (from
   the project, or with `--project PROJECT`). Wait until it is running
   again.
3. Connect with VS Code (or `ssh HOST true`). The tunnel prints
   `agent was recreated; run scion ssh-config --write` on stderr (in the
   Remote-SSH output log, or in the terminal for `ssh`), and then ssh
   refuses the connection with a host key verification failure. The same
   hint may also appear under `scion ssh` while the `ssh-config` pin is
   stale; that is expected.
4. Run `scion ssh-config --write --hub HUB_URL`. It prints
   `host key for scion.HUB.PROJECT.AGENT changed (agent was recreated); known_hosts line rewritten`
   and exits 0.
5. The known-hosts line now holds a different key.
6. Connect again with VS Code. The connection succeeds.

**Record:** the old and new known-hosts lines, the refusal output from
step 3, the `ssh-config` output from step 4, and that step 6 connected.

### A8. Restore

Only if you want the earlier setup back instead of the one written in
this run:

```sh
rm -f ~/.ssh/config.d/scion-*
mv ~/scion-ssh-backup/scion-* ~/.ssh/config.d/ 2>/dev/null
[ -d ~/scion-ssh-backup/ssh ] && rm -rf ~/.scion/ssh && mv ~/scion-ssh-backup/ssh ~/.scion/ssh
cp ~/scion-ssh-backup/ssh-config ~/.ssh/config 2>/dev/null
```

### Part A checklist

| # | Check | Expected | Issue |
|---|---|---|---|
| A3 | include file, known-hosts file, single `Include` line, Host block, public-key pin | as listed in A3 | #2811 |
| A4 | `scion ssh` opens a shell; returns the remote status | succeeds | #2811 (command), #2810 (server) |
| A4 | no tmux session from `scion ssh` | `tmux list-sessions` unchanged | #2810 |
| A5.2 | VS Code installs and starts its server | succeeds | #2810, #2811 |
| A5.3 | open a folder under `/workspace` | succeeds | #2810 |
| A5.4-5 | read and write a file | succeeds | #2810 |
| A5.6 | terminal | succeeds | #2810 |
| A5.7 | reconnect after a forced interruption | succeeds | #2810, #2811 |
| A6 | direct-tcpip to `localhost` and `127.0.0.1` | succeeds | #2810 |
| A6 | direct-tcpip to non-loopback addresses or any other name | refused | #2810 |
| A6 | `-R` | refused | #2810 |
| A6 | agent forwarding | refused | #2810 |
| A7 | changed host key reported and replaced; connection succeeds again | as listed in A7 | #2811 |

**How a pass maps to the issues.** All Part A rows passing is the live
evidence for criterion 9. The #2810 rows are the live acceptance of the
embedded SSH server (5.3). The #2811 rows are the live acceptance of
`scion ssh` and `scion ssh-config` (5.4). A failed row blocks the issue in
its last column. Record the failure with its output.

---

## Part B: workstation test hub run of criterion 25 (a) to (e)

Set up once:

- two projects on the test hub, `P1` and `P2`;
- running agents `A1` and `A2` in `P1`, and `A3` in `P2`;
- in each agent, a listener on port 8000 that is exposed:

  ```sh
  scion ssh --hub HUB_URL P1/A1 'nohup python3 -m http.server 8000 --bind 127.0.0.1 >/dev/null 2>&1 & sciontool expose 8000'
  ```

  (and the same for `P1/A2` and `P2/A3`).

A few sub-items of criterion 25 are CI only. They are listed, with the
reason, under "CI only" after the checklist.

### B(a). Tunnels to several agents in two projects

1. In three terminals, open one tunnel each:

   ```sh
   scion tunnel --hub HUB_URL --project P1 A1 --port 8000 --local-port 18001
   scion tunnel --hub HUB_URL --project P1 A2 --port 8000 --local-port 18002
   scion tunnel --hub HUB_URL --project P2 A3 --port 8000 --local-port 18003
   ```

   Then `curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:1800N/`
   for N = 1, 2, 3: each prints `200`.
2. Stop the first tunnel with Ctrl-C. It exits 130. The other two still
   answer `200`.
3. Withdraw access to one agent. Precondition: port 8000 is not in the
   operator allow-list for the agent, and auto-expose is off in the agent.
   In a fourth terminal, hold a stdio tunnel open to `A3`:

   ```sh
   (sleep 600) | scion tunnel --hub HUB_URL --project P2 A3 --port 8000 --stdio; echo "exit $?"
   ```

   Then run `scion ssh --hub HUB_URL P2/A3 sciontool unexpose 8000`. The
   held stdio tunnel exits 7 within one recheck interval (60s at the
   default) with `Error: tunnel closed by policy (authz_expired)`. The
   listener tunnel to `A3` (port 18003) refuses its next connection, and
   exits 5. The `A2` tunnel still answers `200`.
4. Stop `A2` (`scion stop --hub HUB_URL --project P1 A2`) and run
   `scion tunnel --hub HUB_URL --project P1 A2 --port 8000 --stdio </dev/null`:
   exit 9, `Error: the agent is not running (agent_not_running)`.
5. *Optional, test hub only, with the hub owner's agreement:* with the
   broker of `A1` stopped, the same command for `A1` exits 10 with
   `Error: the agent's runtime broker is not connected (runtime_broker_unavailable)`.
6. If you stopped the broker in step 5, restart it. Then open an `A1`
   tunnel as in step 1 and check that it answers `200`.
7. A hub without tunnels. This needs a test hub whose REST API works and
   whose conduit experiment is off (`HUB_OFF_URL`), with an agent `P/A` on
   it:

   ```sh
   scion tunnel --hub HUB_OFF_URL --project P A --port 8000 --stdio </dev/null; echo "exit $?"
   ```

   Exit 11 (never 4), with `Error: the hub at HUB_OFF_URL does not support tunnels (HTTP <code>)`.
   The hub itself answers 404; a front proxy may answer another code, such
   as 405 or 501. Exit 11 is the check.
   If no such hub is available, this check is CI only (see "CI only"
   below).

**Record:** each command, its exit code and stderr line, and the `curl`
results.

### B(b). Project-scoped token

1. Create a token scoped to `P1`:

   ```sh
   scion hub token create --hub HUB_URL --project P1 --name tunnel-check --scopes agent:port_access,agent:read,project:read --expires 1h
   ```

2. Use it from a separate home directory, so that the stored login isn't
   used:

   ```sh
   export SCION_HUB_TOKEN=<the token>
   unset SCION_DEV_TOKEN SCION_DEV_TOKEN_FILE
   export HOME=$(mktemp -d)
   scion tunnel --hub HUB_URL --project P1 A1 --port 8000 --local-port 18011 &
   curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18011/      # 200
   scion tunnel --hub HUB_URL --project P2 A3 --port 8000 --stdio </dev/null; echo "exit $?"
   ```

   The `P1` tunnel succeeds. The `P2` tunnel exits 4 with a one-line
   not-found error: a project that the token cannot see reads as not
   found. The `P1` tunnel still answers `200` afterwards, and its session
   stays up.
3. Revoke the token afterwards (`scion hub token revoke` in your normal
   shell).

**Record:** the exit codes and stderr lines, and the `curl` results.

### B(c). Expired credential

1. Create a token with the shortest expiry the hub accepts (for example
   `--expires 5m`, with the scopes of B(b)) and use it as in B(b),
   including `unset SCION_DEV_TOKEN SCION_DEV_TOKEN_FILE`.
2. Before the expiry, hold a stdio tunnel open to `P1/A1`, with its input
   on a named pipe:

   ```sh
   mkfifo /tmp/held-in
   scion tunnel --hub HUB_URL --project P1 A1 --port 8000 --stdio </tmp/held-in >/tmp/held-out &
   exec 3>/tmp/held-in
   ```

3. After the expiry, open another tunnel:
   `scion tunnel --hub HUB_URL --project P1 A2 --port 8000 --stdio </dev/null`.
   It exits 3, with a one-line error ending in
   `log in with: scion hub auth login --hub HUB_URL`.
4. The held stream from step 2 still carries data:

   ```sh
   printf 'GET / HTTP/1.0\r\nHost: localhost\r\n\r\n' >&3
   sleep 2; head -1 /tmp/held-out     # HTTP/1.0 200 OK
   exec 3>&-; rm -f /tmp/held-in /tmp/held-out
   ```

**Record:** the exit code and stderr line from step 3, and the step 4
result.

### B(d). ssh-config for two hubs and two projects

1. Log in to both hubs (`scion hub auth login --hub HUB_URL` and
   `--hub HUB2_URL`), then run `scion ssh-config --write --hub HUB_URL`
   and `scion ssh-config --write --hub HUB2_URL`. Expected:
   - two include files `~/.ssh/config.d/scion-*` and two known-hosts files
     `~/.scion/ssh/known_hosts-*`, one of each per hub;
   - exactly one `Include ~/.ssh/config.d/scion-*` line, at the top of
     `~/.ssh/config` (`grep -c 'config.d/scion' ~/.ssh/config` prints 1);
   - one `Host scion.<hub>.<project>.<agent>` block per agent, across
     both projects.
2. If the hub allows it, create project `a-b` with agent `c` and project
   `a` with agent `b-c`. After regeneration, the two Host names differ
   (`scion.HUB.a-b.c` and `scion.HUB.a.b-c`).
3. Delete an agent (`scion delete ...`), then run
   `scion ssh-config --write --hub HUB_URL` again. Its Host block and its
   known-hosts line are gone.
4. Hub unreachable. Note `sha256sum ~/.ssh/config.d/scion-HUB ~/.scion/ssh/known_hosts-HUB`,
   disconnect the workstation from the network, and run
   `scion ssh-config --write --hub HUB_URL`. It exits non-zero with a
   one-line error. After reconnecting, the checksums are unchanged.
5. Minimal PATH and an unrelated working directory. Copy the
   `ProxyCommand` value of a running agent's Host block and run it with an
   empty environment:

   ```sh
   env -i HOME="$HOME" PATH=/nonexistent /bin/sh -c 'cd / && exec <ProxyCommand value>' </dev/null | head -c 64
   ```

   The output starts with `SSH-2.0-`.
6. Deleted agent. Keep the old Host block of the agent deleted in step 3
   (save a copy of the include file before step 3), and run its
   `ProxyCommand` value the same way, timing it with `time`. It exits 4
   within 5s with a one-line error that says the agent was not found.
   Then `ssh -F <saved include file> <that host> true` exits 255 and
   shows the same line.

**Record:** the `ls` of both directories, the `grep -c` count, the Host
names, the checksums before and after step 4, and the exit codes, times
and stderr lines of steps 4 to 6.

### B(e). No credential

From a shell with no stored login and no `SCION_HUB_TOKEN`
(`export HOME=$(mktemp -d); unset SCION_HUB_TOKEN SCION_DEV_TOKEN SCION_DEV_TOKEN_FILE`):

```sh
scion tunnel --stdio --ssh --hub HUB_URL --project P1 A1 2>&1 >/dev/null | cat -v; echo "exit ${PIPESTATUS[0]}"
scion ssh --hub HUB_URL P1/A1 2>&1 >/dev/null | cat -v; echo "exit ${PIPESTATUS[0]}"
```

(`PIPESTATUS` is bash; in zsh use `${pipestatus[1]}`.)

Expected:

- `scion tunnel` exits 3, `scion ssh` exits 255;
- each prints one line with no `^[` sequences and no usage text, naming
  the hub and the login command;
- each returns at once, without waiting for input from the terminal, and
  neither asks to register or sync a project.

Then, with your normal login, open a tunnel in another terminal, and run
status in the same environment as that tunnel (the same `HOME`, and the
same `XDG_RUNTIME_DIR` if set). Status reads the local state of running
tunnel processes.

```sh
scion tunnel status --json --hub HUB_URL
```

The output is an object with an `entries` array. Each entry has
`endpoint`, a `credential` object (`kind`, `subject`, `scope`) with no
token value, `session_ids`, and `tunnels` entries with `agent`,
`project`, `kind`, `port`, `opened_at` and `authz_deadline`.

**Record:** both stderr lines and exit codes, and the JSON output (check
that it holds no token before sharing it).

### Part B checklist

| Item | Check | Expected |
|---|---|---|
| (a) | three tunnels, two projects, concurrently | succeeds (`200` each) |
| (a) | closing one leaves the others | succeeds |
| (a) | withdrawing access to one agent | the held stdio tunnel exits 7; the listener refuses its next connection with exit 5; the others continue |
| (a) | stopped agent | exit 9 |
| (a) | broker offline (optional) | exit 10 |
| (a) | hub with REST working and conduit off | exit 11, never 4 (CI only if no such hub) |
| (b) | scoped token, inside its project | succeeds |
| (b) | scoped token, project it cannot see | exit 4, one-line not-found error; the open tunnel continues |
| (c) | expired credential at a new tunnel | exit 3 with the login line; the open tunnel continues |
| (d) | two hubs: one include file and one known-hosts file per hub, one `Include` line | as listed |
| (d) | `a-b`/`c` and `a`/`b-c` | distinct Host names |
| (d) | regeneration after a delete | block and known-hosts line removed |
| (d) | regeneration with the hub unreachable | non-zero exit; files unchanged |
| (d) | ProxyCommand with a minimal PATH from `/` | succeeds |
| (d) | ProxyCommand to a deleted agent | exit 4 within 5s; `ssh` exit 255 with that line |
| (e) | no credential | `scion tunnel` exit 3, `scion ssh` exit 255, one plain line each, no prompt |
| (e) | `tunnel status --json` | endpoint, credential identity without secret, open tunnels |

**CI only** (the evidence names the tests):

- (a) Three or more tunnels on one session, and the session cap (`429 rate_limited`): CI only, because the CLI opens one session per process; these become live checks with the multiplexer (5.7).
- (c) The retry on a fresh session after a 401 on an admitted session, and the refusal of a changed identity: CI only, because the CLI opens one session per process; these become live checks with the multiplexer (5.7).
- (a) Exit 11 for a hub without tunnels, if no test hub with REST working and the conduit experiment off is available: CI only, because the CLI checks the credential over REST before the dial, and an endpoint without the hub API gives a different exit.
- (b) A per-tunnel hub refusal for scope (exit 5): CI only, because no visible-project scope refusal is reachable from the CLI. The hub test TestConduitTunnel_ProjectScopedTokenRefusedPerTunnel proves it.
- (d) A truncated agent listing leaves the files unchanged: CI only, because a live hub can't be made to return a truncated listing.
