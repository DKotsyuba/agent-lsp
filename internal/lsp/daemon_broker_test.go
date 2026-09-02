package lsp

import (
	"strings"
	"testing"

	"github.com/blackwell-systems/agent-lsp/internal/types"
)

// TestPublishDiagnosticsFrame_RoundTrip verifies that the frame the broker
// forwards is decoded by a client read loop into the diagnostics cache and
// advances the per-URI publish counter, that the document version stamp
// survives the round trip, and that nil findings are encoded as an empty
// array rather than null.
func TestPublishDiagnosticsFrame_RoundTrip(t *testing.T) {
	c, serverW, _ := newTestClient(t)
	uri := "file:///d.py"

	body, err := publishDiagnosticsFrame(uri, []types.LSPDiagnostic{{Message: "boom"}}, 7, true)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := serverW.Write(EncodeMessage(body)); err != nil {
		t.Fatalf("write: %v", err)
	}

	waitPublishCount(t, c, uri, 1)
	if diags := c.GetDiagnostics(uri); len(diags) != 1 || diags[0].Message != "boom" {
		t.Errorf("cache = %+v, want one diagnostic 'boom'", diags)
	}
	if v, ok := c.PublishedVersion(uri); !ok || v != 7 {
		t.Errorf("published version = %d,%v, want 7,true", v, ok)
	}

	empty, err := publishDiagnosticsFrame(uri, nil, 0, false)
	if err != nil {
		t.Fatalf("encode nil: %v", err)
	}
	if !strings.Contains(string(empty), `"diagnostics":[]`) {
		t.Errorf("nil diagnostics encoded as %s, want empty array", empty)
	}
	if strings.Contains(string(empty), `"version"`) {
		t.Errorf("unstamped frame must not carry a version: %s", empty)
	}
}
