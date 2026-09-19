#!/bin/sh
# SessionStart hook: list the open Docket items for the repo this session runs
# in, so the agent knows what it can pick up and closes items it completes.
#
# Reads the docket MCP server's url and token from ~/.claude.json (the same
# entry `claude mcp add` writes), so the token lives in one place. Prints
# nothing — and never fails the session — when there is no config, the server
# is unreachable, the token is publish-only, or the scope has no open items.
set -u

command -v jq >/dev/null 2>&1 || exit 0
command -v curl >/dev/null 2>&1 || exit 0

# The session's cwd arrives on stdin; fall back to the shell's cwd.
cwd="$(jq -r '.cwd // empty' 2>/dev/null)"
[ -n "$cwd" ] && cd "$cwd" 2>/dev/null

# The `docket` server entry: user scope first, then this project's, then any
# project's — one backlog, so any entry is the right one.
cfg="$HOME/.claude.json"
[ -r "$cfg" ] || exit 0
entry="$(jq -c --arg cwd "$(pwd)" '
  .mcpServers.docket
  // .projects[$cwd].mcpServers.docket
  // ([.projects[]?.mcpServers?.docket? // empty] | first)
  // empty' "$cfg" 2>/dev/null)"
[ -n "$entry" ] || exit 0
url="$(printf '%s' "$entry" | jq -r '.url // empty')"
auth="$(printf '%s' "$entry" | jq -r '.headers.Authorization // empty')"
[ -n "$url" ] && [ -n "$auth" ] || exit 0

# Same scope rule as the /todo skill: the repo directory's name, lowercased;
# inside a Claude Code worktree (<repo>/.claude/worktrees/<name>), the repo's.
root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
case "$root" in
  */.claude/worktrees/*) root="${root%/.claude/worktrees/*}" ;;
esac
scope="$(basename "$root" | tr 'A-Z' 'a-z')"

body="$(jq -cn --arg scope "$scope" \
  '{jsonrpc:"2.0",id:1,method:"tools/call",params:{name:"todo_list",arguments:{scope:$scope}}}')"
resp="$(curl -fsS --max-time 4 "$url" \
  -H "Authorization: $auth" -H "Content-Type: application/json" -d "$body" 2>/dev/null)" || exit 0

printf '%s' "$resp" | jq -r --arg scope "$scope" '
  (.result.structuredContent.todos // []) as $t
  | if ($t | length) == 0 then empty else
      "Docket: \($t | length) open item\(if ($t | length) == 1 then "" else "s" end) in scope \"\($scope)\" — pick up freely; close with todo_close when you complete one:",
      ($t[] | "#\(.id)  \(.title)")
    end' 2>/dev/null
exit 0
