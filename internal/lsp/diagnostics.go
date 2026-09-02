package lsp

import (
	"context"
	"sync"
	"time"

	"github.com/blackwell-systems/agent-lsp/internal/types"
)

// diagnosticsQuietWindow is how long publishDiagnostics traffic must stay
// silent, once every waited-for URI has fresh diagnostics, before the wait
// resolves. Servers often publish twice in quick succession (an empty clear
// on didClose followed by the real set on didOpen); the window absorbs that
// burst so callers read the settled set.
const diagnosticsQuietWindow = 500 * time.Millisecond

// WaitForDiagnostics waits until every uri has diagnostics that describe the
// text the client last sent for it (see LSPClient.DiagnosticsFresh), then until
// diagnosticsQuietWindow elapses with no further publishDiagnostics
// notification for any URI. The client records the sync point itself when it
// sends didOpen/didChange, so callers simply send content (or call
// ReopenDocument) and then wait: a publish that arrives before the wait starts
// is counted, the cache replay performed by SubscribeToDiagnostics is never
// mistaken for a publish, and a URI whose diagnostics are already fresh
// resolves after one quiet window.
// An empty uris list resolves immediately. Returns nil when settled or when
// timeoutMs milliseconds elapse (the caller then reads whatever is cached),
// and ctx.Err() if ctx is cancelled first.
func WaitForDiagnostics(ctx context.Context, client *LSPClient, uris []string, timeoutMs int) error {
	if len(uris) == 0 {
		return nil
	}

	var mu sync.Mutex
	lastEvent := time.Now()

	// notify wakes the loop on every publish so settlement is checked
	// immediately instead of on the next 50ms tick.
	notify := make(chan struct{}, 1)
	cb := types.DiagnosticUpdateCallback(func(_ string, _ []types.LSPDiagnostic) {
		mu.Lock()
		lastEvent = time.Now()
		mu.Unlock()
		select {
		case notify <- struct{}{}:
		default:
		}
	})
	client.SubscribeToDiagnostics(cb)
	defer client.UnsubscribeFromDiagnostics(cb)

	settled := func() bool {
		mu.Lock()
		quiet := time.Since(lastEvent) >= diagnosticsQuietWindow
		mu.Unlock()
		if !quiet {
			return false
		}
		for _, uri := range uris {
			if !client.DiagnosticsFresh(uri) {
				return false
			}
		}
		return true
	}

	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-notify:
		}
		if time.Now().After(deadline) || settled() {
			return nil
		}
	}
}
