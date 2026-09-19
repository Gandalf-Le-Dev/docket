---
name: todo
description: File an item on Docket from this session; scope defaults to the current project. Bare /todo lists the project's open items.
argument-hint: "[scope:] text"
disable-model-invocation: true
allowed-tools: mcp__docket__todo_add, mcp__docket__todo_list, mcp__docket__todo_scopes
---

Root: !`git rev-parse --show-toplevel 2>/dev/null || pwd`
Branch: !`git branch --show-current 2>/dev/null || true`

**Project** = the last segment of Root, lowercased (`/Users/x/Dev/Docket` → `docket`). When Root contains `/.claude/worktrees/`, it is a Claude Code worktree: take the segment just before that instead (`/Users/x/Dev/hopbox/.claude/worktrees/happy-merkle-999f09` → `hopbox`).

Input:
$ARGUMENTS

## Parse

- **Scope** — when the input opens with a single word (letters, digits, `.`, `_`, `-`) followed directly by `:` and then a space or the end of input, that word lowercased is the scope and the remainder is the text. Otherwise the scope is the project above and the whole input is the text.
- **Text** — the words exactly as typed. The first line is the title; later lines are the body. A single line over 500 bytes splits at its first sentence: sentence → title, rest → body.

## Act

**Text empty → list.** `todo_list {scope}`; reply with one `#id  title` line per open item, or `nothing open in <scope>`.

**Text present → file.** When the scope was typed, first call `todo_scopes` so the reply can flag a scope that does not exist yet. Then `todo_add {title, body?, scope, source}` with `source` = `/todo in <root> (<branch>)` — `/todo in <root>` when there is no branch.

The reply is exactly one line: `Filed #<id> in <scope>: <title> — <url>` with the result's `url`, plus ` (already open)` when the result has `duplicate: true`, plus ` (new scope)` when the scope was typed and absent from `todo_scopes`.
