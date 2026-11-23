#!/usr/bin/env bash
# PostToolUse hook for Write and Edit: run `gofmt -s -w` on the edited file
# when it is a Go file. Reads the hook's JSON from stdin. Does nothing for
# other files or a missing file.
set -euo pipefail

file=$(jq -r '.tool_input.file_path // empty')
case "$file" in
  *.go) ;;
  *) exit 0 ;;
esac
[ -f "$file" ] || exit 0
gofmt -s -w "$file"
