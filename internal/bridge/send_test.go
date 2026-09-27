package bridge

import (
	"context"
	"errors"
	"testing"
)

func TestSendWithRetryRetriesTransientErrors(t *testing.T) {
	attempts := 0
	err := sendWithRetry(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary failure")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("sendWithRetry() error = %v", err)
	}
	if attempts != 3 {
		t.Fatalf("sendWithRetry() attempts = %d, want 3", attempts)
	}
}

func TestSendWithRetryReturnsFinalError(t *testing.T) {
	attempts := 0
	wantErr := errors.New("persistent failure")
	err := sendWithRetry(context.Background(), func() error {
		attempts++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("sendWithRetry() error = %v, want %v", err, wantErr)
	}
	if attempts != 3 {
		t.Fatalf("sendWithRetry() attempts = %d, want 3", attempts)
	}
}
