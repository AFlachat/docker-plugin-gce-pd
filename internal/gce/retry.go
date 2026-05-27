package gce

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// BackoffConfig controls the exponential-backoff-with-jitter retry loop used
// around transient GCE API failures (HTTP 429/5xx, quota, connection blips).
//
// Operation *completion* is waited on separately (see waitOp) with its own
// timeout; this backoff is only for the API call itself returning an error.
type BackoffConfig struct {
	InitialInterval time.Duration // first sleep before retry
	MaxInterval     time.Duration // cap on a single sleep
	Multiplier      float64       // growth factor per attempt
	MaxElapsed      time.Duration // give up after this much total time
}

// DefaultBackoff is a sensible default: starts at 500ms, doubles up to 30s,
// gives up after 2 minutes total.
func DefaultBackoff() BackoffConfig {
	return BackoffConfig{
		InitialInterval: 500 * time.Millisecond,
		MaxInterval:     30 * time.Second,
		Multiplier:      2.0,
		MaxElapsed:      2 * time.Minute,
	}
}

// retry runs fn, retrying with exponential backoff + full jitter while fn
// returns a retryable error. It stops on success, on a non-retryable error, on
// ctx cancellation, or once MaxElapsed is exceeded.
func retry(ctx context.Context, cfg BackoffConfig, fn func() error) error {
	start := time.Now()
	interval := cfg.InitialInterval

	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !isRetryable(err) {
			return err
		}
		if time.Since(start) >= cfg.MaxElapsed {
			return &RetryExhaustedError{Attempts: attempt + 1, LastErr: err}
		}

		// Full jitter: sleep a random duration in [0, interval]. This spreads
		// out concurrent clients hammering a throttled API.
		sleep := time.Duration(rand.Int63n(int64(interval) + 1))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}

		interval = time.Duration(float64(interval) * cfg.Multiplier)
		if interval > cfg.MaxInterval {
			interval = cfg.MaxInterval
		}
	}
}

// RetryExhaustedError is returned when retry gives up after MaxElapsed.
type RetryExhaustedError struct {
	Attempts int
	LastErr  error
}

func (e *RetryExhaustedError) Error() string {
	return "gce: retries exhausted after " + itoa(e.Attempts) + " attempts: " + e.LastErr.Error()
}
func (e *RetryExhaustedError) Unwrap() error { return e.LastErr }

// isRetryable decides whether an error is worth retrying. It understands both
// REST-style googleapi.Error (the compute client is REST) and gRPC status
// codes, so callers don't have to care which transport produced the error.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}

	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		switch gerr.Code {
		case 429, // Too Many Requests / rate limit
			500, // Internal
			502, // Bad Gateway
			503, // Service Unavailable
			504: // Gateway Timeout
			return true
		default:
			return false
		}
	}

	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.Unavailable, codes.ResourceExhausted, codes.Aborted, codes.Internal:
			return true
		}
	}

	return false
}

// itoa is a tiny helper to avoid pulling strconv into the error path for a
// single small integer.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
