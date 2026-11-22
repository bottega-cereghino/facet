#!/usr/bin/env bash
# test-change.sh BASE HEAD: fail when HEAD weakens the tests BASE has, unless
# the pull request body, read from PR_BODY, has a line "Test-change: <why>".
#
# A coding agent under pressure deletes or skips a test instead of fixing the
# code. This does not judge whether a change is right; it makes the author say
# why in the PR, where the review reads it. Weakening means: a _test.go file
# deleted; fewer test or fuzz functions, or fewer t.Fatal/t.Error calls, in
# the _test.go files than BASE has; a Skip added; the `go test` line of the
# Makefile changed; fewer steps or jobs in the workflows.
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: $0 BASE HEAD" >&2
  exit 2
fi
base=$1
head=$2

reasons=()

while IFS= read -r f; do
  reasons+=("it deletes $f")
done < <(git diff --name-only --diff-filter=D "$base" "$head" -- '*_test.go')

# count REV REGEX PATHSPEC: how many lines match REGEX in PATHSPEC at REV.
count() {
  git grep -hcE "$2" "$1" -- "$3" | awk -F: '{ n += $NF } END { print n + 0 }'
}

# fewer WHAT REGEX PATHSPEC: note it when HEAD has fewer matches than BASE.
fewer() {
  local before after
  before=$(count "$base" "$2" "$3")
  after=$(count "$head" "$2" "$3")
  if [ "$after" -lt "$before" ]; then
    reasons+=("it leaves $after $1, where $before were")
  fi
}

fewer "test and fuzz functions" '^func (Test|Fuzz)' '*_test.go'
fewer "t.Fatal and t.Error calls" '\.(Fatal|Fatalf|Error|Errorf)\(' '*_test.go'
fewer "workflow steps" '^ *- (name|run|uses):' '.github/workflows/*.yml'
fewer "workflow jobs" '^ *runs-on:' '.github/workflows/*.yml'

if git diff -U0 "$base" "$head" -- '*_test.go' | grep -qE '^\+.*\.Skip(Now|f)?\('; then
  reasons+=("it adds a Skip")
fi
if git diff -U0 "$base" "$head" -- Makefile | grep -qE '^[-+].*go test'; then
  reasons+=("it changes the go test line of the Makefile")
fi

if [ ${#reasons[@]} -eq 0 ]; then
  echo "The tests are not weakened."
  exit 0
fi
printf 'This pull request weakens the tests:\n'
printf '  - %s\n' "${reasons[@]}"
if printf '%s\n' "${PR_BODY:-}" | tr -d '\r' | grep -qE '^Test-change: *[^ ]'; then
  echo "The body says why (Test-change), so it passes."
  exit 0
fi
echo "Add a line 'Test-change: <why>' to the pull request body; the check runs again when the body is edited."
exit 1
