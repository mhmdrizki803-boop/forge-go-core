# forge-go-core

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/mhmdrizki803-boop/forge-go-core/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/mhmdrizki803-boop/forge-go-core/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge in one process: git smart HTTP (v2), the REST v3 routes, GraphQL and MCP over a single in-process store, no database and no operator.

## Surfaces

- **git smart HTTP v2** — `info/refs`, `git-upload-pack`, `git-receive-pack` over `pkt-line`, pack parsing and delta resolution.
- **REST v3** — repositories, issues, pulls, commits, contents, branches, tags, releases, users, search and pagination on one router.
- **GraphQL** — schema with introspection, connection-shaped pagination and nested resolvers, not a REST shim.
- **MCP** — streamable-HTTP JSON-RPC with `initialize`, `tools/list`, `tools/call`, so an agent drives the forge as a tool.

Everything lives in one process and one append-only store, so a clone, an issue list and an agent tool call hit the same bytes without a database, a queue or a second binary.

## Run it

```sh
node main.ts
```

`main.ts` spawns the Go `forge` binary beside it; the process binds an ephemeral port and prints it.

---

<sub>Entered on [make.fun](https://make.fun/?ref=hyy9rcmd&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
