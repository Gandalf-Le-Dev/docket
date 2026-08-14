# Docket

**A self-hosted backlog any agent can write to.**

Agents keep noticing work that is real but out of scope for the repo they are in, and
today that work has nowhere to go — so it gets dropped. Docket is the destination: one
small MCP server, self-hosted on the fleet, that any agent on any box can publish a
todo to, and that the owner can review and edit from any agent, any scope, or a phone.

- `todo_add` from anywhere, with `source` recording who noticed it
- review, edit, and close through MCP or a minimal built-in web page
- one Go binary, one SQLite file, deployed with [Pilot](https://github.com/Gandalf-Le-Dev/pilot)
- publish is cheap, read is privileged: per-box add-only tokens, one review token
- speaks MCP spec revision **2026-07-28** (stateless, per-request metadata) and the
  2025-era handshake revisions alike, from one endpoint

It is small on purpose — the value is that it exists everywhere, not that it is a good
project-management tool. No sprints, no assignees, no boards, ever.

See [DESIGN.md](DESIGN.md) for the full design: the name, the verbs, the schema, auth,
and how it deploys. Designed against
[issue #1](https://github.com/Gandalf-Le-Dev/docket/issues/1).

## Quick start

```sh
go build ./cmd/docket

# Mint tokens. The plaintext is printed exactly once; only a hash is stored.
./docket token new -db ./docket.db -role review  phone     # full access — yours
./docket token new -db ./docket.db -role publish box-nyc   # add-only — one per agent box

./docket serve -db ./docket.db -addr :8340
```

Three surfaces on one port:

| Path       | Surface                                   | Auth                       |
|------------|-------------------------------------------|----------------------------|
| `/mcp`     | MCP, streamable HTTP, POST-only           | `Authorization: Bearer …`  |
| `/`        | Web review page (phone-friendly)          | Review token, entered once |
| `/healthz` | Liveness + DB ping                        | None                       |

## Wiring up an agent

Each agent box gets the standard HTTP MCP client config with its own publish token:

```json
{
  "mcpServers": {
    "docket": {
      "type": "http",
      "url": "https://docket.example.net/mcp",
      "headers": { "Authorization": "Bearer ${DOCKET_TOKEN}" }
    }
  }
}
```

A publish token sees exactly one tool, `todo_add`, and cannot read, edit, or even list
the backlog. A review token gets the full set: `todo_list`, `todo_get`, `todo_update`,
`todo_close`, and `todo_scopes`.

## The verbs

| Tool | Role | Does |
|------|------|------|
| `todo_add {title, body?, scope?, source?}` | publish + review | File an item. Exact-match dedupe against open items returns the existing id with `duplicate: true`. |
| `todo_list {scope?, state?, q?}` | review | Read the backlog. `state` defaults to `open`. |
| `todo_get {id}` | review | One item. |
| `todo_update {id, title?, body?, scope?, state?}` | review | Edit anything; `state: open` reopens. |
| `todo_close {id, outcome?, reason?}` | review | Verdict: `done` (default) or `dropped`. |
| `todo_scopes {}` | review | Scopes in use with open counts. |

## Operating it

```sh
docket token list                 # who holds tokens
docket token revoke box-nyc       # unplug one box; re-minting the name rotates it
```

The database is one SQLite file (default `/var/lib/docket/docket.db`, override with
`-db` or `DOCKET_DB`). Backup is copying the file. Guard rails on publish: title ≤ 500
bytes, body ≤ 64 KB, 120 adds per hour per token.

The container image is published as `ghcr.io/gandalf-le-dev/docket` on tagged
releases; `DESIGN.md` §8 has the Pilot `service.yaml`/`compose.yaml` for deploying it
— including the named-volume detail that keeps the database out of Pilot's immutable
release directories.
