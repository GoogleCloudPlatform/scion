#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

#
# PR Dependency Graph Tool
# ========================
# Analyzes open Pull Requests to determine dependency relationships,
# recommend merge order, and detect file overlaps.
#
# Usage:
#   ./scripts/pr-deps.sh [command] [options]
#
# Commands:
#   graph   Show branch dependency graph (default)
#   order   Show recommended merge order (topological sort)
#   files   Show file overlap matrix across PRs
#
# Options:
#   --author <name>      Filter by author (default: current gh user)
#   --base <branch>      Override default branch detection
#   --dot                Output graph in graphviz DOT format
#   --infer              Infer dependencies via git commit ancestry
#   --all                Show all open PRs regardless of author
#   --repo <owner/repo>  Target a specific repository
#   --no-color           Disable color output
#   --help               Show this help message
#

set -euo pipefail

# --- Colors ---
# shellcheck disable=SC2034 # part of the standard colour palette, kept complete
RED='\033[0;31m'
# shellcheck disable=SC2034 # part of the standard colour palette, kept complete
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
# shellcheck disable=SC2034 # part of the standard colour palette, kept complete
CYAN='\033[0;36m'
BOLD='\033[1m'
# shellcheck disable=SC2034 # part of the standard colour palette, kept complete
DIM='\033[2m'
RESET='\033[0m'

# --- Defaults ---
COMMAND="graph"
AUTHOR=""
BASE_BRANCH=""
DOT_OUTPUT=false
ALL_AUTHORS=false
REPO_FLAG=""
NO_COLOR=false
INFER=false

# --- Usage ---
usage() {
    cat <<'EOF'
PR Dependency Graph Tool

Usage:
  pr-deps.sh [command] [options]

Commands:
  graph   Show branch dependency graph (default)
  order   Show recommended merge order (topological sort)
  files   Show file overlap matrix across PRs

Options:
  --author <name>      Filter by author (default: current gh user)
  --base <branch>      Override default branch detection
  --dot                Output graph in graphviz DOT format (graph command only)
  --infer              Infer dependencies via git commit ancestry (requires
                       local repo with remote tracking branches)
  --all                Show all open PRs regardless of author
  --repo <owner/repo>  Target a specific repository
  --no-color           Disable color output
  --help               Show this help message

Examples:
  pr-deps.sh                           # Show dependency graph for your PRs
  pr-deps.sh graph --all               # Show graph for all open PRs
  pr-deps.sh graph --infer             # Infer hidden dependencies via git
  pr-deps.sh order --author octocat    # Show merge order for octocat's PRs
  pr-deps.sh files                     # Show file overlap matrix
  pr-deps.sh graph --dot | dot -Tpng -o deps.png   # Generate PNG diagram
EOF
}

# --- Dependency checks ---
check_deps() {
    local missing=""
    if ! command -v gh &>/dev/null; then
        missing="$missing  - gh (GitHub CLI)\n"
    fi
    if ! command -v jq &>/dev/null; then
        missing="$missing  - jq\n"
    fi
    if [ -n "$missing" ]; then
        echo "Error: missing required dependencies:" >&2
        echo -e "$missing" >&2
        exit 1
    fi
}

# --- Resolve defaults ---
resolve_author() {
    if [ -n "$AUTHOR" ]; then
        return
    fi
    if [ "$ALL_AUTHORS" = true ]; then
        return
    fi
    AUTHOR=$(gh api user --jq '.login' 2>/dev/null) || {
        echo "Warning: could not detect GitHub user. Use --author or --all." >&2
        exit 1
    }
}

resolve_base_branch() {
    if [ -n "$BASE_BRANCH" ]; then
        return
    fi
    # shellcheck disable=SC2086
    BASE_BRANCH=$(gh repo view $REPO_FLAG --json defaultBranchRef --jq '.defaultBranchRef.name' 2>/dev/null) || {
        BASE_BRANCH="main"
        echo "Warning: could not detect default branch, assuming 'main'." >&2
    }
}

# --- Fetch PR data ---
fetch_prs() {
    local author_filter=""
    if [ -n "$AUTHOR" ]; then
        author_filter="--author $AUTHOR"
    fi

    # shellcheck disable=SC2086
    PR_JSON=$(gh pr list $REPO_FLAG $author_filter \
        --state open \
        --json number,title,headRefName,baseRefName,author \
        --limit 100 2>/dev/null) || {
        echo "Error: failed to fetch PRs. Check gh authentication." >&2
        exit 1
    }

    PR_COUNT=$(echo "$PR_JSON" | jq 'length')
    if [ "$PR_COUNT" -eq 0 ]; then
        local scope="your"
        [ "$ALL_AUTHORS" = true ] && scope="any"
        [ -n "$AUTHOR" ] && scope="$AUTHOR's"
        echo "No open PRs found for $scope account." >&2
        exit 0
    fi
}

# --- Infer dependencies via git commit ancestry ---
# For PRs that all target the default branch, check if one branch's tip
# is an ancestor of another's. If B contains A's commits, B depends on A.
# After finding all ancestry edges, perform transitive reduction to keep
# only direct dependencies, then rewrite PR_JSON baseRefName accordingly.
infer_dependencies() {
    # Collect PRs that target the default branch (candidates for inference)
    local candidates
    candidates=$(echo "$PR_JSON" | jq -r --arg base "$BASE_BRANCH" \
        '.[] | select(.baseRefName == $base) | "\(.number) \(.headRefName)"')

    if [ -z "$candidates" ]; then
        return
    fi

    # Fetch PR head commits into the local repo so ancestry checks work.
    # PR branches live under refs/pull/<number>/head on the remote and
    # are not fetched by a regular 'git fetch origin'.
    echo "Fetching PR head commits for ancestry analysis..." >&2
    local nums=""
    local heads=""
    local resolved_refs=""
    local skipped=""

    while read -r num head; do
        local ref=""
        # 1. Already available locally (tracking branch or checked-out)
        if git rev-parse --verify "origin/$head" &>/dev/null; then
            ref="origin/$head"
        elif git rev-parse --verify "$head" &>/dev/null; then
            ref="$head"
        else
            # 2. Fetch the PR's head ref from the remote
            if git fetch origin "refs/pull/$num/head:refs/pr/$num" &>/dev/null 2>&1; then
                ref="refs/pr/$num"
            else
                skipped="$skipped  #$num ($head): could not fetch\n"
                continue
            fi
        fi
        nums="$nums $num"
        heads="$heads $head"
        resolved_refs="$resolved_refs $ref"
    done <<< "$candidates"

    if [ -n "$skipped" ]; then
        echo "Note: skipping inference for PRs that could not be resolved:" >&2
        echo -e "$skipped" >&2
    fi

    # Convert to arrays
    local -a num_arr head_arr ref_arr
    read -ra num_arr <<< "$nums"
    read -ra head_arr <<< "$heads"
    read -ra ref_arr <<< "$resolved_refs"
    local count=${#num_arr[@]}

    if [ "$count" -lt 2 ]; then
        return
    fi

    # Check pairwise ancestry: edges[i] = space-separated list of indices
    # that i depends on (i.e., whose tip is an ancestor of i's tip)
    local -a ancestors_of
    for ((i = 0; i < count; i++)); do
        ancestors_of[i]=""
        for ((j = 0; j < count; j++)); do
            if [ "$i" -eq "$j" ]; then
                continue
            fi
            # Is j's tip an ancestor of i's tip? If so, i depends on j.
            if git merge-base --is-ancestor "${ref_arr[$j]}" "${ref_arr[$i]}" 2>/dev/null; then
                ancestors_of[i]="${ancestors_of[i]} $j"
            fi
        done
    done

    # Transitive reduction: for each PR, keep only the closest ancestor.
    # If i depends on j and j depends on k, remove the i->k edge.
    # "Closest" = the ancestor whose tip is nearest to i's tip, which is
    # the ancestor that itself has the most ancestors in common with i.
    for ((i = 0; i < count; i++)); do
        local deps="${ancestors_of[$i]}"
        if [ -z "$deps" ]; then
            continue
        fi

        local -a dep_arr
        read -ra dep_arr <<< "$deps"

        # For each ancestor j of i, remove any ancestor k of i where k is
        # also an ancestor of j (meaning j is "closer" to i than k).
        local -a direct_deps
        direct_deps=()
        for j_idx in "${dep_arr[@]}"; do
            local is_transitive=false
            for other_idx in "${dep_arr[@]}"; do
                if [ "$j_idx" -eq "$other_idx" ]; then
                    continue
                fi
                # Is j an ancestor of other? If so, j is further from i than other.
                # Equivalently: is j_idx in ancestors_of[other_idx]?
                local other_anc="${ancestors_of[$other_idx]}"
                case " $other_anc " in
                    *" $j_idx "*)
                        # j is an ancestor of other, which is also an ancestor of i.
                        # So j->i is transitive through other. Skip j.
                        is_transitive=true
                        break
                        ;;
                esac
            done
            if [ "$is_transitive" = false ]; then
                direct_deps+=("$j_idx")
            fi
        done

        # Rewrite PR_JSON: set baseRefName to the direct dependency's headRefName.
        # If multiple direct deps exist (diamond), pick the first (arbitrary but stable).
        if [ ${#direct_deps[@]} -gt 0 ]; then
            local parent_idx="${direct_deps[0]}"
            local parent_head="${head_arr[$parent_idx]}"
            local pr_num="${num_arr[$i]}"
            PR_JSON=$(echo "$PR_JSON" | jq --argjson num "$pr_num" --arg new_base "$parent_head" \
                '[ .[] | if .number == $num then .baseRefName = $new_base else . end ]')
        fi
    done
}

# --- graph command: ASCII tree (rendered entirely in jq) ---
cmd_graph() {
    if [ "$DOT_OUTPUT" = true ]; then
        cmd_graph_dot
        return
    fi

    local scope_label="$AUTHOR"
    [ "$ALL_AUTHORS" = true ] && scope_label="all authors"

    if [ "$NO_COLOR" = true ]; then
        echo "PR Dependency Graph (${scope_label})"
    else
        echo -e "${BOLD}PR Dependency Graph (${scope_label})${RESET}"
    fi
    echo ""

    # jq does all the tree-building and rendering
    echo "$PR_JSON" | jq -r --arg base "$BASE_BRANCH" --arg no_color "$NO_COLOR" --arg all_authors "$ALL_AUTHORS" '
        # Build lookup: branch -> list of PR objects whose base is that branch
        def children_of(branch):
            [ .[] | select(.baseRefName == branch) ];

        # Collect all head branch names
        ( [ .[].headRefName ] ) as $heads |

        # Find root bases: bases that are not any PRs head
        ( [ .[].baseRefName ] | unique | map(select(. as $b | $heads | index($b) | not)) ) as $roots |

        # Recursive tree renderer
        def render(branch; prefix; prs):
            (prs | children_of(branch)) as $kids |
            if ($kids | length) == 0 then empty
            else
                $kids | to_entries[] |
                .key as $idx |
                .value as $pr |
                (if $idx == (($kids | length) - 1) then "└── " else "├── " end) as $connector |
                (if $idx == (($kids | length) - 1) then (prefix + "    ") else (prefix + "│   ") end) as $child_prefix |
                (if $all_authors == "true" then " [\($pr.author.login)]" else "" end) as $author_str |
                (if $no_color == "true" then
                    "\(prefix)\($connector)#\($pr.number) \($pr.headRefName) (\($pr.title))\($author_str)"
                else
                    "\(prefix)\($connector)\u001b[0;36m#\($pr.number)\u001b[0m \u001b[1m\($pr.headRefName)\u001b[0m \u001b[2m(\($pr.title))\u001b[0m\($author_str)"
                end),
                render($pr.headRefName; $child_prefix; prs)
            end;

        # Render each root
        . as $prs |
        $roots[] |
        (if $no_color == "true" then . else "\u001b[0;32m\u001b[1m\(.)\u001b[0m" end),
        render(.; ""; $prs),
        ""
    '
}

cmd_graph_dot() {
    echo "$PR_JSON" | jq -r --arg base "$BASE_BRANCH" '
        "digraph pr_dependencies {",
        "  rankdir=LR;",
        "  node [shape=box, style=rounded];",
        "",
        # PR nodes
        (.[] | "  \"\(.headRefName)\" [label=\"#\(.number): \(.title | gsub("\""; "\\\""))\"];"),
        "",
        # Default branch node
        "  \"\($base)\" [label=\"\($base)\", style=\"filled,rounded\", fillcolor=\"#90EE90\"];",
        "",
        # Edges
        (.[] | "  \"\(.baseRefName)\" -> \"\(.headRefName)\";"),
        "}"
    '
}

# --- order command: topological sort (in jq) ---
cmd_order() {
    local scope_label="$AUTHOR"
    [ "$ALL_AUTHORS" = true ] && scope_label="all authors"

    if [ "$NO_COLOR" = true ]; then
        echo "Recommended Merge Order (${scope_label})"
    else
        echo -e "${BOLD}Recommended Merge Order (${scope_label})${RESET}"
    fi
    echo ""

    echo "$PR_JSON" | jq -r --arg no_color "$NO_COLOR" '
        # Kahn topological sort
        # A PR depends on another if its baseRefName == the others headRefName
        ( [ .[].headRefName ] ) as $heads |

        # in_degree: count of dependencies for each PR
        def in_degree(pr):
            if ($heads | index(pr.baseRefName)) then 1 else 0 end;

        # head_to_pr lookup
        ( reduce .[] as $pr ({}; . + { ($pr.headRefName): $pr.number }) ) as $head_to_pr |

        # Initialize
        . as $prs |
        ( [ $prs[] | { num: .number, head: .headRefName, base: .baseRefName, title: .title, deg: in_degree(.) } ] ) as $nodes |

        # Iterative topological sort
        { sorted: [], remaining: $nodes, step: 1 } |
        until(
            (.remaining | length) == 0 or ([ .remaining[] | select(.deg == 0) ] | length) == 0;
            . as $state |
            ([ $state.remaining[] | select(.deg == 0) ]) as $ready |
            ([ $ready[].head ]) as $ready_heads |
            {
                sorted: ($state.sorted + $ready),
                remaining: [
                    $state.remaining[] |
                    select(.deg != 0) |
                    if (.base as $b | $ready_heads | index($b)) then .deg = (.deg - 1) else . end
                ],
                step: ($state.step + ($ready | length))
            }
        ) |

        # Output sorted entries
        (.sorted | to_entries[] |
            .key as $idx |
            .value as $pr |
            if $no_color == "true" then
                "  \($idx + 1). #\($pr.num) \($pr.head) -> \($pr.base) (\($pr.title))"
            else
                "  \u001b[1m\($idx + 1).\u001b[0m \u001b[0;36m#\($pr.num)\u001b[0m \($pr.head) \u001b[2m->\u001b[0m \u001b[0;32m\($pr.base)\u001b[0m \u001b[2m(\($pr.title))\u001b[0m"
            end
        ),

        # Check for cycles
        if (.remaining | length) > 0 then
            "",
            (if $no_color == "true" then
                "Warning: circular dependency detected! The following PRs form a cycle:"
            else
                "\u001b[0;31mWarning: circular dependency detected! The following PRs form a cycle:\u001b[0m"
            end),
            (.remaining[] | "  #\(.num) \(.head) -> \(.base)")
        else empty end
    '
}

# --- files command: file overlap detection ---
cmd_files() {
    local scope_label="$AUTHOR"
    [ "$ALL_AUTHORS" = true ] && scope_label="all authors"

    if [ "$NO_COLOR" = true ]; then
        echo "File Overlap Analysis (${scope_label})"
    else
        echo -e "${BOLD}File Overlap Analysis (${scope_label})${RESET}"
    fi
    echo ""

    if [ "$PR_COUNT" -gt 20 ]; then
        if [ "$NO_COLOR" = true ]; then
            echo "Warning: fetching file lists for $PR_COUNT PRs, this may take a moment..." >&2
        else
            echo -e "${YELLOW}Warning: fetching file lists for $PR_COUNT PRs, this may take a moment...${RESET}" >&2
        fi
    fi

    # Collect PR numbers
    local pr_numbers
    pr_numbers=$(echo "$PR_JSON" | jq -r '.[].number')

    # Fetch file lists into a temp dir (one file per PR)
    local tmpdir
    tmpdir=$(mktemp -d)
    trap '[ -n "${tmpdir:-}" ] && rm -rf "$tmpdir"' EXIT

    for num in $pr_numbers; do
        # shellcheck disable=SC2086
        gh pr view "$num" $REPO_FLAG --json files --jq '.files[].path' 2>/dev/null | sort > "$tmpdir/$num" || true
    done

    # Build a combined JSON with PR metadata + files for jq to process overlaps
    local combined="[]"
    for num in $pr_numbers; do
        local head
        head=$(echo "$PR_JSON" | jq -r --argjson n "$num" '.[] | select(.number == $n) | .headRefName')
        local files_json
        files_json=$(jq -R -s 'split("\n") | map(select(length > 0))' < "$tmpdir/$num")
        combined=$(echo "$combined" | jq --argjson n "$num" --arg h "$head" --argjson f "$files_json" \
            '. + [{ number: $n, head: $h, files: $f }]')
    done

    # Use jq to find pairwise overlaps
    local result
    result=$(echo "$combined" | jq -r --arg no_color "$NO_COLOR" '
        . as $prs |
        [ range(length) as $i | range($i+1; length) as $j |
            ($prs[$i].files - ($prs[$i].files - $prs[$j].files)) as $common |
            select(($common | length) > 0) |
            {
                a_num: $prs[$i].number,
                a_head: $prs[$i].head,
                b_num: $prs[$j].number,
                b_head: $prs[$j].head,
                common: $common
            }
        ] |
        if length == 0 then
            if $no_color == "true" then
                "No file overlaps detected between PRs."
            else
                "\u001b[0;32mNo file overlaps detected between PRs.\u001b[0m"
            end
        else
            .[] |
            (if $no_color == "true" then
                "#\(.a_num) (\(.a_head)) <-> #\(.b_num) (\(.b_head)): \(.common | length) shared file(s)"
            else
                "\u001b[0;36m#\(.a_num)\u001b[0m (\(.a_head)) \u001b[0;33m<->\u001b[0m \u001b[0;36m#\(.b_num)\u001b[0m (\(.b_head)): \u001b[1m\(.common | length)\u001b[0m shared file(s)"
            end),
            (.common[] | "    \(.)"),
            ""
        end
    ')

    echo "$result"
}

# --- Parse arguments ---
parse_args() {
    local positional_set=false
    while [ $# -gt 0 ]; do
        case "$1" in
            graph|order|files)
                if [ "$positional_set" = false ]; then
                    COMMAND="$1"
                    positional_set=true
                else
                    echo "Error: unexpected argument '$1'" >&2
                    exit 1
                fi
                shift
                ;;
            --author)
                AUTHOR="${2:?--author requires a value}"
                shift 2
                ;;
            --base)
                BASE_BRANCH="${2:?--base requires a value}"
                shift 2
                ;;
            --dot)
                DOT_OUTPUT=true
                shift
                ;;
            --infer)
                INFER=true
                shift
                ;;
            --all)
                ALL_AUTHORS=true
                shift
                ;;
            --repo)
                REPO_FLAG="--repo ${2:?--repo requires a value}"
                shift 2
                ;;
            --no-color)
                NO_COLOR=true
                shift
                ;;
            --help|-h)
                usage
                exit 0
                ;;
            *)
                echo "Error: unknown option '$1'" >&2
                echo "Run 'pr-deps.sh --help' for usage." >&2
                exit 1
                ;;
        esac
    done
}

# --- Main ---
main() {
    parse_args "$@"
    check_deps
    resolve_author
    resolve_base_branch
    fetch_prs

    if [ "$INFER" = true ]; then
        infer_dependencies
    fi

    case "$COMMAND" in
        graph) cmd_graph ;;
        order) cmd_order ;;
        files) cmd_files ;;
    esac
}

main "$@"
