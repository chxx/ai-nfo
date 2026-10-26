package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type retryNoticeKey struct{}

func (s *Store) retryContext(ctx context.Context, jobID string) context.Context {
	return context.WithValue(ctx, retryNoticeKey{}, func(message string) {
		s.db.Exec("UPDATE jobs SET message=? WHERE id=? AND status='running'", message, jobID)
	})
}

func retryDelay(attempt int, header string, now time.Time) time.Duration {
	delay := min(2*time.Second*time.Duration(1<<min(attempt, 4)), 30*time.Second)
	if seconds, err := strconv.ParseInt(strings.TrimSpace(header), 10, 64); err == nil && seconds > 0 {
		if seconds > 300 {
			return 301 * time.Second
		}
		delay = max(delay, time.Duration(seconds)*time.Second)
	} else if date, err := http.ParseTime(header); err == nil {
		delay = max(delay, date.Sub(now))
	}
	return delay
}

func transientStatus(status int) bool {
	return status == 408 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504
}

// Callers hold the shared AI slot through the response body and retry backoff.
func (s *Store) requestBytes(req *http.Request, retries int) (int, []byte, error) {
	return s.requestBytesWithClient(req, retries, s.client, 2000000)
}

func (s *Store) requestBytesWithClient(req *http.Request, retries int, client *http.Client, limit int64) (int, []byte, error) {
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		current := req.Clone(ctx)
		if attempt > 0 && req.Body != nil {
			if req.GetBody == nil {
				return 0, nil, userError("请求无法重试，请重新执行")
			}
			body, err := req.GetBody()
			if err != nil {
				return 0, nil, err
			}
			current.Body = body
		}
		response, err := client.Do(current)
		status, header := 0, ""
		var raw []byte
		if response != nil {
			status, header = response.StatusCode, response.Header.Get("Retry-After")
			if err == nil {
				raw, err = io.ReadAll(io.LimitReader(response.Body, limit+1))
			}
			response.Body.Close()
		}
		if ctx.Err() != nil {
			return status, nil, ctx.Err()
		}
		if int64(len(raw)) > limit {
			return status, nil, userError("接口返回内容过大")
		}
		retryable := transientStatus(status) || err != nil && (status == 0 || status >= 200 && status < 300)
		if !retryable || attempt >= retries {
			return status, raw, err
		}
		delay := retryDelay(attempt, header, time.Now())
		// Never retry earlier than Retry-After when its delay exceeds our wait budget.
		if delay > 5*time.Minute {
			return status, raw, err
		}
		if notice, ok := req.Context().Value(retryNoticeKey{}).(func(string)); ok {
			notice(fmt.Sprintf("接口暂时失败，%.0f 秒后自动重试 %d/%d", delay.Seconds(), attempt+1, retries))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status, nil, ctx.Err()
		case <-timer.C:
		}
	}
}
