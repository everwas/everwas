#!/usr/bin/env bash
# The docsite mirrors the contract documents that live, canonically, in the
# repo: the wire protocol, the MCP doc, the sync API, and the ADRs. A mirror
# that nobody is forced to touch goes stale silently -- the page keeps
# rendering, the search
# keeps finding it, and the first person to notice the drift is a user whose
# agent is speaking a protocol the docs no longer describe.
#
# This check fails when a contract file changes in the given range and no
# corresponding docsite page changed with it. Satisfying it is deliberately
# cheap: propagate the edit, or touch the mirror page's note if the change
# genuinely does not affect it. The point is that a human looked.
#
# Usage: scripts/check-docs-mirrors.sh [<git diff range>]
# Default range: HEAD~1..HEAD
set -euo pipefail

range="${1:-HEAD~1..HEAD}"
changed="$(git diff --name-only "$range")"

hit() { grep -qE "$1" <<<"$changed"; }

fail=0

if hit '^docs/nats-subjects\.md$' && ! hit '^docsite/src/content/docs/reference/wire-protocol\.md$'; then
  echo "::error::docs/nats-subjects.md changed but its mirror docsite/src/content/docs/reference/wire-protocol.md did not"
  fail=1
fi

if hit '^docs/mcp\.md$' && ! hit '^docsite/src/content/docs/(reference/mcp-tools|guides/enable-mcp)\.md$'; then
  echo "::error::docs/mcp.md changed but neither docsite mirror (reference/mcp-tools.md, guides/enable-mcp.md) did"
  fail=1
fi

if hit '^docs/adr/' && ! hit '^docsite/src/content/docs/decisions/'; then
  echo "::error::docs/adr/ changed but no page under docsite/src/content/docs/decisions/ did"
  fail=1
fi

if hit '^docs/sync-api\.md$' && ! hit '^docsite/src/content/docs/reference/sync-api\.md$'; then
  echo "::error::docs/sync-api.md changed but its mirror docsite/src/content/docs/reference/sync-api.md did not"
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "The repo files are canonical and the docsite pages mirror them;"
  echo "update the mirror alongside the contract (range checked: $range)."
  exit 1
fi

echo "docs mirrors in sync ($range)"
