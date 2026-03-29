package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"rockerboo/mcp-lsp-bridge/interfaces"
	"rockerboo/mcp-lsp-bridge/logger"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/myleshyson/lsprotocol-go/protocol"
)

func DocumentDiagnosticsTool(bridge interfaces.BridgeInterface) (mcp.Tool, server.ToolHandlerFunc) {
	return mcp.NewTool("document_diagnostics",
			mcp.WithDescription(`Get diagnostics (errors, warnings) for a specific file via LSP textDocument/diagnostic.

URI FORMAT (any of these work):
- Relative path (recommended): uri="do-extension-ame/CommonModules/МойМодуль/Ext/Module.bsl"
- Container absolute: uri="/projects/do-extension-ame/CommonModules/МойМодуль/Ext/Module.bsl"
- File URI: uri="file:///projects/do-extension-ame/CommonModules/МойМодуль/Ext/Module.bsl"
- Host Windows path: uri="F:\path\to\file.bsl" (auto-mapped to container)

OUTPUT: Diagnostic report grouped by severity (errors, warnings, hints)`),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithString("uri", mcp.Description("URI to the file to diagnose"), mcp.Required()),
			mcp.WithString("identifier", mcp.Description("Optional identifier for the diagnostic request")),
			mcp.WithString("previous_result_id", mcp.Description("Optional result ID from previous diagnostic request for caching")),
		), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Parse and validate parameters
			uri, err := request.RequireString("uri")
			if err != nil {
				logger.Error("document_diagnostics: URI parsing failed", err)
				return mcp.NewToolResultError(err.Error()), nil
			}

			// Optional parameters
			identifier := request.GetString("identifier", "")
			previousResultId := request.GetString("previous_result_id", "")

			if result, ok := CheckReadyOrReturn(bridge); !ok {
				return result, nil
			}

			// Infer language for debugging
			language, langErr := bridge.InferLanguage(uri)
			if langErr != nil {
				logger.Error("document_diagnostics: Language inference failed", langErr)
				return mcp.NewToolResultError(fmt.Sprintf("failed to infer language for %s: %v", uri, langErr)), nil
			}

			logger.Info(fmt.Sprintf("document_diagnostics: Processing request for %s (language: %s)", uri, string(*language)))

			// Get document diagnostics from bridge
			bridgeWithDiagnostics, ok := bridge.(interface {
				GetDocumentDiagnostics(uri string, identifier string, previousResultId string) (*protocol.DocumentDiagnosticReport, error)
			})
			if !ok {
				return mcp.NewToolResultError("document diagnostics not supported by this bridge implementation"), nil
			}

			report, err := bridgeWithDiagnostics.GetDocumentDiagnostics(uri, identifier, previousResultId)
			if err != nil {
				errMsg := err.Error()
				// LSP error -32603 (Internal error) often means file not found in project workspace.
				if strings.Contains(errMsg, "-32603") || strings.Contains(errMsg, "InternalError") || strings.Contains(errMsg, "Internal error") {
					logger.Warn(fmt.Sprintf("document_diagnostics: LSP Internal error for %s — file likely not found in LSP workspace", uri))
					return mcp.NewToolResultError(fmt.Sprintf(
						"File not found in LSP project workspace: %s\n\n"+
							"The LSP server returned Internal error (-32603). This usually means:\n"+
							"1. The file does not exist at the path visible to the LSP server\n"+
							"2. The file is outside the project workspace root\n"+
							"3. The file was recently created and the LSP server hasn't indexed it yet\n\n"+
							"Try using did_change_watched_files to notify the server about new files.",
						uri)), nil
				}
				logger.Error("document_diagnostics: Request failed", err)
				return mcp.NewToolResultError(fmt.Sprintf("document diagnostics request failed: %v", err)), nil
			}

			// Format the response
			result := formatDocumentDiagnostics(report, uri)

			logger.Info("document_diagnostics: Successfully processed " + uri)
			return mcp.NewToolResultText(result), nil
		}
}

func formatDocumentDiagnostics(report *protocol.DocumentDiagnosticReport, uri string) string {
	if report == nil {
		return "No diagnostic report available"
	}

	// DocumentDiagnosticReport is Or2[RelatedFullDocumentDiagnosticReport, RelatedUnchangedDocumentDiagnosticReport]
	// Try to access the Value directly first
	if report.Value != nil {
		switch v := report.Value.(type) {
		case protocol.RelatedFullDocumentDiagnosticReport:
			return formatFullDiagnosticReport(&v, uri)
		case *protocol.RelatedFullDocumentDiagnosticReport:
			return formatFullDiagnosticReport(v, uri)
		case protocol.RelatedUnchangedDocumentDiagnosticReport:
			return formatUnchangedDiagnosticReport(&v, uri)
		case *protocol.RelatedUnchangedDocumentDiagnosticReport:
			return formatUnchangedDiagnosticReport(v, uri)
		}
	}

	// Fallback: Convert to JSON and back to extract the actual diagnostic data
	reportBytes, err := json.Marshal(report)
	if err != nil {
		return fmt.Sprintf("Error parsing diagnostic report: %v", err)
	}

	// First, determine the report kind by parsing as a generic map
	var rawReport map[string]interface{}
	if err := json.Unmarshal(reportBytes, &rawReport); err != nil {
		return fmt.Sprintf("Error parsing diagnostic report: %v", err)
	}

	kind, _ := rawReport["kind"].(string)

	// Handle "full" report (with items array, even if empty)
	if kind == "full" {
		var fullReport protocol.RelatedFullDocumentDiagnosticReport
		if err := json.Unmarshal(reportBytes, &fullReport); err == nil {
			return formatFullDiagnosticReport(&fullReport, uri)
		}
	}

	// Handle "unchanged" report
	if kind == "unchanged" {
		var unchangedReport protocol.RelatedUnchangedDocumentDiagnosticReport
		if err := json.Unmarshal(reportBytes, &unchangedReport); err == nil {
			return formatUnchangedDiagnosticReport(&unchangedReport, uri)
		}
	}

	// Fallback: try to parse as full report anyway (for servers that don't set kind)
	var fullReport protocol.RelatedFullDocumentDiagnosticReport
	if err := json.Unmarshal(reportBytes, &fullReport); err == nil {
		return formatFullDiagnosticReport(&fullReport, uri)
	}

	// If we can't parse it, show raw data for debugging
	var result strings.Builder
	result.WriteString("DOCUMENT DIAGNOSTICS:\n")
	result.WriteString(fmt.Sprintf("File: %s\n", uri))
	result.WriteString("Report Type: Unknown\n")
	result.WriteString(fmt.Sprintf("Raw data: %s\n", string(reportBytes)))

	return result.String()
}

func formatFullDiagnosticReport(report *protocol.RelatedFullDocumentDiagnosticReport, uri string) string {
	var result strings.Builder

	result.WriteString("DOCUMENT DIAGNOSTICS:\n")
	result.WriteString(fmt.Sprintf("File: %s\n", uri))
	result.WriteString("Report Type: Full\n")
	if report.ResultId != "" {
		result.WriteString(fmt.Sprintf("Result ID: %s\n", report.ResultId))
	}

	result.WriteString(fmt.Sprintf("\nTotal issues: %d\n", len(report.Items)))

	if len(report.Items) == 0 {
		result.WriteString("\n✅ No issues found - code is clean!\n")
	} else {
		result.WriteString("ISSUES FOUND:\n")
		result.WriteString(strings.Repeat("=", 50) + "\n\n")

		// Group diagnostics by severity
		errors := make([]protocol.Diagnostic, 0)
		warnings := make([]protocol.Diagnostic, 0)
		infos := make([]protocol.Diagnostic, 0)
		hints := make([]protocol.Diagnostic, 0)

		for _, diagnostic := range report.Items {
			if diagnostic.Severity == nil {
				errors = append(errors, diagnostic) // Default to error if no severity
			} else {
				switch *diagnostic.Severity {
				case 1: // Error
					errors = append(errors, diagnostic)
				case 2: // Warning
					warnings = append(warnings, diagnostic)
				case 3: // Information
					infos = append(infos, diagnostic)
				case 4: // Hint
					hints = append(hints, diagnostic)
				}
			}
		}

		// Format errors
		if len(errors) > 0 {
			result.WriteString(fmt.Sprintf("ERRORS (%d):\n", len(errors)))
			for i, diagnostic := range errors {
				result.WriteString(formatEnhancedDiagnostic(i+1, diagnostic, "ERROR"))
			}
			result.WriteString("\n")
		}

		// Format warnings
		if len(warnings) > 0 {
			result.WriteString(fmt.Sprintf("WARNINGS (%d):\n", len(warnings)))
			for i, diagnostic := range warnings {
				result.WriteString(formatEnhancedDiagnostic(i+1, diagnostic, "WARNING"))
			}
			result.WriteString("\n")
		}

		// Format info messages
		if len(infos) > 0 {
			result.WriteString(fmt.Sprintf("INFORMATION (%d):\n", len(infos)))
			for i, diagnostic := range infos {
				result.WriteString(formatEnhancedDiagnostic(i+1, diagnostic, "INFO"))
			}
			result.WriteString("\n")
		}

		// Format hints
		if len(hints) > 0 {
			result.WriteString(fmt.Sprintf("HINTS (%d):\n", len(hints)))
			for i, diagnostic := range hints {
				result.WriteString(formatEnhancedDiagnostic(i+1, diagnostic, "HINT"))
			}
			result.WriteString("\n")
		}
	}

	// Handle related documents
	if len(report.RelatedDocuments) > 0 {
		result.WriteString("RELATED DOCUMENTS:\n")
		result.WriteString(strings.Repeat("-", 30) + "\n")
		for relatedUri := range report.RelatedDocuments {
			result.WriteString(fmt.Sprintf("- %s\n", relatedUri))
		}
		result.WriteString("\n")
	}

	return result.String()
}

func formatUnchangedDiagnosticReport(report *protocol.RelatedUnchangedDocumentDiagnosticReport, uri string) string {
	var result strings.Builder

	result.WriteString("Report Type: Unchanged Document Diagnostic Report\n")
	result.WriteString(fmt.Sprintf("Result ID: %s\n", report.ResultId))
	result.WriteString("No changes since last diagnostic request - results are unchanged.\n\n")

	// Handle related documents
	if len(report.RelatedDocuments) > 0 {
		result.WriteString("RELATED DOCUMENTS:\n")
		result.WriteString(strings.Repeat("-", 30) + "\n")
		for relatedUri := range report.RelatedDocuments {
			result.WriteString(fmt.Sprintf("- %s\n", relatedUri))
		}
	}

	return result.String()
}

func formatEnhancedDiagnostic(index int, diagnostic protocol.Diagnostic, severityStr string) string {
	var result strings.Builder

	result.WriteString(fmt.Sprintf("%d. %s\n", index, diagnostic.Message))
	result.WriteString(fmt.Sprintf("   Location: Line %d, Column %d-%d\n",
		diagnostic.Range.Start.Line+1,
		diagnostic.Range.Start.Character+1,
		diagnostic.Range.End.Character+1))

	// Show diagnostic code if available
	if diagnostic.Code != nil {
		result.WriteString(fmt.Sprintf("   Code: %v\n", *diagnostic.Code))
	}

	// Show source (e.g., 'typescript', 'biome', etc.)
	if diagnostic.Source != "" {
		result.WriteString(fmt.Sprintf("   Source: %s\n", diagnostic.Source))
	}

	// Show diagnostic tags
	if len(diagnostic.Tags) > 0 {
		var tags []string
		for _, tag := range diagnostic.Tags {
			switch tag {
			case 1:
				tags = append(tags, "Unnecessary")
			case 2:
				tags = append(tags, "Deprecated")
			default:
				tags = append(tags, fmt.Sprintf("Tag-%d", tag))
			}
		}
		result.WriteString(fmt.Sprintf("   Tags: %s\n", strings.Join(tags, ", ")))
	}

	// Show code description link if available
	if diagnostic.CodeDescription != nil && diagnostic.CodeDescription.Href != "" {
		result.WriteString(fmt.Sprintf("   Reference: %s\n", diagnostic.CodeDescription.Href))
	}

	// Show related information if available
	if len(diagnostic.RelatedInformation) > 0 {
		result.WriteString("   Related Information:\n")
		for _, info := range diagnostic.RelatedInformation {
			result.WriteString(fmt.Sprintf("      - %s (Line %d)\n",
				info.Message,
				info.Location.Range.Start.Line+1))
		}
	}

	result.WriteString("\n")
	return result.String()
}

func RegisterDocumentDiagnosticsTool(s ToolServer, bridge interfaces.BridgeInterface) {
	tool, handler := DocumentDiagnosticsTool(bridge)
	s.AddTool(tool, handler)
}
