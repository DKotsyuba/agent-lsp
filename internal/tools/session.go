package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/blackwell-systems/agent-lsp/internal/lsp"
	"github.com/blackwell-systems/agent-lsp/internal/types"
)

// HandleStartLsp starts (or restarts) the LSP server. If an existing client is
// non-nil, Shutdown is called before creating the new client.
func HandleStartLsp(
	ctx context.Context,
	getClient func() *lsp.LSPClient,
	setClient func(*lsp.LSPClient),
	serverPath string,
	serverArgs []string,
	args map[string]any,
) (types.ToolResult, error) {
	rootDir, ok := args["root_dir"].(string)
	if !ok || rootDir == "" {
		return types.ErrorResult("root_dir is required"), nil
	}

	languageID, _ := args["language_id"].(string)

	// Shutdown any existing client.
	if existing := getClient(); existing != nil {
		_ = existing.Shutdown(ctx) // best-effort
	}

	// Passive mode: connect to an already-running language server via TCP.
	// Skips subprocess spawn; reuses the IDE's warm index.
	if connectAddr, ok := args["connect"].(string); ok && connectAddr != "" {
		client, err := lsp.NewPassiveClient(connectAddr)
		if err != nil {
			return types.ErrorResult(fmt.Sprintf("passive connect failed: %s", err)), nil
		}
		if err := client.Initialize(ctx, rootDir); err != nil {
			_ = client.Shutdown(ctx)
			return types.ErrorResult(fmt.Sprintf("passive initialize failed: %s", err)), nil
		}
		setClient(client)
		return appendHint(types.TextResult("Connected to existing language server at "+connectAddr), "Workspace initialized. Use list_symbols or get_diagnostics to begin analysis."), nil
	}

	// Optional workspace scoping: generate a language-server config file that
	// limits indexing to specific subdirectories.
	var scopeConfig *lsp.ScopeConfig
	if rawScope, ok := args["scope"]; ok {
		scopePaths := ParseScopePaths(rawScope)
		if len(scopePaths) > 0 {
			sc, err := lsp.GenerateScopeConfig(rootDir, languageID, scopePaths)
			if err != nil {
				return types.ErrorResult(fmt.Sprintf("failed to generate scope config: %s", err)), nil
			}
			scopeConfig = sc
		}
	}

	client := lsp.NewLSPClient(serverPath, serverArgs)
	if err := client.Initialize(ctx, rootDir); err != nil {
		// Clean up scope config on init failure.
		lsp.RemoveScopeConfig(scopeConfig)
		return types.ErrorResult(fmt.Sprintf("failed to initialize LSP server: %s", err)), nil
	}

	if scopeConfig != nil {
		client.SetScopeConfig(scopeConfig)
	}

	// Auto-scope activation: when no manual scope was specified and the
	// workspace is large enough to benefit, enable automatic scope shifting.
	if scopeConfig == nil && languageID != "" {
		if lsp.ShouldAutoScope(rootDir, languageID) {
			client.SetAutoScope(true, nil)
		}
	}

	setClient(client)

	// Optional: block until $/progress indexing completes before returning.
	// Useful for servers like jdtls that index the workspace asynchronously
	// after initialize and need time before Tier 2 tools return results.
	if secs, ok := args["ready_timeout_seconds"].(float64); ok && secs > 0 {
		timeout := time.Duration(secs) * time.Second
		client.WaitForWorkspaceReadyTimeout(ctx, timeout)
	}

	return appendHint(types.TextResult("LSP server started successfully"), "Workspace initialized. Use list_symbols or get_diagnostics to begin analysis."), nil
}

// ParseScopePaths extracts scope paths from the args value.
// Accepts a single string, a JSON-encoded array string, or []interface{} (JSON array).
func ParseScopePaths(raw any) []string {
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		// If the string looks like a JSON array, decode it.
		// This handles the case where a typed string field receives
		// a JSON array from the client (e.g., scope: ["a", "b"]).
		if strings.HasPrefix(strings.TrimSpace(v), "[") {
			var arr []string
			if json.Unmarshal([]byte(v), &arr) == nil && len(arr) > 0 {
				return arr
			}
		}
		return []string{v}
	case []any:
		paths := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				paths = append(paths, s)
			}
		}
		return paths
	default:
		return nil
	}
}

// HandleRestartLspServer restarts the LSP server with the given root dir.
// root_dir is required: omitting it would construct a malformed "file://" rootURI.
func HandleRestartLspServer(ctx context.Context, client *lsp.LSPClient, args map[string]any) (types.ToolResult, error) {
	if err := CheckInitialized(client); err != nil {
		return types.ErrorResult(err.Error()), nil
	}

	rootDir, _ := args["root_dir"].(string)
	if rootDir == "" {
		return types.ErrorResult("root_dir is required for restart_lsp_server"), nil
	}
	if err := client.Restart(ctx, rootDir); err != nil {
		return types.ErrorResult(fmt.Sprintf("failed to restart LSP server: %s", err)), nil
	}
	// M4: In multi-server configurations only the default client is restarted.
	// Other configured servers remain running. Restart each independently if needed.
	return types.TextResult("LSP server restarted successfully. Note: in multi-server configurations only the default server was restarted; other configured servers are unaffected."), nil
}

// HandleOpenDocument opens a document in the LSP server. args: file_path
// (required, validated against the workspace root), language_id (optional,
// inferred from the extension) and text (optional; when omitted the file's
// current disk content is sent, since the server treats the didOpen text as
// the document). Reopening an already open document sends a full-content
// didChange instead. Returns an error result for a missing/invalid path or a
// failed send; on success it may also shift the auto-scope to the file's package.
func HandleOpenDocument(ctx context.Context, client *lsp.LSPClient, args map[string]any) (types.ToolResult, error) {
	if err := CheckInitialized(client); err != nil {
		return types.ErrorResult(err.Error()), nil
	}

	filePath, ok := args["file_path"].(string)
	if !ok || filePath == "" {
		return types.ErrorResult("file_path is required"), nil
	}

	// Validate path to prevent traversal attacks, consistent with WithDocument.
	cleanPath, err := ValidateFilePath(filePath, client.RootDir())
	if err != nil {
		return types.ErrorResult(fmt.Sprintf("invalid file_path: %s", err)), nil
	}

	languageID, _ := args["language_id"].(string)
	if languageID == "" {
		languageID = client.LanguageIDForFile(filePath)
	}

	// text is an optional Go-specific extension not present in the TypeScript schema.
	// Callers may provide file content directly to avoid a disk read. When it is
	// omitted the file is read from disk: the didOpen text is authoritative for
	// the server (it does not read the file itself), so an empty text would make
	// it analyse an empty document until the next reopen. If the file cannot be
	// read (e.g. a not-yet-saved document) the document is opened empty.
	text, _ := args["text"].(string)
	if text == "" {
		if data, readErr := os.ReadFile(cleanPath); readErr == nil {
			text = string(data)
		}
	}
	fileURI := CreateFileURI(filePath)

	if err := client.OpenDocument(ctx, fileURI, text, languageID); err != nil {
		return types.ErrorResult(fmt.Sprintf("failed to open document: %s", err)), nil
	}

	// Auto-scope: shift the scope to the package containing this file.
	if client.AutoScope() {
		lsp.UpdateAutoScope(client, filePath, languageID)
	}

	return types.TextResult(fmt.Sprintf("Document opened: %s", filePath)), nil
}

// HandleCloseDocument closes a document in the LSP server.
func HandleCloseDocument(ctx context.Context, client *lsp.LSPClient, args map[string]any) (types.ToolResult, error) {
	if err := CheckInitialized(client); err != nil {
		return types.ErrorResult(err.Error()), nil
	}

	filePath, ok := args["file_path"].(string)
	if !ok || filePath == "" {
		return types.ErrorResult("file_path is required"), nil
	}

	fileURI := CreateFileURI(filePath)
	if err := client.CloseDocument(ctx, fileURI); err != nil {
		return types.ErrorResult(fmt.Sprintf("failed to close document: %s", err)), nil
	}
	return types.TextResult(fmt.Sprintf("Document closed: %s", filePath)), nil
}
