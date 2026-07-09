# Design: Release Process for Scion

**Date:** 2026-07-09
**Author:** release-arch (architect agent)
**Status:** Approved — user feedback incorporated 2026-07-09
**Branch:** `scion/release-process-doc`
**Inputs:** `/scion-volumes/scratchpad/release-proposal.md`, existing CI/CD workflows in `ptone/scion`

---

## 1. Problem & Goals

Scion currently has no formal release process. The existing `build-release.yml` workflow triggers on **all** tag pushes (`tags: - '*'`), builds multi-arch binaries, and creates a GitHub Release with `prerelease: false` hardcoded. There is no distinction between preview/RC and stable releases, no release branches, and no defined cadence.

### Goals

1. **Two-channel release model.** Preview (RC) builds for early testing; Stable builds promoted from a proven RC. Users who want bleeding-edge opt into preview; everyone else gets stable.
2. **Weekly cadence as a goal, not a mandate.** Aim to cut an RC weekly and promote to stable after a bake period. The cadence is aspirational — skip a week if `main` isn't ready or there aren't enough changes to justify a release. A stable promotion can be deferred if the RC hasn't had enough soak time.
3. **`main` never stops.** Feature work continues merging to `main` while the release branch stabilizes independently.
4. **Minimal process overhead.** One person should be able to execute a full release cycle with a handful of git commands. No release manager role required.
5. **Automated channel routing.** CI differentiates RC from stable based on tag pattern alone — no manual workflow inputs, no manual checkbox toggling.

### Non-Goals

- **Automated release cutting.** The RC branch and tags are created manually (or by a future bot). This design does not include a scheduled GitHub Action that auto-cuts releases.
- **Long-lived support branches.** There is no plan for maintaining `release/v1.1` after `release/v1.2` is cut. Patch releases only apply to the latest release branch.
- **Container image releases.** Container images are built and published through a separate homebrew tap workflow. This design does not change that process.
- **Changelog automation.** The existing daily changelog system (`changelog/`) continues as-is. GitHub's auto-generated release notes (from PR titles) serve as per-release changelogs.
- **Apple binary signing.** Covered by a separate design (`.design/apple-binary-signing.md`).

---

## 2. Proposed Design

### 2.1 Branch and Tag Taxonomy

| Component | Naming Convention | Example | Purpose |
|-----------|-------------------|---------|---------|
| **Development branch** | `main` | — | All feature PRs merge here. CI runs on every push and PR. |
| **Release branch** | `release/vX.Y` | `release/v0.3` | Cut from `main` to freeze features. Lives until stable is tagged. |
| **RC tag** | `vX.Y.Z-rc.N` | `v0.3.0-rc.1` | Triggers a preview build. Published as a GitHub pre-release. |
| **Stable tag** | `vX.Y.Z` | `v0.3.0` | Triggers a stable build. Published as the latest GitHub release. |

**Version number decisions:**

- **The first release under this process is `v0.3.0`.** The `v0.2.x` series is reserved for the existing experimental homebrew-based release process. Starting at `v0.3.0` avoids version collisions and clearly delineates the new release channel.
- Minor version (`Y`) increments with each weekly release cycle (v0.3 → v0.4 → v0.5 → …).
- Patch version (`Z`) is always `0` for the initial release of a cycle. It increments only for hotfix releases after a stable tag has been published.
- RC counter (`N`) starts at 1 and increments with each new candidate on the same release branch.

### 2.2 Weekly Cadence

```
Tuesday                                              Monday (next week)
   |                                                     |
   v                                                     v
 Cut release/vX.Y ──► Tag rc.1 ──► Stabilize ──► Tag vX.Y.0 (stable)
   from main               │          │
                            │          ├── cherry-pick fixes from main
                            │          └── tag rc.2, rc.3 as needed
                            │
                        CI builds preview release
```

**Step-by-step (operator commands):**

```bash
# 1. Cut the release branch and first RC
git checkout main && git pull origin main
git checkout -b release/v0.3
git push origin release/v0.3
git tag v0.3.0-rc.1
git push origin v0.3.0-rc.1

# 2. Fix bugs during stabilization
#    Always merge fixes to main first, then cherry-pick:
git checkout release/v0.3
git cherry-pick <commit-sha>
git push origin release/v0.3
git tag v0.3.0-rc.2
git push origin v0.3.0-rc.2

# 3. Promote to stable (when RC has baked sufficiently)
git checkout release/v0.3
git tag v0.3.0
git push origin v0.3.0
```

**Key rule:** Bug fixes are merged to `main` first, then cherry-picked to the release branch. This ensures `main` never regresses and the release branch does not need to be merged back.

### 2.3 CI/CD Changes to `build-release.yml`

The current workflow needs three modifications:

#### 2.3.1 Tighten tag triggers

**Current:**
```yaml
on:
  push:
    tags:
      - '*'
```

**Proposed:**
```yaml
on:
  push:
    tags:
      - 'v[0-9]+.[0-9]+.[0-9]+'        # Stable: v0.1.0, v1.2.3
      - 'v[0-9]+.[0-9]+.[0-9]+-rc.*'   # Preview: v0.1.0-rc.1
```

This prevents accidental release builds from non-version tags.

#### 2.3.2 Add release-type detection step

Insert a step after checkout to determine the release channel:

```yaml
- name: Determine release channel
  id: channel
  run: |
    TAG="${GITHUB_REF#refs/tags/}"
    if [[ "$TAG" == *"-rc."* ]]; then
      echo "prerelease=true" >> $GITHUB_OUTPUT
      echo "channel=preview" >> $GITHUB_OUTPUT
    else
      echo "prerelease=false" >> $GITHUB_OUTPUT
      echo "channel=stable" >> $GITHUB_OUTPUT
    fi
    echo "tag=$TAG" >> $GITHUB_OUTPUT
```

#### 2.3.3 Wire prerelease flag into the release step

**Current:**
```yaml
- name: Create Release
  uses: softprops/action-gh-release@v2
  with:
    files: release/*
    draft: false
    prerelease: false
    generate_release_notes: true
```

**Proposed:**
```yaml
- name: Create Release
  uses: softprops/action-gh-release@v2
  with:
    files: release/*
    draft: false
    prerelease: ${{ steps.channel.outputs.prerelease == 'true' }}
    generate_release_notes: true
    name: ${{ steps.channel.outputs.tag }}
```

### 2.4 Hotfix Releases (Post-Stable)

If a critical bug is found after a stable release:

1. Cherry-pick the fix from `main` onto the **existing** release branch.
2. Tag a patch release: `v0.3.1-rc.1` → bake → `v0.3.1`.
3. The same CI pipeline handles it — no special workflow.

### 2.5 Release Branch Lifecycle

- **Creation:** Manual, when the release manager decides `main` is ready.
- **Deletion:** Release branches are **not deleted**. They serve as a historical record and make `git describe` work correctly. GitHub will archive them naturally. (Revisit if branch count becomes unwieldy, unlikely at weekly cadence.)

### 2.6 Version Embedding

The existing `hack/version.sh` and `build-release.yml` version injection via `-ldflags` already works correctly:
- `git describe --tags --exact-match` extracts the tag name.
- The tag is injected into `pkg/version.Version`.
- No changes needed. RC tags like `v0.1.0-rc.1` will appear in `scion version` output as-is, which is the desired behavior for identifying preview builds.

---

## 3. Alternatives Considered

### 3.1 Tag-only workflow (no release branches)

**Approach:** Tag directly on `main`. No `release/vX.Y` branches.

**Why rejected:** Without a release branch, there is no way to cherry-pick stabilization fixes without also pulling in new feature commits. The RC would be a moving target. This works for projects with very low commit velocity, but Scion has daily commits and the changelog shows continuous activity.

### 3.2 GitHub Release drafts as the preview channel

**Approach:** Create a draft release for preview, then un-draft it for stable. No separate tag patterns.

**Why rejected:** Draft releases are not visible to users at all — they can't serve as a preview channel. GitHub pre-releases (what this design uses) are visible but clearly marked, which is the correct UX for "available but not recommended for production."

### 3.3 CalVer instead of SemVer

**Approach:** Use calendar versioning like `2026.28` (year.week) instead of `v0.1.0`.

**Why rejected:** SemVer is the Go ecosystem convention. `go install` and tools like `pkg.go.dev` understand SemVer. CalVer would require custom tooling and confuse Go users. Additionally, SemVer's major version bump provides a clear signal for breaking changes when the project reaches 1.0.

### 3.4 Fully automated weekly release via scheduled GitHub Action

**Approach:** A cron-triggered workflow that automatically cuts `release/vX.Y`, tags RC, and promotes after N days.

**Why rejected for now:** The project is pre-1.0 and the team is small. Full automation adds complexity (what if main is broken on Tuesday? what if there are no changes worth releasing?). Manual cutting with automated CI is the right balance. The design is compatible with adding automation later — the tag patterns and branch conventions don't change.

---

## 4. Migration / Rollout

The `v0.2.x` series exists in a separate homebrew tap release process. This design introduces a parallel GitHub-native release channel starting at `v0.3.0`. The two processes are independent and do not conflict.

1. **Update `build-release.yml`** with the tightened tag patterns and prerelease detection (§2.3). This is backward-compatible: the workflow simply stops triggering on non-version tags. Existing `v0.2.x` tags from the homebrew process will continue to match the SemVer patterns and build correctly (they have no `-rc.` suffix, so they'll be treated as stable — matching current behavior).
2. **Cut the first release** using the operator commands in §2.2. The first release will be `v0.3.0-rc.1`.
3. **Document the process** — this design doc serves as the operator reference.

No feature flags, no gradual rollout, no breaking changes.

---

## 5. Resolved Questions

1. **Starting version number.** → **`v0.3.0`**. The `v0.2.x` series is used by the existing homebrew tap release process.
2. **Weekly cadence.** → **Weekly is a goal, not a strict requirement.** Stable promotion can be skipped if the RC isn't ready. No fixed day-of-week mandated.
3. **Minimum bake time.** → **Maintainer judgment.** No hard minimum; the weekly cadence provides a natural ~5-day window, but promotion can be deferred.
4. **Container images.** → **Out of scope.** Handled by the homebrew tap process, which also manages image builds.

## 5.1 Remaining Open Questions

1. **Branch protection.** Should `release/v*` branches get branch protection rules (require PR for cherry-picks, require CI to pass)? This can be decided later without affecting the design.

---

## 6. Implementation Phases

### Phase 1: Workflow update (one commit)
- Modify `.github/workflows/build-release.yml`:
  - Tighten tag triggers to SemVer patterns.
  - Add release-channel detection step.
  - Wire `prerelease` flag dynamically.
- No functional change until a matching tag is pushed.

### Phase 2: First release (operational, not code)
- Cut `release/v0.3` from `main`.
- Tag `v0.3.0-rc.1` and verify the preview build.
- After bake period, tag `v0.3.0` and verify the stable build.

### Phase 3: Documentation (one commit)
- Commit this design doc to `.design/release-process.md`.
- Optionally add a `RELEASING.md` in the repo root with the operator commands from §2.2 as a quick-reference runbook.

---

## 7. Acceptance Criteria

- [ ] Pushing a `vX.Y.Z-rc.N` tag produces a GitHub Release marked as **pre-release**, with multi-arch binaries attached.
- [ ] Pushing a `vX.Y.Z` tag produces a GitHub Release marked as **latest release**, with multi-arch binaries attached.
- [ ] Pushing a non-SemVer tag (e.g., `test-tag`) does **not** trigger a release build.
- [ ] `scion version` on a binary built from an RC tag shows the RC version string (e.g., `scion v0.3.0-rc.1 (commit abcdef01)`).
- [ ] `scion version` on a binary built from a stable tag shows the stable version string (e.g., `scion v0.3.0 (commit abcdef01)`).
- [ ] The release branch can receive cherry-picked commits and produce subsequent RC tags without affecting `main`.
- [ ] GitHub auto-generated release notes correctly list PRs/commits included in each release.
