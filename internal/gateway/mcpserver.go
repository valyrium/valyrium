package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type JSONRPCRequest struct {
	Jsonrpc string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      interface{}     `json:"id"`
}

type JSONRPCResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
	ID      interface{}    `json:"id,omitempty"`
}

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) handleMCPEndpoint(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != "POST" {
		s.sendJSON(w, 404, map[string]interface{}{
			"error": "method not allowed",
		})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.sendJSON(w, 400, map[string]interface{}{
			"error": "failed to read body",
		})
		return
	}

	var req JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.sendJSON(w, 400, map[string]interface{}{
			"error": "invalid JSON-RPC request",
		})
		return
	}

	if req.Jsonrpc != "2.0" {
		s.sendJSON(w, 400, map[string]interface{}{
			"error": "unsupported jsonrpc version",
		})
		return
	}

	// Handle notifications (no ID field) — respond with 202 No Content
	if req.ID == nil {
		w.WriteHeader(202)
		return
	}

	session := s.sessions.GetSession(sessionID)
	if session == nil {
		resp := JSONRPCResponse{
			Jsonrpc: "2.0",
			Error: &JSONRPCError{
				Code:    -32600,
				Message: "session not found",
			},
			ID: req.ID,
		}
		s.sendJSON(w, 200, resp)
		return
	}

	switch req.Method {
	case "initialize":
		s.handleMCPInitialize(w, req, sessionID)
	case "tools/list":
		s.handleMCPToolsList(w, req, session)
	case "tools/call":
		s.handleMCPToolsCall(w, req, session, sessionID)
	default:
		resp := JSONRPCResponse{
			Jsonrpc: "2.0",
			Error: &JSONRPCError{
				Code:    -32601,
				Message: fmt.Sprintf("method not found: %s", req.Method),
			},
			ID: req.ID,
		}
		s.sendJSON(w, 200, resp)
	}
}

func (s *Server) handleMCPInitialize(w http.ResponseWriter, req JSONRPCRequest, sessionID string) {
	resp := JSONRPCResponse{
		Jsonrpc: "2.0",
		Result: map[string]interface{}{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]interface{}{},
			"serverInfo": map[string]interface{}{
				"name":    "llm-gateway-relay",
				"version": "1.0.0",
			},
		},
		ID: req.ID,
	}
	s.sendJSON(w, 200, resp)
}

func (s *Server) handleMCPToolsList(w http.ResponseWriter, req JSONRPCRequest, session *Session) {
	session.Mu.Lock()
	tools := make([]map[string]interface{}, len(session.Tools))
	for i, tool := range session.Tools {
		tools[i] = map[string]interface{}{
			"name":        tool.Function.Name,
			"description": tool.Function.Description,
			"inputSchema": tool.Function.Parameters,
		}
	}
	session.Mu.Unlock()

	resp := JSONRPCResponse{
		Jsonrpc: "2.0",
		Result: map[string]interface{}{
			"tools": tools,
		},
		ID: req.ID,
	}
	s.sendJSON(w, 200, resp)
}

type ToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handleMCPToolsCall(w http.ResponseWriter, req JSONRPCRequest, session *Session, sessionID string) {
	var params ToolsCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		resp := JSONRPCResponse{
			Jsonrpc: "2.0",
			Error: &JSONRPCError{
				Code:    -32602,
				Message: "invalid params",
			},
			ID: req.ID,
		}
		s.sendJSON(w, 200, resp)
		return
	}

	session.Mu.Lock()

	var toolCallID string
	for id, call := range session.PendingCalls {
		if call.Name == params.Name && string(call.Arguments) == string(params.Arguments) {
			toolCallID = id
			break
		}
	}

	if toolCallID == "" {
		session.Mu.Unlock()
		resp := JSONRPCResponse{
			Jsonrpc: "2.0",
			Error: &JSONRPCError{
				Code:    -32600,
				Message: "tool call not found or already resolved",
			},
			ID: req.ID,
		}
		s.sendJSON(w, 200, resp)
		return
	}

	pendingCall := session.PendingCalls[toolCallID]
	delete(session.PendingCalls, toolCallID)

	result, ok := session.BufferedResults[toolCallID]
	if ok {
		delete(session.BufferedResults, toolCallID)
		session.LastActivity = time.Now()
		session.Mu.Unlock()

		resp := JSONRPCResponse{
			Jsonrpc: "2.0",
			Result: map[string]interface{}{
				"content": []map[string]interface{}{
					{
						"type": "text",
						"text": result,
					},
				},
			},
			ID: req.ID,
		}
		s.sendJSON(w, 200, resp)
		return
	}

	session.LastActivity = time.Now()
	session.Mu.Unlock()

	result = <-pendingCall.Resolver

	resp := JSONRPCResponse{
		Jsonrpc: "2.0",
		Result: map[string]interface{}{
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": result,
				},
			},
		},
		ID: req.ID,
	}
	s.sendJSON(w, 200, resp)
}
