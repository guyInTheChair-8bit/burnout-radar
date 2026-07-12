// Package mcp implements a Model Context Protocol (MCP) server for BurnoutRadar.
//
// The MCP server exposes channel burnout metrics stored in SQLite to AI assistants
// (such as Gemini) via the JSON-RPC 2.0 based Model Context Protocol.
//
// ZKA NOTE: This package only reads scalar metrics from the DB — no PII is
// present in any data it handles or returns.
//
// Reference: https://modelcontextprotocol.io/specification
package mcp

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"burnoutradar-mcp/db"
)

// ---- JSON-RPC 2.0 wire types ------------------------------------------------

// jsonRPCRequest represents an incoming MCP/JSON-RPC 2.0 request.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"` // string | number | null per spec
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCResponse is the standard JSON-RPC 2.0 success response envelope.
type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

// rpcError encodes a JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Standard JSON-RPC error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
)

// ---- MCP protocol types -----------------------------------------------------

// initializeResult is returned in response to an MCP "initialize" request.
type initializeResult struct {
	ProtocolVersion string     `json:"protocolVersion"`
	ServerInfo      serverInfo `json:"serverInfo"`
	Capabilities    caps       `json:"capabilities"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type caps struct {
	Tools struct{} `json:"tools"`
}

// toolDefinition describes a single MCP tool in the tools/list response.
type toolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

type inputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]property `json:"properties"`
	Required   []string            `json:"required"`
}

type property struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

// toolsListResult wraps the list of available tools.
type toolsListResult struct {
	Tools []toolDefinition `json:"tools"`
}

// toolCallParams are the parameters for a tools/call request.
type toolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// toolCallResult wraps the tool output in MCP's content format.
type toolCallResult struct {
	Content []contentItem `json:"content"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ---- MCPServer --------------------------------------------------------------

// MCPServer handles MCP JSON-RPC requests, backed by the SQLite metrics DB.
// ZKA: It reads only scalar channel metrics — no PII is present.
type MCPServer struct {
	database *db.DB
}

// NewMCPServer creates an MCPServer with the provided DB handle.
func NewMCPServer(database *db.DB) *MCPServer {
	return &MCPServer{database: database}
}

// ServeHTTP implements http.Handler. All MCP traffic is POST to a single
// endpoint; method routing is done by the JSON-RPC "method" field.
func (s *MCPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "MCP requires POST", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB limit
	defer r.Body.Close()

	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, nil, rpcParseError, "parse error: "+err.Error())
		return
	}

	if req.JSONRPC != "2.0" {
		s.writeError(w, req.ID, rpcInvalidRequest, "jsonrpc must be \"2.0\"")
		return
	}

	// Route by MCP method name.
	switch req.Method {
	case "initialize":
		s.handleInitialize(w, req)
	case "tools/list":
		s.handleToolsList(w, req)
	case "tools/call":
		s.handleToolsCall(w, req)
	default:
		s.writeError(w, req.ID, rpcMethodNotFound, fmt.Sprintf("unknown method: %q", req.Method))
	}
}

// handleInitialize responds to the MCP handshake.
func (s *MCPServer) handleInitialize(w http.ResponseWriter, req jsonRPCRequest) {
	result := initializeResult{
		ProtocolVersion: "2024-11-05",
		ServerInfo: serverInfo{
			Name:    "burnoutradar-mcp",
			Version: "1.0.0",
		},
		Capabilities: caps{},
	}
	s.writeResult(w, req.ID, result)
}

// handleToolsList returns the manifest of all tools this MCP server exposes.
func (s *MCPServer) handleToolsList(w http.ResponseWriter, req jsonRPCRequest) {
	result := toolsListResult{
		Tools: []toolDefinition{
			{
				Name:        "get_channel_burnout_metrics",
				Description: "Retrieve computed burnout risk metrics (Gini coefficient, Pareto share, z-score, DM share, average word count) for a Slack channel on a given date. All values are mathematical scalars — no user-level data is returned.",
				InputSchema: inputSchema{
					Type: "object",
					Properties: map[string]property{
						"channel_id": {
							Type:        "string",
							Description: "The Slack channel ID (e.g. C012AB3CD).",
						},
						"date": {
							Type:        "string",
							Description: "The date in YYYY-MM-DD format (UTC). Defaults to today if omitted.",
						},
					},
					Required: []string{"channel_id"},
				},
			},
		},
	}
	s.writeResult(w, req.ID, result)
}

// handleToolsCall dispatches a tools/call request to the appropriate handler.
func (s *MCPServer) handleToolsCall(w http.ResponseWriter, req jsonRPCRequest) {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(w, req.ID, rpcInvalidParams, "invalid params: "+err.Error())
		return
	}

	switch params.Name {
	case "get_channel_burnout_metrics":
		s.toolGetChannelBurnoutMetrics(w, req.ID, params.Arguments)
	default:
		s.writeError(w, req.ID, rpcMethodNotFound, fmt.Sprintf("unknown tool: %q", params.Name))
	}
}

// toolGetChannelBurnoutMetrics implements the get_channel_burnout_metrics tool.
//
// ZKA: Queries only mathematical scalars from the DB. The channel_id is a
// non-identifying team channel reference, not a personal identifier.
func (s *MCPServer) toolGetChannelBurnoutMetrics(
	w http.ResponseWriter,
	id interface{},
	args map[string]interface{},
) {
	// Extract required channel_id argument.
	channelID, ok := args["channel_id"].(string)
	if !ok || channelID == "" {
		s.writeError(w, id, rpcInvalidParams, "channel_id is required and must be a string")
		return
	}

	// Extract optional date argument; default to today (UTC).
	date := time.Now().UTC().Format("2006-01-02")
	if d, ok := args["date"].(string); ok && d != "" {
		date = d
	}

	// Query the DB for scalar metrics. No PII involved.
	metrics, err := s.database.GetMetrics(date, channelID)
	if err != nil {
		log.Printf("mcp: GetMetrics(%s, %s): %v", date, channelID, err)
		s.writeError(w, id, rpcInternalError, "database error: "+err.Error())
		return
	}

	// Handle not-found case gracefully.
	if metrics == nil {
		text := fmt.Sprintf("No metrics found for channel %q on %s.", channelID, date)
		s.writeResult(w, id, toolCallResult{
			Content: []contentItem{{Type: "text", Text: text}},
		})
		return
	}

	// Serialise the metrics struct to a human-readable JSON blob for the LLM.
	metricsJSON, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		s.writeError(w, id, rpcInternalError, "serialisation error: "+err.Error())
		return
	}

	s.writeResult(w, id, toolCallResult{
		Content: []contentItem{
			{
				Type: "text",
				Text: fmt.Sprintf("Channel burnout metrics for %s on %s:\n\n```json\n%s\n```", channelID, date, string(metricsJSON)),
			},
		},
	})
}

// ---- helpers ----------------------------------------------------------------

// writeResult sends a successful JSON-RPC 2.0 response.
func (s *MCPServer) writeResult(w http.ResponseWriter, id interface{}, result interface{}) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("mcp: writeResult encode error: %v", err)
	}
}

// writeError sends a JSON-RPC 2.0 error response.
func (s *MCPServer) writeError(w http.ResponseWriter, id interface{}, code int, msg string) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("mcp: writeError encode error: %v", err)
	}
}
