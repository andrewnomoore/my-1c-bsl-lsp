package bridge

import (
	"sync"

	"time"

	"rockerboo/mcp-lsp-bridge/types"
	"rockerboo/mcp-lsp-bridge/utils"

	"github.com/mark3labs/mcp-go/server"
)

// MCPLSPBridge combines MCP server capabilities with multiple LSP clients
type MCPLSPBridge struct {
	server             *server.MCPServer
	clients            map[types.LanguageServer]types.LanguageClientInterface
	config             types.LSPServerConfigProvider
	allowedDirectories []string
	pathMapper         *utils.DockerPathMapper
	mu                 sync.RWMutex

	// Track documents opened via didOpen to avoid duplicate notifications.
	openedDocuments   map[string]bool
	openedDocumentsMu sync.RWMutex

	// Auto-connect support: connect default language client(s) once, lazily.
	autoConnectMu          sync.Mutex
	autoConnectStartedAt   time.Time
	autoConnectLastAttempt time.Time

	// Warm-up support: best-effort indexing/caching to make heavy LSP tools reliable.
	warmupMu          sync.Mutex
	warmupStartedAt   time.Time
	warmupFinishedAt  time.Time
	warmupLastAttempt time.Time
	warmupRunning     bool
	warmupDone        bool
	warmupErr         string
}

// WarmupStatus returns current warm-up state.
func (b *MCPLSPBridge) WarmupStatus() (running bool, done bool, err string, startedAt time.Time, finishedAt time.Time) {
	b.warmupMu.Lock()
	defer b.warmupMu.Unlock()
	return b.warmupRunning, b.warmupDone, b.warmupErr, b.warmupStartedAt, b.warmupFinishedAt
}

// ListConnectedClients returns a snapshot of currently connected clients.
// This is intentionally NOT part of interfaces.BridgeInterface to avoid breaking mocks;
// consume via type assertion in tooling.
func (b *MCPLSPBridge) ListConnectedClients() map[types.LanguageServer]types.LanguageClientInterface {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[types.LanguageServer]types.LanguageClientInterface, len(b.clients))
	for k, v := range b.clients {
		out[k] = v
	}
	return out
}

// GetConnectedLanguages returns a list of languages for which clients are already connected.
// This provides a fast path for tools that need language info without expensive filesystem scans.
// It resolves server names to actual language identifiers by scanning extension_language_map
// and checking which languages have matching connected servers.
func (b *MCPLSPBridge) GetConnectedLanguages() []types.Language {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.config == nil || len(b.clients) == 0 {
		return nil
	}

	// Collect all connected server names.
	connectedServers := make(map[types.LanguageServer]bool, len(b.clients))
	for serverKey := range b.clients {
		connectedServers[serverKey] = true
	}

	// Resolve server names → language IDs by checking each known language.
	// GetServerNameFromLanguage("bsl") → "bsl-language-server"; if that server is connected, include "bsl".
	seen := make(map[types.Language]bool)
	var languages []types.Language

	// Try all languages referenced in language_servers config.
	// We iterate all server configs and for each try FindAllServerConfigs with the server name as language,
	// but that won't work. Instead, scan known file extensions to discover language IDs.
	for _, lang := range b.getKnownLanguages() {
		server := b.config.GetServerNameFromLanguage(lang)
		if server != "" && connectedServers[server] {
			if !seen[lang] {
				seen[lang] = true
				languages = append(languages, lang)
			}
		}
	}

	return languages
}

// getKnownLanguages returns all language IDs known to the config (from extension_language_map).
func (b *MCPLSPBridge) getKnownLanguages() []types.Language {
	// Type-assert to access LanguageServerMap for direct server→languages mapping.
	type languageServerMapProvider interface {
		GetLanguageServerMap() map[types.LanguageServer][]types.Language
	}
	if provider, ok := b.config.(languageServerMapProvider); ok {
		seen := make(map[types.Language]bool)
		var langs []types.Language
		for _, languages := range provider.GetLanguageServerMap() {
			for _, lang := range languages {
				if !seen[lang] {
					seen[lang] = true
					langs = append(langs, lang)
				}
			}
		}
		return langs
	}

	// Fallback: try extension language map via type assertion.
	type extensionMapProvider interface {
		GetExtensionLanguageMap() map[string]types.Language
	}
	if provider, ok := b.config.(extensionMapProvider); ok {
		seen := make(map[types.Language]bool)
		var langs []types.Language
		for _, lang := range provider.GetExtensionLanguageMap() {
			if !seen[lang] {
				seen[lang] = true
				langs = append(langs, lang)
			}
		}
		return langs
	}

	return nil
}

// AllClientsInSessionMode returns true if ALL connected clients use session mode.
// In session mode, LSP Session Manager handles initialization and warmup,
// so mcp-lsp-bridge should skip its own warmup gate.
func (b *MCPLSPBridge) AllClientsInSessionMode() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if len(b.clients) == 0 {
		return false
	}

	// Get all server configs
	serverConfigs := b.config.GetLanguageServers()

	// Check each connected client's server config
	// Note: b.clients keys are language names (e.g., "bsl"), not server names
	for langKey := range b.clients {
		// Try to find server config by treating key as server name first
		serverConfig, exists := serverConfigs[langKey]
		if !exists {
			// Key might be a language, find the server name for it
			serverName := b.config.GetServerNameFromLanguage(types.Language(langKey))
			if serverName == "" {
				return false
			}
			serverConfig, exists = serverConfigs[serverName]
		}
		if !exists || serverConfig == nil || !serverConfig.IsSessionMode() {
			return false
		}
	}
	return true
}
