// Package render turns a template + a user's merge fields into a concrete
// subject and body. Templates are parsed once and reused across all workers
// (both text/template and html/template are safe for concurrent Execute).
//
// The subject is text/template (plain text). The body is html/template, which
// context-aware auto-escapes merge values so a name like `O'Brien & <Sons>`
// becomes `O&#39;Brien &amp; &lt;Sons&gt;` in the HTML instead of breaking the
// markup or opening an injection hole.
package render

import (
	"bytes"
	"fmt"
	htmltmpl "html/template"
	texttmpl "text/template"

	"emailblast/internal/model"
)

// Message is a fully rendered, ready-to-send email.
type Message struct {
	Subject string
	Body    string
}

// Renderer holds pre-parsed templates shared by all workers.
type Renderer struct {
	subject *texttmpl.Template
	body    *htmltmpl.Template
}

// New parses the subject (text) and body (HTML) templates once. Use Go template
// syntax referencing merge fields, e.g. "Hi {{.first_name}}". Missing keys
// render as empty rather than erroring (Option "missingkey=zero").
func New(subjectTmpl, bodyTmpl string) (*Renderer, error) {
	s, err := texttmpl.New("subject").Option("missingkey=zero").Parse(subjectTmpl)
	if err != nil {
		return nil, fmt.Errorf("parse subject: %w", err)
	}
	b, err := htmltmpl.New("body").Option("missingkey=zero").Parse(bodyTmpl)
	if err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	return &Renderer{subject: s, body: b}, nil
}

// Render executes both templates against the user's merge fields. Buffers are
// caller-local so this is safe to call concurrently from every worker. HTML
// escaping of merge values is automatic in the body.
func (r *Renderer) Render(u model.User) (Message, error) {
	var sb, bb bytes.Buffer
	if err := r.subject.Execute(&sb, u.Fields); err != nil {
		return Message{}, fmt.Errorf("render subject for %s: %w", u.ID, err)
	}
	if err := r.body.Execute(&bb, u.Fields); err != nil {
		return Message{}, fmt.Errorf("render body for %s: %w", u.ID, err)
	}
	return Message{Subject: sb.String(), Body: bb.String()}, nil
}
