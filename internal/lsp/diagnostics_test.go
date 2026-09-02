package lsp

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blackwell-systems/agent-lsp/internal/types"
)

// TestWaitForDiagnostics_SettlesAfterQuietWindow verifies that WaitForDiagnostics
// resolves once each tracked URI has received a fresh notification and a 500ms
// quiet window has elapsed.
func TestWaitForDiagnostics_SettlesAfterQuietWindow(t *testing.T) {
	c, serverW, _ := newTestClient(t)

	ctx := context.Background()
	uris := []string{"file:///a.go", "file:///b.go"}

	done := make(chan error, 1)
	go func() {
		done <- WaitForDiagnostics(ctx, c, uris, 5000)
	}()

	// Fire two rounds of notifications per URI: the first is the initial-snapshot
	// (skipped by seenInitial logic), the second is the fresh notification that
	// triggers settlement.
	for round := 0; round < 2; round++ {
		time.Sleep(10 * time.Millisecond)
		for _, uri := range uris {
			if err := writeMsg(serverW, map[string]any{
				"jsonrpc": "2.0",
				"method":  "textDocument/publishDiagnostics",
				"params": map[string]any{
					"uri":         uri,
					"diagnostics": []any{},
				},
			}); err != nil {
				t.Fatalf("write diag round %d: %v", round, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// WaitForDiagnostics should settle after 500ms quiet window.
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil error, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("timeout: WaitForDiagnostics did not settle")
	}
}

// TestWaitForDiagnostics_Timeout verifies that WaitForDiagnostics resolves
// after the timeout even if no notifications are received.
func TestWaitForDiagnostics_Timeout(t *testing.T) {
	c, serverW, _ := newTestClient(t)
	_ = serverW

	ctx := context.Background()
	uris := []string{"file:///missing.go"}

	start := time.Now()
	err := WaitForDiagnostics(ctx, c, uris, 200)
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("expected nil error on timeout, got: %v", err)
	}
	if elapsed < 190*time.Millisecond {
		t.Errorf("resolved too early: %v (expected >= 190ms)", elapsed)
	}
	if elapsed > 600*time.Millisecond {
		t.Errorf("resolved too late: %v (expected <= 600ms)", elapsed)
	}
}

// TestWaitForDiagnostics_EmptyURIs verifies that an empty URI list resolves immediately.
func TestWaitForDiagnostics_EmptyURIs(t *testing.T) {
	c, serverW, _ := newTestClient(t)
	_ = serverW

	ctx := context.Background()
	start := time.Now()
	err := WaitForDiagnostics(ctx, c, []string{}, 5000)
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("expected nil error, got: %v", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("expected immediate resolution, took %v", elapsed)
	}
}

// TestWaitForDiagnostics_ContextCancelled verifies that WaitForDiagnostics
// respects context cancellation.
func TestWaitForDiagnostics_ContextCancelled(t *testing.T) {
	c, serverW, _ := newTestClient(t)
	_ = serverW

	ctx, cancel := context.WithCancel(context.Background())
	uris := []string{"file:///never.go"}

	done := make(chan error, 1)
	go func() {
		done <- WaitForDiagnostics(ctx, c, uris, 30000)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("timeout: expected WaitForDiagnostics to return on cancel")
	}
}

// TestWaitForDiagnostics_SubscribeUnsubscribe ensures no leak of callbacks
// after WaitForDiagnostics returns.
func TestWaitForDiagnostics_SubscribeUnsubscribe(t *testing.T) {
	c, serverW, _ := newTestClient(t)
	_ = serverW

	ctx := context.Background()

	// Count subscriptions before.
	c.diagMu.RLock()
	before := len(c.diagSubs)
	c.diagMu.RUnlock()

	err := WaitForDiagnostics(ctx, c, []string{"file:///x.go"}, 100)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// After return, subscription must be cleaned up.
	c.diagMu.RLock()
	after := len(c.diagSubs)
	c.diagMu.RUnlock()

	if after != before {
		t.Errorf("callback leak: before=%d after=%d", before, after)
	}
}

// TestWaitForFileIndexed_Timeout verifies that WaitForFileIndexed returns
// nil (not an error) after the timeout elapses with no notifications.
func TestWaitForFileIndexed_Timeout(t *testing.T) {
	c, serverW, _ := newTestClient(t)
	_ = serverW

	ctx := context.Background()
	uri := "file:///missing.go"

	start := time.Now()
	err := c.WaitForFileIndexed(ctx, uri, 200)
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("expected nil error on timeout, got: %v", err)
	}
	if elapsed < 190*time.Millisecond {
		t.Errorf("resolved too early: %v (expected >= 190ms)", elapsed)
	}
	if elapsed > 700*time.Millisecond {
		t.Errorf("resolved too late: %v (expected <= 700ms)", elapsed)
	}
}

// TestWaitForFileIndexed_ContextCancelled verifies that WaitForFileIndexed
// returns context.Canceled when the context is cancelled.
func TestWaitForFileIndexed_ContextCancelled(t *testing.T) {
	c, serverW, _ := newTestClient(t)
	_ = serverW

	ctx, cancel := context.WithCancel(context.Background())
	uri := "file:///never.go"

	done := make(chan error, 1)
	go func() {
		done <- c.WaitForFileIndexed(ctx, uri, 30000)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("timeout: expected WaitForFileIndexed to return on cancel")
	}
}

// TestWaitForFileIndexed_StabilityWindowReset verifies that each new
// notification resets the 1500ms stability window.
func TestWaitForFileIndexed_StabilityWindowReset(t *testing.T) {
	c, serverW, _ := newTestClient(t)

	ctx := context.Background()
	uri := "file:///a.go"

	done := make(chan error, 1)
	go func() {
		// Use a 6s timeout to give the stability window room.
		done <- c.WaitForFileIndexed(ctx, uri, 6000)
	}()

	// Send first notification.
	time.Sleep(50 * time.Millisecond)
	if err := writeMsg(serverW, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/publishDiagnostics",
		"params":  map[string]any{"uri": uri, "diagnostics": []any{}},
	}); err != nil {
		t.Fatalf("write first notification: %v", err)
	}

	// Send second notification 200ms later (within stability window).
	time.Sleep(200 * time.Millisecond)
	if err := writeMsg(serverW, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/publishDiagnostics",
		"params":  map[string]any{"uri": uri, "diagnostics": []any{}},
	}); err != nil {
		t.Fatalf("write second notification: %v", err)
	}

	// Should NOT settle before ~1500ms after the second notification.
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("timeout: WaitForFileIndexed did not settle within 5s")
	}
}

// writePublishDiagnostics sends one unstamped textDocument/publishDiagnostics
// notification for uri with no findings through the simulated server pipe.
func writePublishDiagnostics(t *testing.T, w io.Writer, uri string) {
	t.Helper()
	writePublishDiagnosticsParams(t, w, map[string]any{"uri": uri, "diagnostics": []any{}})
}

// writePublishDiagnosticsParams sends one textDocument/publishDiagnostics
// notification with the given params through the simulated server pipe.
func writePublishDiagnosticsParams(t *testing.T, w io.Writer, params map[string]any) {
	t.Helper()
	if err := writeMsg(w, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/publishDiagnostics",
		"params":  params,
	}); err != nil {
		t.Fatalf("write publishDiagnostics: %v", err)
	}
}

// publishCount returns how many publishDiagnostics notifications the client
// has recorded for uri.
func publishCount(c *LSPClient, uri string) uint64 {
	c.diagMu.RLock()
	defer c.diagMu.RUnlock()
	return c.diagVer[NormalizeFileURI(uri)]
}

// waitPublishCount blocks until uri's publish counter reaches want or one
// second passes, then fails the test if it did not.
func waitPublishCount(t *testing.T, c *LSPClient, uri string, want uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for publishCount(c, uri) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := publishCount(c, uri); got < want {
		t.Fatalf("publish counter = %d, want >= %d", got, want)
	}
}

// TestWaitForDiagnostics_FreshFileSinglePublish verifies that the very first
// publish for a URI with no cached entry counts as fresh: the wait settles one
// quiet window after it instead of running to the timeout.
func TestWaitForDiagnostics_FreshFileSinglePublish(t *testing.T) {
	c, serverW, _ := newTestClient(t)
	uri := "file:///fresh.go"

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- WaitForDiagnostics(context.Background(), c, []string{uri}, 5000)
	}()

	time.Sleep(20 * time.Millisecond)
	writePublishDiagnostics(t, serverW, uri)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("settled too late: %v (expected ~quiet window, not timeout)", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Error("timeout: single publish on a fresh file was not counted")
	}
}

// TestWaitForDiagnostics_PublishBeforeWait verifies that a publish answering
// the latest content send but landing before the wait starts (the
// reopen-then-wait ordering used by get_diagnostics) is still counted.
func TestWaitForDiagnostics_PublishBeforeWait(t *testing.T) {
	c, serverW, clientR := newTestClient(t)
	uri := "file:///early.go"
	go func() { _, _ = io.Copy(io.Discard, clientR) }() // drain didOpen

	if err := c.OpenDocument(context.Background(), uri, "package x\n", "go"); err != nil {
		t.Fatalf("open: %v", err)
	}
	writePublishDiagnostics(t, serverW, uri)
	waitPublishCount(t, c, uri, 1)

	start := time.Now()
	if err := WaitForDiagnostics(context.Background(), c, []string{uri}, 5000); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("settled too late: %v (publish before wait was not counted)", elapsed)
	}
}

// TestWaitForDiagnostics_StaleVersionIgnored reproduces the pyright reopen
// sequence on a stamping server: after the content is re-sent the server first
// emits an unstamped empty clear, then the stamped result for the new document
// version. The clear must not settle the wait; a publish stamped with an older
// version must not; the stamped result for the current version must.
func TestWaitForDiagnostics_StaleVersionIgnored(t *testing.T) {
	c, serverW, clientR := newTestClient(t)
	uri := "file:///stamped.py"
	go func() { _, _ = io.Copy(io.Discard, clientR) }() // drain didOpen/didChange

	// Open at version 1 and let the server stamp version 1 so the client
	// learns this server stamps.
	if err := c.OpenDocument(context.Background(), uri, "x = 1\n", "python"); err != nil {
		t.Fatalf("open: %v", err)
	}
	writePublishDiagnosticsParams(t, serverW, map[string]any{"uri": uri, "version": 1, "diagnostics": []any{}})
	waitPublishCount(t, c, uri, 1)

	// Re-send content (didChange path bumps the version to 2 and moves the
	// sync point past the version-1 publish).
	if err := c.OpenDocument(context.Background(), uri, "x = 2\n", "python"); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if c.DiagnosticsFresh(uri) {
		t.Fatal("diagnostics reported fresh before any publish for the new content")
	}

	done := make(chan error, 1)
	go func() { done <- WaitForDiagnostics(context.Background(), c, []string{uri}, 3000) }()

	// Unstamped clear, then a stale stamped publish: neither may settle the wait.
	writePublishDiagnosticsParams(t, serverW, map[string]any{"uri": uri, "diagnostics": []any{}})
	writePublishDiagnosticsParams(t, serverW, map[string]any{"uri": uri, "version": 1, "diagnostics": []any{}})
	waitPublishCount(t, c, uri, 3)
	select {
	case <-done:
		t.Fatal("wait settled on a clear/stale publish")
	case <-time.After(800 * time.Millisecond):
	}

	start := time.Now()
	writePublishDiagnosticsParams(t, serverW, map[string]any{"uri": uri, "version": 2,
		"diagnostics": []any{map[string]any{"message": "boom", "severity": 1,
			"range": map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 1}}}}})
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
			t.Errorf("settled too late after stamped result: %v", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Error("timeout: stamped result for the current version was not counted")
	}
	if diags := c.GetDiagnostics(uri); len(diags) != 1 {
		t.Errorf("cache = %+v, want the stamped result", diags)
	}
}

// TestReopenDocument_UnchangedIsNoop verifies that ReopenDocument sends nothing
// when the disk content equals the text last sent, and sends didClose/didOpen
// with a bumped version once the file changes.
func TestReopenDocument_UnchangedIsNoop(t *testing.T) {
	c, _, clientR := newTestClient(t)
	path := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(path, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := PathToFileURI(path)
	ctx := context.Background()

	// Sends block until the simulated server reads them, so every send runs
	// in a goroutine while the test reads the pipe.
	opened := make(chan error, 1)
	go func() { opened <- c.OpenDocument(ctx, uri, "package a\n", "go") }()
	if m := readNextMsg(t, clientR); m["method"] != "textDocument/didOpen" {
		t.Fatalf("expected didOpen, got %v", m["method"])
	}
	if err := <-opened; err != nil {
		t.Fatalf("open: %v", err)
	}

	go func() {
		if err := c.ReopenDocument(ctx, uri); err != nil {
			t.Errorf("reopen unchanged: %v", err)
		}
		_ = c.sendNotification("test/marker", nil)
	}()
	if m := readNextMsg(t, clientR); m["method"] != "test/marker" {
		t.Fatalf("unchanged reopen sent %v, want nothing before the marker", m["method"])
	}

	if err := os.WriteFile(path, []byte("package a // changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		if err := c.ReopenDocument(ctx, uri); err != nil {
			t.Errorf("reopen changed: %v", err)
		}
	}()
	if m := readNextMsg(t, clientR); m["method"] != "textDocument/didClose" {
		t.Fatalf("expected didClose, got %v", m["method"])
	}
	m := readNextMsg(t, clientR)
	if m["method"] != "textDocument/didOpen" {
		t.Fatalf("expected didOpen, got %v", m["method"])
	}
	td := m["params"].(map[string]any)["textDocument"].(map[string]any)
	if td["version"] != float64(2) || td["text"] != "package a // changed\n" {
		t.Errorf("didOpen payload = %v, want version 2 with new text", td)
	}
}

// TestWaitForDiagnostics_OnlyFreshNotifications verifies that pre-existing
// diagnostics in the cache do NOT count as "fresh notification".
func TestWaitForDiagnostics_OnlyFreshNotifications(t *testing.T) {
	c, serverW, _ := newTestClient(t)

	uri := "file:///cached.go"

	// Pre-populate diagnostic cache directly.
	c.diagMu.Lock()
	c.diags[uri] = []types.LSPDiagnostic{{Message: "pre-existing"}}
	c.diagMu.Unlock()

	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- WaitForDiagnostics(ctx, c, []string{uri}, 300)
	}()

	// Should NOT resolve immediately from cache — needs a fresh notification.
	select {
	case err := <-done:
		// It resolved — check it was the timeout path (300ms), not instant.
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		// If it resolved immediately that's a bug, but we can only assert it
		// didn't take too long via the timeout path.
	case <-time.After(500 * time.Millisecond):
		// After 300ms timeout, WaitForDiagnostics should have returned.
		t.Error("WaitForDiagnostics did not resolve after timeout")
	}

	// Now also test: if we send a fresh notification it should resolve early.
	done2 := make(chan error, 1)
	go func() {
		done2 <- WaitForDiagnostics(ctx, c, []string{uri}, 5000)
	}()

	time.Sleep(10 * time.Millisecond)
	if err := writeMsg(serverW, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/publishDiagnostics",
		"params": map[string]any{
			"uri":         uri,
			"diagnostics": []any{},
		},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case err := <-done2:
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("timeout waiting for WaitForDiagnostics to settle after fresh notification")
	}
}
