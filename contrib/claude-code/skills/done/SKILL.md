---
name: done
description: Close a Docket item from this session, as done or dropped.
argument-hint: "[drop] id [reason]"
disable-model-invocation: true
allowed-tools: mcp__docket__todo_close
---

Input:
$ARGUMENTS

## Parse

- **Outcome** — `done`, unless the input opens with the word `drop`, which is removed and makes it `dropped`.
- **Id** — the next token, with any leading `#` removed.
- **Reason** — everything after the id, exactly as typed; absent when nothing follows.

## Act

Input empty, or no id → reply `usage: /done [drop] <id> [reason]`.

Otherwise `todo_close {id, outcome, reason?}`. The result carries the item.

The reply is exactly one line: `Closed #<id> (done): <title>` or `Dropped #<id>: <title>`; on a tool error, the error's message.
