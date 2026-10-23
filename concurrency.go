package main

import (
	"context"
	"sync"
)

// Reducing the limit preserves in-flight reservations and blocks new work
// until the number of active requests is below the new limit.
type workLimiter struct {
	mu            sync.Mutex
	limit, active int
	changed       chan struct{}
}

func newWorkLimiter(limit int) *workLimiter {
	return &workLimiter{limit: limit, changed: make(chan struct{})}
}

func (l *workLimiter) setLimit(limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limit = limit
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *workLimiter) acquire(ctx, service context.Context, wait bool) (func(), error) {
	for {
		l.mu.Lock()
		if err := ctx.Err(); err != nil {
			l.mu.Unlock()
			return nil, err
		}
		if err := service.Err(); err != nil {
			l.mu.Unlock()
			return nil, err
		}
		if l.active < l.limit {
			l.active++
			l.mu.Unlock()
			return func() {
				l.mu.Lock()
				defer l.mu.Unlock()
				l.active--
				close(l.changed)
				l.changed = make(chan struct{})
			}, nil
		}
		changed := l.changed
		l.mu.Unlock()
		if !wait {
			return nil, userError("封面操作已达到配置的并发上限，请稍后重试")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-service.Done():
			return nil, service.Err()
		case <-changed:
		}
	}
}

func (c *Config) validateConcurrency() error {
	if c.RetryCount < 0 || c.RetryCount > 5 {
		return userError("自动重试次数须为 0–5 的整数")
	}
	if c.Concurrency == 0 {
		c.Concurrency = 1
	}
	if c.Concurrency < 1 || c.Concurrency > 8 {
		return userError("并发数量须为 1–8 的整数")
	}
	return nil
}
