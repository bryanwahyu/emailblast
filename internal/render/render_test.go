package render

import (
	"strings"
	"testing"

	"emailblast/internal/model"
)

// TestHTMLEscaping proves the body uses html/template: a hostile merge value is
// context-escaped instead of breaking markup or injecting HTML. The subject is
// text/template, so it is NOT escaped (plain text has no markup to protect).
func TestHTMLEscaping(t *testing.T) {
	r, err := New("Hi {{.name}}", "<p>Hello {{.name}}, welcome</p>")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	u := model.User{
		ID:     "u1",
		Email:  "x@example.com",
		Fields: map[string]string{"name": "O'Brien & <Sons>"},
	}
	msg, err := r.Render(u)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Body: dangerous chars must be entity-escaped.
	if strings.Contains(msg.Body, "<Sons>") {
		t.Errorf("body not HTML-escaped: %q", msg.Body)
	}
	for _, want := range []string{"&lt;Sons&gt;", "&amp;", "&#39;"} {
		if !strings.Contains(msg.Body, want) {
			t.Errorf("body missing escape %q; got %q", want, msg.Body)
		}
	}

	// Subject: plain text, left verbatim.
	if msg.Subject != "Hi O'Brien & <Sons>" {
		t.Errorf("subject should be verbatim text, got %q", msg.Subject)
	}
}

// TestMissingKeyEmpty confirms unknown merge fields render empty, not an error.
func TestMissingKeyEmpty(t *testing.T) {
	r, err := New("Hi {{.first_name}}", "<p>{{.nope}}</p>")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msg, err := r.Render(model.User{ID: "u2", Fields: map[string]string{}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if msg.Body != "<p></p>" {
		t.Errorf("missing key should render empty, got %q", msg.Body)
	}
}
