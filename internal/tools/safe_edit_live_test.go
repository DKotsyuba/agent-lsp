// safe_edit_live_test.go is a live-gopls regression for the safe_apply_edit
// internal preview encoding mismatch: HandleSafeApplyEdit parses the preview
// result returned by HandleSimulateEditAtomic as JSON, but that call honored
// the caller's requested output format (GCF/JSON) from ctx, so a caller
// requesting GCF output made the internal parse fail before any disk write.
package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/blackwell-systems/agent-lsp/internal/lsp"
	"github.com/blackwell-systems/agent-lsp/internal/session"
)

// liveResolver resolves every request to a single pre-initialized client,
// mirroring the shape lsp.ClientResolver needs for SessionManager.CreateSession.
// A nil client is valid: it forces CreateSession/ApplyEdit to fail, letting a
// test exercise session-creation-failure propagation without a fake LSP server.
type liveResolver struct{ client *lsp.LSPClient }

// ClientForFile returns the single pre-initialized client regardless of path.
func (r *liveResolver) ClientForFile(string) *lsp.LSPClient { return r.client }

// DefaultClient returns the single pre-initialized client.
func (r *liveResolver) DefaultClient() *lsp.LSPClient { return r.client }

// AllClients returns the single pre-initialized client as a one-element slice.
func (r *liveResolver) AllClients() []*lsp.LSPClient { return []*lsp.LSPClient{r.client} }

// Shutdown is a no-op; the test owns the client's lifecycle via t.Cleanup.
func (r *liveResolver) Shutdown(context.Context) error { return nil }

// setupLiveSafeEdit starts a real gopls against a small temp module and
// returns a ready client plus session manager, or skips the test if gopls
// is unavailable.
func setupLiveSafeEdit(t *testing.T) (context.Context, *lsp.LSPClient, *session.SessionManager, string) {
	t.Helper()
	gopls := findGopls()
	if gopls == "" {
		t.Skip("gopls not found; skipping live safe_apply_edit integration test")
	}

	dir := t.TempDir()
	// macOS returns /var/folders/... from t.TempDir(), which is a symlink to
	// /private/var/folders/...; ValidateFilePath resolves symlinks on the file
	// path it validates, so the module root passed to gopls must be resolved
	// the same way or gopls reports the file as outside its workspace.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module safeedittest\n\ngo 1.21\n"), 0644); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(dir, "main.go")
	src := "package main\n\nfunc add(a, b int) int {\n\treturn a + b\n}\n"
	if err := os.WriteFile(srcPath, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	client := lsp.NewLSPClient(gopls, nil)
	if err := client.Initialize(ctx, dir); err != nil {
		t.Fatalf("initialize gopls: %v", err)
	}
	t.Cleanup(func() { client.Shutdown(context.Background()) })

	// Warm baseline diagnostics for the file before any test edit. The
	// evaluate step captures baseline diagnostics lazily on a file's first
	// edit; without this warm-up the baseline snapshot can race with gopls's
	// diagnostics for the edited (post-change) content, understating
	// net_delta for edits that introduce new errors.
	fileURI := CreateFileURI(srcPath)
	if err := client.OpenDocument(ctx, fileURI, src, "go"); err != nil {
		t.Fatalf("open document: %v", err)
	}
	if err := lsp.WaitForDiagnostics(ctx, client, []string{fileURI}, 5000); err != nil {
		t.Fatalf("wait for baseline diagnostics: %v", err)
	}

	mgr := session.NewSessionManager(&liveResolver{client: client})
	return ctx, client, mgr, srcPath
}

// TestHandleSafeApplyEdit_SafeEditAppliesAcrossFormats is a table-driven
// regression: a net-delta-zero edit must be applied to disk and reported as
// applied=true regardless of the caller's requested output format (default
// JSON or GCF), proving the internal forced-JSON preview parse in
// HandleSafeApplyEdit does not leak into the outer encoding.
func TestHandleSafeApplyEdit_SafeEditAppliesAcrossFormats(t *testing.T) {
	cases := []struct {
		name   string
		format string
	}{
		{name: "default_json", format: ""},
		{name: "gcf", format: "gcf"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, client, mgr, srcPath := setupLiveSafeEdit(t)
			if tc.format != "" {
				ctx = ContextWithOutputFormat(ctx, tc.format)
			}

			args := map[string]any{
				"file_path": srcPath,
				"old_text":  "return a + b",
				"new_text":  "return a + b // sum",
			}
			res, err := HandleSafeApplyEdit(ctx, client, mgr, args)
			if err != nil {
				t.Fatalf("HandleSafeApplyEdit returned error: %v", err)
			}
			if res.IsError {
				t.Fatalf("HandleSafeApplyEdit returned error result: %s", resultText(res))
			}
			out := resultText(res)
			if strings.Contains(out, "failed to parse preview result") {
				t.Fatalf("internal preview GCF/JSON mismatch resurfaced: %s", out)
			}

			switch tc.format {
			case "gcf":
				if !strings.HasPrefix(out, "GCF profile=") {
					t.Errorf("expected GCF-encoded result, got: %s", out)
				}
				if !strings.Contains("\n"+out+"\n", "\napplied=true\n") {
					t.Errorf("expected applied=true in GCF result, got: %s", out)
				}
				if !strings.Contains("\n"+out+"\n", "\nnet_delta=0\n") {
					t.Errorf("expected net_delta=0 in GCF result, got: %s", out)
				}
			default:
				var parsed map[string]any
				if jsonErr := json.Unmarshal([]byte(res.Content[0].Text), &parsed); jsonErr != nil {
					t.Fatalf("expected outer result to be valid JSON, got %q: %v", res.Content[0].Text, jsonErr)
				}
				applied, ok := parsed["applied"].(bool)
				if !ok || !applied {
					t.Errorf("expected applied=true (bool) in JSON result, got: %v", parsed["applied"])
				}
				if nd, ok := parsed["net_delta"].(float64); !ok || nd != 0 {
					t.Errorf("expected net_delta=0 in JSON result, got: %v", parsed["net_delta"])
				}
			}

			got, err := os.ReadFile(srcPath)
			if err != nil {
				t.Fatal(err)
			}
			want := "package main\n\nfunc add(a, b int) int {\n\treturn a + b // sum\n}\n"
			if string(got) != want {
				t.Errorf("disk content mismatch:\nwant:\n%s\ngot:\n%s", want, got)
			}
		})
	}
}

// TestHandleSafeApplyEdit_GCFCallerRefusesUnsafeEdit verifies that a
// diagnostic-introducing edit is still refused (net_delta > 0) and disk is
// left byte-for-byte untouched, even when the caller requested GCF output.
func TestHandleSafeApplyEdit_GCFCallerRefusesUnsafeEdit(t *testing.T) {
	ctx, client, mgr, srcPath := setupLiveSafeEdit(t)
	ctx = ContextWithOutputFormat(ctx, "gcf")

	before, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}

	args := map[string]any{
		"file_path": srcPath,
		"old_text":  "return a + b",
		"new_text":  "return a +", // introduces a syntax error
	}
	res, err := HandleSafeApplyEdit(ctx, client, mgr, args)
	if err != nil {
		t.Fatalf("HandleSafeApplyEdit returned error: %v", err)
	}
	if res.IsError {
		t.Fatalf("HandleSafeApplyEdit returned error result: %s", resultText(res))
	}
	out := resultText(res)
	if strings.Contains(out, "failed to parse preview result") {
		t.Fatalf("internal preview GCF/JSON mismatch resurfaced: %s", out)
	}
	if !strings.HasPrefix(out, "GCF profile=") {
		t.Errorf("expected GCF-encoded result, got: %s", out)
	}
	if !strings.Contains("\n"+out+"\n", "\napplied=false\n") {
		t.Errorf("expected applied=false in refusal result, got: %s", out)
	}
	if !regexp.MustCompile(`(?m)^net_delta=[1-9][0-9]*$`).MatchString(out) {
		t.Errorf("expected positive net_delta in refusal result, got: %s", out)
	}

	after, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("disk content changed despite unsafe edit refusal:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestHandleSimulateEditAtomic_GCFCallerReturnsGCF is a direct-caller
// regression proving HandleSimulateEditAtomic itself still honors the
// caller's requested output format (GCF) for its own return value, disjoint
// from HandleSafeApplyEdit's internal forced-JSON preview parse, and leaves
// disk untouched since it only simulates.
func TestHandleSimulateEditAtomic_GCFCallerReturnsGCF(t *testing.T) {
	ctx, client, mgr, srcPath := setupLiveSafeEdit(t)
	ctx = ContextWithOutputFormat(ctx, "gcf")

	before, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}

	simArgs := map[string]any{
		"workspace_root": client.RootDir(),
		"language":       client.LanguageIDForFile(srcPath),
		"file_path":      srcPath,
		"start_line":     4,
		"start_column":   2,
		"end_line":       4,
		"end_column":     14,
		"new_text":       "return a + b // sum",
	}
	res, err := HandleSimulateEditAtomic(ctx, mgr, simArgs)
	if err != nil {
		t.Fatalf("HandleSimulateEditAtomic returned error: %v", err)
	}
	if res.IsError {
		t.Fatalf("HandleSimulateEditAtomic returned error result: %s", resultText(res))
	}
	out := resultText(res)
	if !strings.HasPrefix(out, "GCF profile=") {
		t.Errorf("expected GCF-encoded result, got: %s", out)
	}

	after, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("disk content changed by a preview-only simulate_edit_atomic call:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestHandleSafeApplyEdit_SessionCreateFailurePropagatesAndLeavesDiskUnchanged
// verifies that when the underlying session manager cannot create a session
// (e.g. because the resolved client is unusable), HandleSafeApplyEdit
// surfaces the failure as an error result instead of silently applying, and
// leaves disk byte-for-byte unchanged.
func TestHandleSafeApplyEdit_SessionCreateFailurePropagatesAndLeavesDiskUnchanged(t *testing.T) {
	ctx, client, _, srcPath := setupLiveSafeEdit(t)

	before, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}

	// A session manager backed by a liveResolver with a nil client fails at
	// CreateSession, so the preview step in HandleSafeApplyEdit must fail
	// before any disk write is attempted.
	brokenMgr := session.NewSessionManager(&liveResolver{client: nil})

	args := map[string]any{
		"file_path": srcPath,
		"old_text":  "return a + b",
		"new_text":  "return a + b // sum",
	}
	res, err := HandleSafeApplyEdit(ctx, client, brokenMgr, args)
	if err != nil {
		t.Fatalf("HandleSafeApplyEdit returned error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result when session creation fails, got: %s", resultText(res))
	}
	out := resultText(res)
	if !strings.Contains(out, "create_session failed:") {
		t.Errorf("expected the session-creation failure to propagate verbatim, got: %s", out)
	}

	after, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("disk content changed despite session creation failure:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
