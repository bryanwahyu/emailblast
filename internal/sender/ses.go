//go:build ses

// Package sender: Amazon SES v2 backend. Compiled only with the "ses" build
// tag so the default build stays dependency-free:
//
//	go get github.com/aws/aws-sdk-go-v2/config github.com/aws/aws-sdk-go-v2/service/sesv2
//	go build -tags ses ./...
//
// SES is the recommended ESP for high-volume: cheapest ($0.10/1k) and highest
// throughput once you request a sending-quota increase. Batch is unnecessary
// here because the worker pool already parallelizes SendEmail calls.
package sender

import (
	"context"
	"errors"
	"fmt"

	"emailblast/internal/model"
	"emailblast/internal/render"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	smithy "github.com/aws/smithy-go"
)

// SESSender delivers via Amazon SES v2. The underlying client is safe for
// concurrent use, so one instance is shared by every worker.
type SESSender struct {
	client *sesv2.Client
	from   string // verified sender identity, e.g. "News <news@example.com>"
	unsub  Unsubscribe
}

// NewSES loads AWS config from the standard chain (env, shared config, IAM role)
// and returns a ready sender. from must be a verified SES identity.
func NewSES(ctx context.Context, from string, unsub Unsubscribe) (*SESSender, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &SESSender{client: sesv2.NewFromConfig(cfg), from: from, unsub: unsub}, nil
}

func (s *SESSender) Name() string { return "ses" }

// unsubHeaders maps the Unsubscribe config to SES Simple-message headers. Nil
// when nothing is configured (SES rejects empty header values).
func unsubHeaders(unsub Unsubscribe, recipient string) []types.MessageHeader {
	v := unsub.Header(recipient)
	if v == "" {
		return nil
	}
	hs := []types.MessageHeader{
		{Name: aws.String("List-Unsubscribe"), Value: aws.String(v)},
	}
	if unsub.OneClick() {
		hs = append(hs, types.MessageHeader{
			Name:  aws.String("List-Unsubscribe-Post"),
			Value: aws.String("List-Unsubscribe=One-Click"),
		})
	}
	return hs
}

func (s *SESSender) Send(ctx context.Context, u model.User, msg render.Message, key string) error {
	_, err := s.client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(s.from),
		Destination:      &types.Destination{ToAddresses: []string{u.Email}},
		Content: &types.EmailContent{
			Simple: &types.Message{
				Subject: &types.Content{Data: aws.String(msg.Subject)},
				Body:    &types.Body{Html: &types.Content{Data: aws.String(msg.Body)}},
				Headers: unsubHeaders(s.unsub, u.Email),
			},
		},
		// Dedupe key: SES does not natively dedupe, but propagating it as a tag
		// lets a downstream suppression/event pipeline discard resend duplicates.
		EmailTags: []types.MessageTag{{Name: aws.String("idkey"), Value: aws.String(key)}},
	})
	if err != nil {
		if isRetryableSES(err) {
			return fmt.Errorf("%w: %v", ErrRetryable, err)
		}
		return err // permanent: bad address, etc. -> DLQ
	}
	return nil
}

// isRetryableSES treats throttling and 5xx-class faults as retryable and
// everything else (validation, rejected address) as permanent.
func isRetryableSES(err error) bool {
	var te interface{ ErrorFault() smithy.ErrorFault }
	if errors.As(err, &te) && te.ErrorFault() == smithy.FaultServer {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "TooManyRequestsException", "Throttling", "ThrottlingException",
			"ServiceUnavailable", "RequestTimeout":
			return true
		}
	}
	return false
}
