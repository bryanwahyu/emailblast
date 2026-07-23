//go:build ses

package main

import (
	"context"

	"emailblast/internal/sender"
)

func newSES(ctx context.Context, from string, unsub sender.Unsubscribe) (sender.Sender, error) {
	return sender.NewSES(ctx, from, unsub)
}
