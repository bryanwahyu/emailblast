//go:build !ses

package main

import (
	"context"
	"fmt"

	"emailblast/internal/sender"
)

// newSES is a stub unless built with `-tags ses`, keeping the default build
// dependency-free. Rebuild with the AWS SDK to enable real SES sending.
func newSES(_ context.Context, _ string) (sender.Sender, error) {
	return nil, fmt.Errorf("ses backend not compiled in; build with: go build -tags ses .")
}
