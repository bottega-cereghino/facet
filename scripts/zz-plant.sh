#!/usr/bin/env bash
# Planted to see the shfmt gate fail once; reverted in the next commit.
set -euo pipefail
if [ -n "${HOME}" ]; then
        echo "badly indented"
fi
