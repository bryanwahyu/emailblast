package main

import (
	"context"
	"fmt"
	"time"

	"emailblast/internal/model"
	"emailblast/internal/sender"
	"emailblast/internal/source"
)

// sourceStream launches the producer in a goroutine and returns a channel that
// yields its terminal error once the stream is fully drained/closed.
func sourceStream(path string, out chan<- model.User) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- source.NewCSV(path).Stream(out) }()
	return errc
}

// buildSender constructs the chosen ESP backend. The "ses" case is only real
// when compiled with `-tags ses`; otherwise newSES returns an error (see the
// build-tagged files ses_enabled.go / ses_disabled.go).
func buildSender(
	ctx context.Context,
	backend, from string, verbose bool, mockDelay time.Duration, mockFail int64,
	smtpHost, smtpPort, smtpUser, smtpPass string, smtpTLS bool, smtpPool int,
	unsub sender.Unsubscribe,
) (sender.Sender, error) {
	switch backend {
	case "mock":
		return sender.NewMock(mockFail, mockDelay, verbose), nil
	case "smtp":
		if smtpHost == "" {
			return nil, fmt.Errorf("smtp backend needs -smtp-host")
		}
		return sender.NewSMTP(smtpHost, smtpPort, from, smtpUser, smtpPass, smtpTLS, smtpPool, unsub), nil
	case "ses":
		return newSES(ctx, from, unsub)
	default:
		return nil, fmt.Errorf("unknown backend %q (want mock|smtp|ses)", backend)
	}
}
