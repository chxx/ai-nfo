package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func setRetries(t *testing.T, s *Store, count int) {
	t.Helper()
	c := s.getConfig()
	c.RetryCount = count
	if _, err := s.saveSettings(SettingsRequest{Config: c}); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticRequestRetries(t *testing.T) {
	for _, endpoint := range []string{"chat", "native", "search"} {
		t.Run(endpoint, func(t *testing.T) {
			s := testStore(t)
			configure(t, s, "https://ai.example", "compatible")
			setRetries(t, s, 1)
			if endpoint == "search" {
				c := s.getConfig()
				c.SearchProvider = "tavily"
				c.SearchURL = "https://search.example"
				c.SearchKey = "search-secret"
				if _, err := s.saveSettings(SettingsRequest{Config: c}); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			firstBody := ""
			s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				calls++
				if calls == 1 {
					firstBody = string(b)
					return mockResponse(r, 429, "application/json", []byte("secret upstream failure")), nil
				}
				if string(b) != firstBody {
					t.Error("request body not replayed")
				}
				response := `{"choices":[{"message":{"content":"{\"title\":\"恢复成功\"}"}}]}`
				if endpoint == "native" {
					response = nativeSearchFixture
				}
				if endpoint == "search" {
					response = `{"results":[{"url":"https://courses.example/lesson","title":"course"}]}`
				}
				return mockResponse(r, 200, "application/json", []byte(response)), nil
			})
			var err error
			if endpoint == "chat" {
				var m *Metadata
				m, err = s.ai(context.Background(), nil)
				if err == nil && m.Title != "恢复成功" {
					t.Fatal(m)
				}
			} else {
				_, err = s.searchPages(context.Background(), "course")
			}
			if err != nil || calls != 2 {
				t.Fatal(err, calls)
			}
		})
	}
}

func TestRetryLimitsAndDelays(t *testing.T) {
	for _, tc := range []struct{ status, retries, want int }{{429, 0, 1}, {401, 3, 1}, {403, 3, 1}, {500, 1, 2}} {
		s := testStore(t)
		configure(t, s, "https://ai.example", "compatible")
		setRetries(t, s, tc.retries)
		calls := 0
		s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return mockResponse(r, tc.status, "application/json", []byte("secret payload")), nil
		})
		_, err := s.ai(context.Background(), nil)
		if err == nil || calls != tc.want || strings.Contains(err.Error(), "secret") {
			t.Fatal(tc, err, calls)
		}
	}
	now := time.Now().Truncate(time.Second)
	if retryDelay(0, "", now) != 2*time.Second || retryDelay(1, "", now) != 4*time.Second || retryDelay(0, "20", now) != 20*time.Second || retryDelay(0, now.Add(time.Minute).UTC().Format(http.TimeFormat), now) != time.Minute || retryDelay(0, "9223372036854775807", now) <= 5*time.Minute {
		t.Fatal("invalid backoff/Retry-After handling")
	}
}

func TestCancelDuringRetryAndManualResume(t *testing.T) {
	s := testStore(t)
	configure(t, s, "https://ai.example", "compatible")
	setRetries(t, s, 2)
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if fail.Load() {
			return mockResponse(r, 429, "application/json", nil), nil
		}
		return mockResponse(r, 200, "application/json", []byte(`{"choices":[{"message":{"content":"{\"title\":\"恢复成功\"}"}}]}`)), nil
	})
	put(t, s, "one.mp4", "")
	ids := scan(t, s, "", "movie")
	id := start(t, s, "generate", ids, false)
	deadline := time.Now().Add(time.Second)
	seen := false
	for time.Now().Before(deadline) {
		jobs, _ := s.listJobs()
		for _, j := range jobs {
			if j.ID == id && strings.Contains(j.Message, "自动重试 1/2") {
				seen = true
			}
		}
		if seen {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !seen {
		t.Fatal("retry progress missing")
	}
	if err := s.cancelJob(id); err != nil {
		t.Fatal(err)
	}
	job := waitJob(t, s, id)
	if job.Status != "cancelled" || job.Done != 0 || calls.Load() != 1 {
		t.Fatal(job, calls.Load())
	}
	fail.Store(false)
	r, err := s.retryJob(id)
	if err != nil {
		t.Fatal(err)
	}
	job = waitJob(t, s, r["id"])
	if job.Status != "completed" || job.Done != 1 || calls.Load() != 2 {
		t.Fatal(job, calls.Load())
	}
}

func TestManualRetryOnlyFailedItemsPreservesOverwrite(t *testing.T) {
	s := testStore(t)
	configure(t, s, "https://ai.example", "compatible")
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		calls.Add(1)
		if fail.Load() && strings.Contains(string(b), "fail.mp4") {
			return mockResponse(r, 429, "application/json", nil), nil
		}
		return mockResponse(r, 200, "application/json", []byte(`{"choices":[{"message":{"content":"{\"title\":\"新草稿\"}"}}]}`)), nil
	})
	put(t, s, "fail.mp4", "")
	put(t, s, "ok.mp4", "")
	ids := scan(t, s, "", "movie")
	for _, id := range ids {
		draft(t, s, id)
	}
	first := start(t, s, "generate", ids, true)
	if job := waitJob(t, s, first); job.Failed != 1 || job.Done != 2 {
		t.Fatal(job)
	}
	fail.Store(false)
	app := newWebApp(s, "")
	w := request(app, "POST", "/api/jobs/retry", encode(map[string]string{"id": first}), nil, "")
	var result map[string]string
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	job := waitJob(t, s, result["id"])
	v, _ := s.getVideo(identifier("fail.mp4"))
	if job.Total != 1 || job.Done != 1 || job.Failed != 0 || job.Skipped != 0 || calls.Load() != 3 || v.Metadata.Title != "新草稿" {
		t.Fatal(job, calls.Load(), v.Metadata)
	}
	again, err := s.retryJob(first)
	if err != nil || again["id"] != "" {
		t.Fatal("successful retry was queued again", again, err)
	}
	protected := newWebApp(s, "password123")
	if w := request(protected, "POST", "/api/jobs/retry", encode(result), nil, ""); w.Code != 401 {
		t.Fatal("retry not authenticated")
	}
	if w := request(app, "POST", "/api/jobs/retry", encode(result), nil, "https://evil.example"); w.Code != 403 {
		t.Fatal("cross-site retry accepted", w.Code)
	}
}

func TestManualSaveRetryAndRetrySettings(t *testing.T) {
	s := testStore(t)
	if s.getConfig().RetryCount != 2 {
		t.Fatal("wrong default retries")
	}
	for _, invalid := range []int{-1, 6} {
		c := s.getConfig()
		c.RetryCount = invalid
		if _, err := s.saveSettings(SettingsRequest{Config: c}); err == nil {
			t.Fatal("invalid retries accepted")
		}
	}
	setRetries(t, s, 0)
	reopened, err := newStore(s.root, s.data)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.getConfig().RetryCount != 0 {
		t.Fatal("disabled retries not persisted")
	}
	reopened.close()
	put(t, s, "video.mp4", "")
	ids := scan(t, s, "", "movie")
	id := start(t, s, "save", ids, false)
	if job := waitJob(t, s, id); job.Failed != 1 {
		t.Fatal(job)
	}
	draft(t, s, ids[0])
	r, err := s.retryJob(id)
	if err != nil {
		t.Fatal(err)
	}
	if job := waitJob(t, s, r["id"]); job.Status != "completed" || job.Done != 1 {
		t.Fatal(job)
	}
}

func TestInterruptedTaskCanBeRetriedAfterRestart(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	s, err := newStore(root, data)
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, "one.mp4", "")
	ids := scan(t, s, "", "movie")
	draft(t, s, ids[0])
	if _, err = s.db.Exec("INSERT INTO jobs(id,action,status,ids,total,created) VALUES('interrupted-save','save','running',?,1,?)", encode(ids), now()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO job_items(job_id,video_id,status,overwrite) VALUES('interrupted-save',?,'running',0)", ids[0]); err != nil {
		t.Fatal(err)
	}
	s.close()
	s, err = newStore(root, data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	result, err := s.retryJob("interrupted-save")
	if err != nil {
		t.Fatal(err)
	}
	if job := waitJob(t, s, result["id"]); job.Status != "completed" || job.Done != 1 {
		t.Fatal(job)
	}
}
