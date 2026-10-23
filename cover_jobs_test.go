package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAutoCoverGenerationAndSeriesDedup(t *testing.T) {
	s := testStore(t)
	fakeMediaTools(t, `{"streams":[{"index":1,"codec_type":"video","codec_name":"png","disposition":{"attached_pic":1}}]}`, testCoverPNG(t))
	configure(t, s, "https://ai.example", "compatible")
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Path != "/chat/completions" {
			t.Errorf("embedded cover should not search: %s", r.URL.Path)
		}
		return mockResponse(r, 200, "application/json", []byte(`{"choices":[{"message":{"content":"{\"title\":\"课程\",\"plot\":\"简介\"}"}}]}`)), nil
	})
	put(t, s, "course/S01E01.mp4", "source")
	put(t, s, "course/S01E02.mp4", "source")
	ids := scan(t, s, "course", "episode")
	result, err := s.startJob(JobRequest{Action: "generate", IDs: ids})
	if err != nil || result["cover_job_id"] == "" {
		t.Fatal(result, err)
	}
	for _, key := range []string{"id", "cover_job_id"} {
		job := waitJob(t, s, result[key])
		if job.Status != "completed" || job.Failed != 0 {
			t.Fatal(job)
		}
		if key == "cover_job_id" && (job.Total != 3 || job.Done != 3) {
			t.Fatal("series not deduplicated", job)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("unexpected model calls", calls.Load())
	}
	for _, path := range []string{"course/poster.jpg", "course/S01E01-thumb.jpg", "course/S01E02-thumb.jpg"} {
		if !s.videoAvailable(path) {
			t.Fatal("missing", path)
		}
	}
	for _, id := range ids {
		v, _ := s.getVideo(id)
		if v.Status != "draft" || v.Metadata.Title != "课程" {
			t.Fatal("cover changed draft", v)
		}
	}
	job := waitJob(t, s, start(t, s, "covers", ids, false))
	if job.Skipped != 3 || job.Failed != 0 || calls.Load() != 3 {
		t.Fatal("existing covers not preserved", job)
	}
	// Opting out creates only the text job.
	no := false
	result, err = s.startJob(JobRequest{Action: "generate", IDs: ids, AutoCovers: &no})
	if err != nil || result["cover_job_id"] != "" {
		t.Fatal(result, err)
	}
	waitJob(t, s, result["id"])
}

func TestAutoCoverGlobalConcurrencyCancelAndRetry(t *testing.T) {
	s := testStore(t)
	fakeMediaTools(t, `{"streams":[]}`, testCoverPNG(t))
	configure(t, s, "https://ai.example", "compatible")
	var active, peak atomic.Int32
	entered := make(chan struct{}, 16)
	resume := make(chan struct{})
	// Avoid external calls; hold native searches until cancellation or explicit release.
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-resume:
			return mockResponse(r, 400, "application/json", []byte(`{}`)), nil
		}
	})
	for i := range 8 {
		put(t, s, fmt.Sprintf("video%d.mp4", i), "source")
	}
	ids := scan(t, s, "", "movie")
	first := start(t, s, "covers", ids[:4], false)
	for range 4 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("four workers did not start")
		}
	}
	second := start(t, s, "covers", ids[4:], false)
	duplicate := start(t, s, "covers", ids, false)
	if duplicate != "" {
		t.Fatal("active targets duplicated", duplicate)
	}
	if _, err := s.searchCovers(context.Background(), CoverRequest{ID: ids[0], Scope: "video"}); err == nil {
		t.Fatal("manual search exceeded shared cap")
	}
	select {
	case <-entered:
		t.Fatal("more than four concurrent pipelines")
	case <-time.After(80 * time.Millisecond):
	}
	// A text/NFO job is independent of the blocked cover queue.
	waitJob(t, s, start(t, s, "save", ids[:1], false))
	if err := s.cancelJob(second); err != nil {
		t.Fatal(err)
	}
	if err := s.cancelJob(first); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first, second} {
		if job := waitJob(t, s, id); job.Status != "cancelled" {
			t.Fatal(job)
		}
	}
	if peak.Load() != 4 || active.Load() != 0 {
		t.Fatal(peak.Load(), active.Load())
	}
	if files, _ := filepath.Glob(filepath.Join(s.root, "*.jpg")); len(files) != 0 {
		t.Fatal("cancel wrote covers", files)
	}
	close(resume)
	for _, id := range []string{first, second} {
		retry, err := s.retryCoverJob(id)
		if err != nil || retry["id"] == "" {
			t.Fatal(retry, err)
		}
		job := waitJob(t, s, retry["id"])
		if job.Status != "completed" || job.Done != 4 {
			t.Fatal("screenshot fallback failed", job)
		}
	}
	if peak.Load() > 4 {
		t.Fatal("retry exceeded cap")
	}
}

func TestAutoCoverWebSelectionAndFailureIsolation(t *testing.T) {
	for _, match := range []string{"likely", "uncertain"} {
		t.Run(match, func(t *testing.T) {
			s := testStore(t)
			fakeMediaTools(t, `{"streams":[]}`, testCoverPNG(t))
			configure(t, s, "https://ai.example", "compatible")
			mockPublicPages(t, s)
			s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/responses" {
					return mockResponse(r, 200, "application/json", []byte(nativeSearchFixture)), nil
				}
				var body struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) < 2 {
					t.Error("invalid ranking request")
					return nil, fmt.Errorf("invalid request")
				}
				var input struct {
					Candidates []CoverCandidate `json:"candidates"`
				}
				if err := json.Unmarshal([]byte(body.Messages[1].Content), &input); err != nil || len(input.Candidates) != 1 {
					t.Error("missing candidates")
					return nil, fmt.Errorf("invalid candidates")
				}
				content := encode(map[string]any{"matches": []any{map[string]string{"id": input.Candidates[0].ID, "match": match, "reason": "来源匹配"}}})
				return mockResponse(r, 200, "application/json", []byte(encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}}))), nil
			})
			put(t, s, "video.mp4", "source")
			ids := scan(t, s, "", "movie")
			draft(t, s, ids[0])
			job := waitJob(t, s, start(t, s, "covers", ids, false))
			if job.Status != "completed" {
				t.Fatal(job)
			}
			var raw string
			if err := s.db.QueryRow("SELECT metadata FROM cover_selections").Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var chosen CoverCandidate
			json.Unmarshal([]byte(raw), &chosen)
			if match == "likely" && chosen.SourceURL == "" || match == "uncertain" && chosen.Kind != "snapshot" {
				t.Fatal("wrong automatic choice", chosen)
			}
			// Inaccessible source failure must not overwrite the text draft status.
			os.Remove(filepath.Join(s.root, "video-poster.jpg"))
			os.Remove(filepath.Join(s.root, "video.mp4"))
			job = waitJob(t, s, start(t, s, "covers", ids, false))
			v, _ := s.getVideo(ids[0])
			if job.Failed != 1 || v.Status != "draft" || v.Metadata == nil {
				t.Fatal(job, v)
			}
		})
	}
}

func TestAutoCoverSelectionAndRestart(t *testing.T) {
	candidates := []CoverCandidate{{ID: "uncertain", Match: "uncertain", Width: 4000, Height: 3000}, {ID: "small", Match: "likely", Width: 128, Height: 72}, {ID: "large", Match: "likely", Width: 1280, Height: 720}}
	if got := bestAutoCover(candidates, true, "video"); got == nil || got.ID != "large" {
		t.Fatal(got)
	}
	if got := bestAutoCover(candidates[:1], true, "video"); got != nil {
		t.Fatal("uncertain source selected")
	}
	root, data := t.TempDir(), t.TempDir()
	s, err := newStore(root, data)
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, "video.mp4", "source")
	put(t, s, "video-poster.jpg", "preserve")
	ids := scan(t, s, "", "movie")
	tasks, err := s.coverTargets(ids)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := queueCoverTasks(tx, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s.close()
	s, err = newStore(root, data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if job := waitJob(t, s, id); job.Status != "interrupted" {
		t.Fatal(job)
	}
	items, err := s.coverTaskList(id)
	if err != nil || len(items) != 1 || items[0].Status != "interrupted" {
		t.Fatal(items, err)
	}
	retry, err := s.retryCoverJob(id)
	if err != nil {
		t.Fatal(err)
	}
	if job := waitJob(t, s, retry["id"]); job.Skipped != 1 || job.Status != "completed" {
		t.Fatal(job)
	}
	app := newWebApp(s, "password123")
	for _, path := range []string{"/api/cover-tasks?id=" + id, "/api/cover-jobs/retry"} {
		method := "GET"
		if path == "/api/cover-jobs/retry" {
			method = "POST"
		}
		if w := request(app, method, path, `{"id":"`+id+`"}`, nil, ""); w.Code != 401 {
			t.Fatal("missing auth", path, w.Code)
		}
	}
}
