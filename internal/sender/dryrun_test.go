package sender

import (
	"context"
	"testing"

	"emailblast/internal/model"
	"emailblast/internal/render"
)

// recordingSender is a spy inner backend; DryRun must never call it.
type recordingSender struct{ called int }

func (r *recordingSender) Name() string { return "rec" }
func (r *recordingSender) Send(ctx context.Context, u model.User, m render.Message, key string) error {
	r.called++
	return nil
}

func TestDryRunSendsNothing(t *testing.T) {
	inner := &recordingSender{}
	dr := NewDryRun(inner, false)

	for i := 0; i < 5; i++ {
		if err := dr.Send(context.Background(), model.User{Email: "a@x.com"}, render.Message{}, "k"); err != nil {
			t.Fatal(err)
		}
	}
	if inner.called != 0 {
		t.Errorf("inner backend called %d times; dry run must send nothing", inner.called)
	}
	if dr.Count() != 5 {
		t.Errorf("dry run count=%d want 5", dr.Count())
	}
	if dr.Name() != "rec+dryrun" {
		t.Errorf("name=%q want rec+dryrun", dr.Name())
	}
}

func TestDryRunRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inner := &recordingSender{}
	dr := NewDryRun(inner, false)
	if err := dr.Send(ctx, model.User{}, render.Message{}, "k"); err == nil {
		t.Error("expected context error")
	}
}

// TestUnsubscribeHeader covers the List-Unsubscribe construction used by SMTP+SES.
func TestUnsubscribeHeader(t *testing.T) {
	u := Unsubscribe{URL: "https://you.com/u?e={{email}}", Mailto: "unsub@you.com"}
	got := u.Header("a@x.com")
	want := "<https://you.com/u?e=a@x.com>, <mailto:unsub@you.com?subject=unsubscribe>"
	if got != want {
		t.Errorf("Header=%q want %q", got, want)
	}
	if !u.OneClick() {
		t.Error("URL present should enable one-click")
	}
	if (Unsubscribe{}).Header("a@x.com") != "" {
		t.Error("empty config should yield empty header")
	}
}
