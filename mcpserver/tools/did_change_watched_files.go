package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"rockerboo/mcp-lsp-bridge/interfaces"
	"rockerboo/mcp-lsp-bridge/logger"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/myleshyson/lsprotocol-go/protocol"
)

func DidChangeWatchedFilesTool(bridge interfaces.BridgeInterface) (mcp.Tool, server.ToolHandlerFunc) {
	return mcp.NewTool("did_change_watched_files",
			mcp.WithDescription(`Notify the language server about file changes and force document re-indexing.

For Created (type=1) and Changed (type=2) events, the bridge automatically:
1. Sends workspace/didChangeWatchedFiles notification
2. Closes stale documents (didClose)
3. Re-opens them with fresh content from disk (didOpen)

This ensures diagnostics, symbols, and hover reflect the latest file content.

USAGE: language="bsl", changes_json='[{"uri":"file:///projects/path/file.bsl","type":2}]'
Types: 1=Created, 2=Changed, 3=Deleted`),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithString("language", mcp.Description("Language server ID (e.g., 'bsl')."), mcp.Required()),
			mcp.WithString("changes_json", mcp.Description("JSON array of file events: [{\"uri\":\"file:///path\",\"type\":1}]. type: 1=Created, 2=Changed, 3=Deleted."), mcp.Required()),
		), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			language, err := request.RequireString("language")
			if err != nil {
				logger.Error("did_change_watched_files: language parsing failed", err)
				return mcp.NewToolResultError(err.Error()), nil
			}

			if result, ok := CheckReadyOrReturn(bridge); !ok {
				return result, nil
			}

			changesJSON, err := request.RequireString("changes_json")
			if err != nil {
				logger.Error("did_change_watched_files: changes_json parsing failed", err)
				return mcp.NewToolResultError(err.Error()), nil
			}

			var changes []protocol.FileEvent
			if err := json.Unmarshal([]byte(changesJSON), &changes); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("Invalid changes_json: %v", err)), nil
			}

			if err := bridge.DidChangeWatchedFiles(language, changes); err != nil {
				logger.Error("did_change_watched_files: notification failed", err)
				return mcp.NewToolResultError(fmt.Sprintf("didChangeWatchedFiles failed: %v", err)), nil
			}

			return mcp.NewToolResultText("ok"), nil
		}
}

func RegisterDidChangeWatchedFilesTool(mcpServer ToolServer, bridge interfaces.BridgeInterface) {
	mcpServer.AddTool(DidChangeWatchedFilesTool(bridge))
}
