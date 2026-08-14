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

It is small on purpose — the value is that it exists everywhere, not that it is a good
project-management tool. No sprints, no assignees, no boards, ever.

See [DESIGN.md](DESIGN.md) for the full design: the name, the verbs, the schema, auth,
and how it deploys. Designed against
[issue #1](https://github.com/Gandalf-Le-Dev/docket/issues/1).
