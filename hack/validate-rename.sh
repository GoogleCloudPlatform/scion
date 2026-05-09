#!/bin/bash

# hack/validate-rename.sh
# Reports occurrences of 'grove' (case-insensitive) in the codebase, with exclusions.

# Default to non-strict mode
STRICT=false
for arg in "$@"; do
  if [[ "$arg" == "--strict" ]]; then
    STRICT=true
  fi
done

# Define exclusions based on requirements:
# .design/grove-to-project-rename.md, .scratch/ directory, /scion-volumes/scratchpad/, go.sum file, changelog/ directory, .git/ directory.

# Directories to exclude
EXCLUDE_DIRS=(
  ".git"
  ".scratch"
  "changelog"
  "scratchpad" # matches /scion-volumes/scratchpad/
  ".scion"
)

# Files to exclude
EXCLUDE_FILES=(
  "go.sum"
  "grove-to-project-rename.md" # matches .design/grove-to-project-rename.md
)

# Build grep command
# -r: recursive
# -i: case-insensitive
# -c: count matches per file
# -I: ignore binary files
GREP_CMD="grep -r -i -I -c"

for dir in "${EXCLUDE_DIRS[@]}"; do
  GREP_CMD+=" --exclude-dir=$dir"
done

for file in "${EXCLUDE_FILES[@]}"; do
  GREP_CMD+=" --exclude=$file"
done

# Execute grep and capture results
# We search in the current directory (.)
# grep -v ":0$" filters out files with zero matches
RESULTS=$($GREP_CMD "grove" . | grep -v ":0$")

if [[ -z "$RESULTS" ]]; then
  echo "No occurrences of 'grove' found (respecting exclusions)."
  echo "----------------------------------------"
  echo "Grand Total: 0"
  exit 0
fi

# Print sorted results
echo "Occurrences of 'grove' per file (sorted descending):"
echo "$RESULTS" | awk -F: '{ printf "%d\t%s\n", $2, $1 }' | sort -rn

# Calculate grand total
TOTAL=$(echo "$RESULTS" | awk -F: '{sum += $2} END {print sum}')

echo "----------------------------------------"
echo "Grand Total: $TOTAL"

# Strict mode: exit non-zero if any matches found
if [[ "$STRICT" == "true" && "$TOTAL" -gt 0 ]]; then
  echo "ERROR: 'grove' found in strict mode."
  exit 1
fi

exit 0
