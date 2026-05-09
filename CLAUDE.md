# tether — v2 implementation in progress

## v0/ is archived — do not read proactively

`v0/` 是 v1 (post-pivot 之前) 的全部代码归档：`v0/{cmd,internal,docs,go.mod,go.sum,README.md}`。

- **不要主动读** `v0/`。所有 v2 实施期决定 (s1–s9) 优先以 v2 spec + addenda + plan 为准，**不**回看 v0 实现。
- 仅当 v2 实施明确需要参考某个 v1 行为时（例如 `v0/internal/cc/` 里的 cc 集成踩坑），按需读单个文件，读完即止。
- v0/ 不再修改、不再扩展、不再修 bug。如果 v1 有遗留 ticket，也当 v0 的事，新功能在 v2 重写。

## Authoritative spec / plan

- **Spec**: `../tether-doc/wiki/specs/2026-05-09-tether-simplified-design.md` §10.A–§10.K
- **Addenda**: D-05a / D-05b / D-17a / D-22 (同目录)
- **Plan**: `.workspace/memory/local/plan-t-01KR6E5V5PG3CXS598FABDY5WZ.md`（9 phases，s1 起步）
- **Active task**: `t-01KR6E5V5PG3CXS598FABDY5WZ` slug `v2-impl` (in_progress)

## What's at root vs v0/

| Path | Status | Why |
|---|---|---|
| `poc/go-quic-wt/` | **v2 reference, KEEP** | step7-13 单端口 mux + dual channel + permission hook PoC，s1/s2 直接 port |
| `.claude/` | keep | Claude Code config |
| `.env.dev`, `.env.dev.example` | keep | polyforge workspace env (author 等) |
| `.gitignore` | keep | v2 沿用，需要时增量改 |
| `v0/cmd/`, `v0/internal/`, `v0/docs/`, `v0/go.mod`, `v0/go.sum`, `v0/README.md` | **archived** | v1 production code |

## v2 layout (coming in s1+)

按 spec §10.C.1：根目录新建 `cmd/tether/`、`internal/{server,wire,agent,session,workspace,skill,crypto,pair}/`、`web/`、`scripts/`、`Makefile`、`tygo.yaml`、新 `go.mod` (module path 沿用 `github.com/piaobeizu/tether`)。

## Commits

走 `/pf2-commit msg="..."`，不要裸 `git commit`（polyforge 管理多仓 commit）。
