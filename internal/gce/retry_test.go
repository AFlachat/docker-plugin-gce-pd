package gce

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
)

func fastBackoff() BackoffConfig {
	return BackoffConfig{
		InitialInterval: time.Millisecond,
		MaxInterval:     2 * time.Millisecond,
		Multiplier:      2.0,
		MaxElapsed:      50 * time.Millisecond,
	}
}

func TestRetrySucceedsAfterTransientErrors(t *testing.T) {
	calls := 0
	err := retry(context.Background(), fastBackoff(), func() error {
		calls++
		if calls < 3 {
			return &googleapi.Error{Code: 503}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry error = %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestRetryStopsOnNonRetryable(t *testing.T) {
	calls := 0
	want := &googleapi.Error{Code: 400}
	err := retry(context.Background(), fastBackoff(), func() error {
		calls++
		return want
	})
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 400)", calls)
	}
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != 400 {
		t.Errorf("err = %v, want the 400 passed through", err)
	}
}

func TestRetryExhausts(t *testing.T) {
	err := retry(context.Background(), fastBackoff(), func() error {
		return &googleapi.Error{Code: 429}
	})
	var ex *RetryExhaustedError
	if !errors.As(err, &ex) {
		t.Fatalf("err = %v, want RetryExhaustedError", err)
	}
}

func TestRetryHonoursContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	err := retry(ctx, fastBackoff(), func() error {
		return &googleapi.Error{Code: 503}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestIsRetryable(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 504}
	for _, code := range retryable {
		if !isRetryable(&googleapi.Error{Code: code}) {
			t.Errorf("isRetryable(%d) = false, want true", code)
		}
	}
	for _, code := range []int{400, 401, 403, 404, 409} {
		if isRetryable(&googleapi.Error{Code: code}) {
			t.Errorf("isRetryable(%d) = true, want false", code)
		}
	}
	if isRetryable(nil) {
		t.Error("isRetryable(nil) = true")
	}
	if isRetryable(errors.New("plain")) {
		t.Error("isRetryable(plain error) = true")
	}
}
