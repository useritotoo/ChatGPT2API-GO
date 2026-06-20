package app

import (
	"context"
	"errors"
	"sync"
	"time"
)

func (s *Server) imageRequestTimeout() time.Duration {
	opts := s.imageGenerationOptions()
	return opts.Timeout + opts.PollInitialWait + 60*time.Second
}

func (s *Server) imageGenerationOptions() imageGenerationOptions {
	return normalizeImageGenerationOptions(imageGenerationOptions{
		Timeout:         time.Duration(s.cfg.ImagePollTimeoutSecs) * time.Second,
		PollInterval:    time.Duration(s.cfg.ImagePollIntervalSecs) * time.Second,
		PollInitialWait: time.Duration(s.cfg.ImagePollInitialWaitSecs) * time.Second,
		UploadTimeout:   time.Duration(s.cfg.ImagePollTimeoutSecs) * time.Second,
	})
}

func (s *Server) generateImageWithPool(ctx context.Context, prompt, model, size, resolution string, refs [][]byte) ([]upstreamImageResult, error) {
	accounts := s.store.LoadAccounts()
	maxAttempts := len(accounts)
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	excluded := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		traceLogf(ctx, "├─ image account attempt %d/%d model=%s resolution=%s excluded=%d", attempt+1, maxAttempts, model, resolution, len(excluded))
		account, err := s.pickAccountExcluding(model, resolution, excluded)
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		client, actualAccount, err := s.upstreamClientForImageAccount(model, resolution, account)
		if err != nil {
			s.accountPool.releaseToken(account.AccessToken)
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		poolToken := account.AccessToken
		token := actualAccount.AccessToken
		traceLogf(ctx, "│  ├─ selected image account %s", accountLabel(actualAccount))
		excluded[poolToken] = true
		excluded[token] = true
		items, err := client.GenerateImage(ctx, prompt, model, size, resolution, refs, s.imageGenerationOptions())
		s.accountPool.releaseToken(poolToken)
		if err == nil {
			traceLogf(ctx, "└─ image account attempt %d success images=%d", attempt+1, len(items))
			s.markAccountSuccess(token, true)
			return items, nil
		}
		traceLogf(ctx, "│  └─ image account attempt %d failed error=%v", attempt+1, err)
		s.markAccountFailure(token, err, true)
		lastErr = err
		if !shouldRetryImageAccount(err) {
			return nil, err
		}
		traceLogf(ctx, "│  ├─ retry with another image account")
	}
	if lastErr == nil {
		lastErr = errors.New("no available image quota")
	}
	return nil, lastErr
}

func (s *Server) generateImagesWithPool(ctx context.Context, prompt, model, size, resolution string, refs [][]byte, n int) ([]upstreamImageResult, error) {
	if n <= 1 {
		return s.generateImageWithPool(ctx, prompt, model, size, resolution, refs)
	}
	limit := s.cfg.ImageAccountConcurrency
	if limit <= 0 {
		limit = 1
	}
	if limit > n {
		limit = n
	}
	sem := make(chan struct{}, limit)
	results := make([][]upstreamImageResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()
			items, err := s.generateImageWithPool(ctx, prompt, model, size, resolution, refs)
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = items
		}()
	}
	wg.Wait()
	out := []upstreamImageResult{}
	var lastErr error
	for i := 0; i < n; i++ {
		if len(results[i]) > 0 {
			out = append(out, results[i][0])
		}
		if errs[i] != nil {
			lastErr = errs[i]
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("upstream returned no image")
}

func shouldRetryImageAccount(err error) bool {
	if err == nil {
		return false
	}
	return isRateLimitErrorText(err) || isInvalidTokenErrorText(err) || isUpstreamBlockErrorText(err) || isTurnstileRequirementErrorText(err) || isRetryableBootstrapError(err) || isTemporaryUpstreamErrorText(err)
}
