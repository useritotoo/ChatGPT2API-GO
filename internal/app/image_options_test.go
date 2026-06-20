package app

import (
	"errors"
	"testing"
	"time"
)

func TestNormalizeImageGenerationOptionsDefaults(t *testing.T) {
	opts := normalizeImageGenerationOptions(imageGenerationOptions{})
	if opts.Timeout != 120*time.Second {
		t.Fatalf("Timeout = %s, want 120s", opts.Timeout)
	}
	if opts.PollInterval != 4*time.Second {
		t.Fatalf("PollInterval = %s, want 4s", opts.PollInterval)
	}
	if opts.PollInitialWait != 0 {
		t.Fatalf("PollInitialWait = %s, want 0", opts.PollInitialWait)
	}
	if opts.UploadTimeout != 120*time.Second {
		t.Fatalf("UploadTimeout = %s, want 120s", opts.UploadTimeout)
	}
}

func TestImageGenerationOptionsFromConfig(t *testing.T) {
	s := &Server{cfg: Config{ImagePollTimeoutSecs: 600, ImagePollIntervalSecs: 7, ImagePollInitialWaitSecs: 5}}
	opts := s.imageGenerationOptions()
	if opts.Timeout != 600*time.Second || opts.UploadTimeout != 600*time.Second {
		t.Fatalf("timeout opts = %#v, want 600s", opts)
	}
	if opts.PollInterval != 7*time.Second {
		t.Fatalf("PollInterval = %s, want 7s", opts.PollInterval)
	}
	if opts.PollInitialWait != 5*time.Second {
		t.Fatalf("PollInitialWait = %s, want 5s", opts.PollInitialWait)
	}
}

func TestShouldRetryImageAccountForTemporaryTimeouts(t *testing.T) {
	if !shouldRetryImageAccount(errors.New("image generation SSE timed out (600s)")) {
		t.Fatal("expected image SSE timeout to be retryable")
	}
	if !shouldRetryImageAccount(errors.New("GET /backend-api/conversation failed: status=503 body=busy")) {
		t.Fatal("expected upstream 503 to be retryable")
	}
}
