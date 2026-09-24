// Package core holds the domain model and the ports that adapters implement.
//
// The MCP protocol's own types (*mcp.Tool, *mcp.CallToolResult,
// json.RawMessage arguments) are the domain vocabulary: this is an MCP
// gateway, so the protocol model IS the model. Adapters translate the world
// into these types; the core never imports transport or storage code.
package core

import (
	"context"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Version is the single source for what the binary reports to MCP clients.
// Release builds stamp it: -ldflags "-X .../internal/core.Version=vX.Y.Z".
var Version = "0.1.0"

// Upstream is one connected backend MCP server — the driven port every
// backend adapter implements.
type Upstream interface {
	// Namespace is the backend's short lowercase name ("fs", "github").
	Namespace() string
	// Tools is the discovered inventory, original upstream names.
	Tools() []*mcp.Tool
	// Call invokes one discovered tool by its ORIGINAL name.
	Call(ctx context.Context, tool string, args json.RawMessage) (*mcp.CallToolResult, error)
	// Close releases the session (subprocess or HTTP connection).
	Close() error
}

// ToolInfo is one catalog entry — a discovered tool plus its governance
// state, as reported by the gateway's own toolhost__* meta-tools.
type ToolInfo struct {
	Qualified   string `json:"qualified"`
	Description string `json:"description,omitempty"`
	Approved    bool   `json:"approved"`
	Enabled     bool   `json:"enabled"`
	// Requested marks an agent-filed approval request still waiting on a
	// human — visible so the agent knows it already asked.
	Requested bool `json:"requested,omitempty"`
}

// Event is one governed action's evidence — the driven audit port.
type Event struct {
	TS      time.Time `json:"ts"`
	Kind    string    `json:"kind"` // tool_call | auth_failed | backend_error | reload
	Backend string    `json:"backend,omitempty"`
	Tool    string    `json:"tool,omitempty"`
	MS      int64     `json:"ms,omitempty"`
	Err     string    `json:"err,omitempty"`
	Remote  string    `json:"remote,omitempty"`
}

// AuditSink receives every Event (driven port). Implementations must not
// block the call path meaningfully — append-and-go.
type AuditSink interface {
	Record(Event)
}

// Event kinds.
const (
	EventToolCall      = "tool_call"
	EventAuthFailed    = "auth_failed"
	EventBackendError  = "backend_error"
	EventToolForbidden = "tool_forbidden"
	EventReload        = "reload"
	EventGovern        = "govern"
)
