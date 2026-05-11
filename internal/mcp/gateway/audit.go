// internal/mcp/gateway/audit.go
package gateway

import "github.com/piaobeizu/tether/internal/mcp/host"

type auditor struct {
	logger host.HistoryLogger
}

func (a *auditor) toolCallStarted(callID, toolName, serverName string) {
	_ = a.logger.Append("mcp_tool_call_started", map[string]any{
		"call_id": callID, "tool": toolName, "server": serverName,
	})
}

func (a *auditor) toolCallCompleted(callID string, contentLen int) {
	_ = a.logger.Append("mcp_tool_call_completed", map[string]any{
		"call_id": callID, "content_size": contentLen,
	})
}

func (a *auditor) toolCallFailed(callID, reason string) {
	_ = a.logger.Append("mcp_tool_call_failed", map[string]any{
		"call_id": callID, "error": reason,
	})
}

func (a *auditor) toolCallDenied(toolName, source, reason string) {
	_ = a.logger.Append("mcp_tool_call_denied", map[string]any{
		"tool": toolName, "source": source, "reason": reason,
	})
}
