# Docket

**A self-hosted backlog any AI agent can write to.**

If you run coding agents (Claude Code, or anything else that speaks
[MCP](https://modelcontextprotocol.io)), you have seen this: mid-task, the agent
notices work that is real but belongs to a *different* project — and there is nowhere
to put it. Filing it in the current repo's tracker pollutes that tracker; saying it in
chat evaporates; so it gets dropped.

Docket is the destination. One small server you host yourself:

- **Agents file items** with a `todo_add` tool, from any machine, any repo, any
  platform — each carrying a `source` that records who noticed it.
- **You and your agents work them** from a phone-friendly built-in web page or via
  the full tool set: list, pick one up, edit, close as *done*, or close as *dropped*
  ("deliberately not doing this" is a verdict worth keeping).
- **You choose who reads.** Every machine gets its own named token — full-access
  *review* tokens for you and for agents that pick up work, and optional add-only
  *publish* tokens for machines that should only ever file.

It is one Go binary and one SQLite file. Small on purpose: no sprints, no assignees,
no boards, ever — the value is that it exists everywhere, not that it is a good
project-management tool.

## Try it in two minutes

You need Go 1.25+ (or use [Docker](#running-in-docker) below).

```sh
git clone https://github.com/Gandalf-Le-Dev/docket && cd docket
go build ./cmd/docket

# 1. Mint your tokens. Each prints its secret exactly once — save them.
./docket token new -db ./docket.db -role review  me       # full access — for you
./docket token new -db ./docket.db -role publish laptop   # add-only — for an agent

# 2. Run the server.
./docket serve -db ./docket.db -addr 127.0.0.1:8340
```

Now open <http://127.0.0.1:8340>, paste the **review** token, and you are looking at
an empty backlog. File something into it from the command line, as an agent would:

```sh
curl -s http://127.0.0.1:8340/mcp \
  -H "Authorization: Bearer dkt_YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"todo_add",
       "arguments":{"title":"my first item","source":"README quick start"}}}'
```

Refresh the page — the item is there. That is the whole loop: agents POST in,
you review on the page.

## Tokens and roles

Every request to Docket authenticates with a token, and every token has one of
exactly two roles:

| Capability | `publish` | `review` |
|---|:---:|:---:|
| File items (`todo_add`) | ✅ | ✅ |
| Read the backlog (`todo_list`, `todo_get`) | ❌ | ✅ |
| Edit and close items (`todo_update`, `todo_close`) | ❌ | ✅ |
| See which scopes exist (`todo_scopes`) | ❌ | ✅ |
| Sign in to the web page | ❌ | ✅ |
| Filing limits (title ≤ 500 B, body ≤ 64 KB, 120 adds/hour) | applies | applies |

The role is a property of the **token, not of who holds it** — agents can hold
review tokens. Pick per machine, based on what you want that machine doing:

- **`review`** is the full-access role: for the web page, and for any agent you
  want *working* the backlog — listing items, picking one up, closing it as done
  when finished. If your agents should both file and pick up todos, give them
  review tokens.
- **`publish`** is deliberate least privilege for machines that should only ever
  *file*: a box exposed to untrusted input, a CI job, someone else's machine.
  Its `tools/list` shows only `todo_add`, and calling anything else returns the
  same "unknown tool" error as a tool that doesn't exist — so if that machine is
  ever steered by a prompt injection, the attacker can add noise to your backlog
  but cannot read your cross-project plans out of it. If that trade-off doesn't
  matter to you, simply never mint one.

Either way, mint **one token per machine**, named after it
(`docket token new -role review laptop`): the name is stamped as `via` on every
item that machine files, so you always know who filed what, and revoking one
machine never touches the others.

There is no third role, and no admin role: creating, revoking, and listing tokens
happens only through the `docket token` CLI on the machine that holds the database
(see [Day-to-day operation](#day-to-day-operation)) — never over the network.

## Connecting a real agent

Mint each agent machine its own token — **review** if it should also read and pick
up work, **publish** if it should only file (see the roles above).

**Claude Code** — user scope, so the backlog is reachable from every project:

```sh
claude mcp add --scope user --transport http docket https://docket.example.net/mcp \
  --header "Authorization: Bearer dkt_YOUR_TOKEN"
```

**Any other MCP client** — standard HTTP server config:

```json
{
  "mcpServers": {
    "docket": {
      "type": "http",
      "url": "https://docket.example.net/mcp",
      "headers": { "Authorization": "Bearer dkt_YOUR_TOKEN" }
    }
  }
}
```

The server speaks both current MCP revisions from one endpoint — the stateless
2026-07-28 protocol and the 2025-era `initialize` handshake — so old and new clients
both just work.

Optionally add a line to the agent's standing instructions (`CLAUDE.md` or
equivalent): *"Work that is real but out of scope for the current repo goes to
Docket's `todo_add`, with `source` set to where you noticed it."* The tool's own
description says the same, so even an uninstructed agent files things sensibly.

**Claude Code extras.** Three one-file pieces under
[`contrib/claude-code`](contrib/claude-code), installed straight from GitHub — no
checkout needed:

```sh
base=https://raw.githubusercontent.com/Gandalf-Le-Dev/docket/main/contrib/claude-code
mkdir -p ~/.claude/skills/todo ~/.claude/skills/done ~/.claude/hooks
curl -fsSL -o ~/.claude/skills/todo/SKILL.md       $base/skills/todo/SKILL.md
curl -fsSL -o ~/.claude/skills/done/SKILL.md       $base/skills/done/SKILL.md
curl -fsSL -o ~/.claude/hooks/docket-open-items.sh $base/hooks/docket-open-items.sh
```

- **`/todo`** files by hand. `/todo fix the flaky store test` files an item scoped to
  the repo you are in; `/todo hopbox: egress allowlist still skipped` files into
  another scope; a bare `/todo` lists the current repo's open items. The reply is
  one line — `Filed #16 in docket: …` — with `(already open)` when Docket
  deduplicated it and `(new scope)` when you typed a scope that did not exist yet.
- **`/done`** closes by hand: `/done 16 shipped in 609cad1` as done,
  `/done drop 16 not worth it` as dropped.
- **The hook** is what makes the agent close things on its own. At session start
  it lists the repo's open items into context, so the agent knows what it can pick
  up and calls `todo_close` when it completes one. It reads the URL and token from
  the `docket` entry `claude mcp add` wrote, and stays silent when nothing is open.
  Enable it in `~/.claude/settings.json`:

  ```json
  {
    "hooks": {
      "SessionStart": [
        {
          "matcher": "^(startup|resume|clear|compact)$",
          "hooks": [{ "type": "command", "command": "sh ~/.claude/hooks/docket-open-items.sh", "timeout": 10 }]
        }
      ]
    }
  }
  ```

Listing and closing need a review token; filing works with either.

To manage the backlog *from* an agent (triage from your desktop, say), add the same
config with a **review** token instead — that unlocks `todo_list`, `todo_get`,
`todo_update`, `todo_close`, and `todo_scopes`.

## The tools

| Tool | Token role | Does |
|------|------|------|
| `todo_add {title, body?, scope?, source?}` | publish + review | File an item. Filing the same title+scope twice while open returns the existing id with `duplicate: true` instead of a second copy. |
| `todo_list {scope?, state?, q?}` | review | Read the backlog. `state`: `open` (default), `done`, `dropped`, `all`; `q` searches title and body. |
| `todo_get {id}` | review | One item. |
| `todo_update {id, title?, body?, scope?, state?}` | review | Edit anything; `state: "open"` reopens a closed item. |
| `todo_close {id, outcome?, reason?}` | review | Close with a verdict: `done` (default) or `dropped`. `reason` is appended to the body. |
| `todo_scopes {}` | review | The scopes in use, with open counts. |

`scope` is a freeform grouping label ("hopbox", "personal", "idea") — lowercased,
never a fixed list. When names drift apart, the web page has a rename that merges
them.

## Running in Docker

Images are published on tagged releases:

```sh
docker run -d --name docket \
  -p 127.0.0.1:8340:8340 \
  -v docket-data:/var/lib/docket \
  ghcr.io/gandalf-le-dev/docket:v0.1.0

docker exec docket docket token new -role review me
docker exec docket docket token new -role publish laptop
```

Or the same thing as `compose.yaml`:

```yaml
services:
  docket:
    image: ghcr.io/gandalf-le-dev/docket:v0.1.0
    restart: unless-stopped
    ports:
      - "127.0.0.1:8340:8340"   # loopback only — put TLS in front, see below
    volumes:
      - docket-data:/var/lib/docket
volumes:
  docket-data:
```

## Deploying for real

Two rules, then any host works:

1. **Terminate TLS in front of it.** Tokens travel in the `Authorization` header and
   a cookie; Docket itself serves plain HTTP on `:8340` and expects a reverse proxy
   (Caddy, nginx, Traefik) to provide HTTPS. Bind the container port to loopback, as
   above, so nothing reaches it *except* through the proxy.
2. **Put the database somewhere that survives redeploys.** Everything lives in one
   SQLite file (default `/var/lib/docket/docket.db`; override with `-db` or
   `DOCKET_DB`). In Docker that means a named volume — never a bind mount inside a
   directory your deploy tool replaces. Backup is copying the file.

`GET /healthz` returns 200 only when the database answers — point your health checks
at it.

This repo's own deployment uses [Pilot](https://github.com/Gandalf-Le-Dev/pilot).

## Day-to-day operation

```sh
docket token list             # every token, role, and status
docket token revoke laptop    # that machine can no longer file
docket token new -role publish laptop   # re-minting a revoked name rotates it
```

Abuse limits on filing: title ≤ 500 bytes, body ≤ 64 KB, and 120 adds per hour per
token — a compromised machine can make noise, but it cannot fill your disk, and `via`
tells you exactly which token to revoke.

## Development

```sh
go test ./...        # store, MCP protocol (both revisions), web UI
go build ./cmd/docket
```

No CGO (the SQLite driver is pure Go), so cross-compiling is
`GOOS=linux GOARCH=amd64 go build ./cmd/docket`.
