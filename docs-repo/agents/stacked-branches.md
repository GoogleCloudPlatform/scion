# Stacked branches under squash-merge

Upstream `GoogleCloudPlatform/scion` squash-merges pull requests: when a lower branch lands on `main`, its commits become one new commit. Plain `git rebase main` on a branch stacked on top of it replays the lower branch's original commits onto that squash commit, causing false conflicts (especially in generated files).

- **Record `<old-base>` before your first commit:** run `git rev-parse HEAD` immediately after creating the stacked branch from the lower branch, and include `<old-base>` in your branch notes and PR body. In a shallow clone (`git rev-parse --is-shallow-repository`), confirm `<old-base>` is present locally before restacking (`git cat-file -e <old-base>^{commit}`, or `git fetch --deepen=200 origin <branch>`).
- **Share commit-pinned compare links while stacked:** use `<old-base>...<branch>` alongside `git diff --stat <old-base>...HEAD` to inspect or share only this branch's commits. Avoid `<lower-branch>...<branch>` compare links: force-pushes on `<lower-branch>` shift that diff or make it empty until `<branch>` is restacked. A PR targeting `main` shows the lower branch's unmerged commits too until restacked onto `main`; naming `<old-base>` in the PR body lets reviewers isolate the phase diff.
- **Restack with `git rebase --onto`:** when `<lower-branch>` rebases to `<new-lower-tip>` or squash-merges into `<new-main>`:
  ```bash
  # Refuses a non-ancestor split point before running the rebase
  git merge-base --is-ancestor <old-base> HEAD &&
    GIT_EDITOR=true git rebase --onto <new-base> <old-base>
  ```
- **Regenerate generated files on conflict:** never hand-merge generated catalogs or code (for example `.design/authorization-operation-catalog.md` from `go test -buildvcs=false ./pkg/hub/authzop/...`—the first run fails after rewriting the stale file, so re-run to confirm green—or `pkg/ent` via `go generate ./pkg/ent/...`). Take `<new-base>`'s copy (`git checkout --ours <path> && git add <path>` during rebase), finish the rebase or resolve code files, and re-run the generator.
- **Check commit parity before force-pushing:** run `git range-diff <old-base>..ORIG_HEAD <new-base>..HEAD` (`=` lines mean unchanged patch hunks; anything else—`!`, `<`, or `>`—needs inspection) and `git diff --stat <new-base>...HEAD`, re-run touched tests, then push with `git push --force-with-lease origin HEAD`. If still stacked on an unmerged branch, update your recorded `<old-base>` to `<new-base>` afterwards.
