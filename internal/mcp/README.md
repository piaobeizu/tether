# internal/mcp — MCP host integration entry point

This package tree will contain the tether MCP host runtime in v0.3.
See `tether-doc/wiki/specs/2026-05-10-mcp-integration-tether-spec.md` for the design.

**AgentProvider** (`internal/agent/`) is orthogonal to the MCP host.
The provider abstraction selects which LLM drives the conversation;
the MCP host manages tool server lifecycles independently of which provider is active.

Placeholder added by the §3.2 forward-compat task (mcp-compat).
Implementation begins after this task is merged.
