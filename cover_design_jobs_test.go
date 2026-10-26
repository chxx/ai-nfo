package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestBatchDesignSeriesOnlyDedupAndOverwrite(t *testing.T) {
	s := testStore(t)
	configureImages(t, s)
	fakeMediaTools(t, `{"streams":[]}`, previewPNG(t, 1280, 720))
	for _, path := range []string{"alpha/S01E01.mp4", "alpha/S01E02.mp4", "beta/S02E01.mp4", "movies/movie.mp4"} {
		put(t, s, path, "source")
	}
	ids := scan(t, s, "alpha", "episode")
	ids = append(ids, scan(t, s, "beta", "episode")...)
	ids = append(ids, scan(t, s, "movies", "movie")...)
	ids = append(ids, ids[0])
	old := previewPNG(t, 1280, 720)
	put(t, s, "beta/poster.jpg", string(old))
	portrait := previewPNG(t, 1024, 1536)
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Path != "/v1/images/edits" {
			t.Errorf("batch used wrong pipeline: %s", r.URL.Path)
		}
		return imageResponse(r, portrait), nil
	})
	w := request(newWebApp(s, ""), "POST", "/api/jobs", encode(JobRequest{Action: "design_covers", IDs: ids}), nil, "")
	var result map[string]string
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	job := waitJob(t, s, result["id"])
	if job.Action != "design_covers" || job.Status != "completed" || job.Total != 2 || job.Done != 2 || job.Skipped != 1 || calls.Load() != 1 {
		t.Fatal("series dedup or preserve failed", job, calls.Load())
	}
	if raw, _ := os.ReadFile(filepath.Join(s.root, "beta/poster.jpg")); !bytes.Equal(raw, old) {
		t.Fatal("existing poster changed without overwrite")
	}
	job = waitJob(t, s, start(t, s, "design_covers", ids, true))
	if job.Total != 2 || job.Skipped != 0 || job.Failed != 0 || calls.Load() != 3 {
		t.Fatal("overwrite did not regenerate each series", job, calls.Load())
	}
	for _, path := range []string{"alpha/poster.jpg", "beta/poster.jpg", "beta/season02-poster.jpg"} {
		raw, err := os.ReadFile(filepath.Join(s.root, path))
		cfg, _, decodeErr := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil || decodeErr != nil || cfg.Width != 1024 || cfg.Height != 1536 {
			t.Fatal(path, cfg, err, decodeErr)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(s.root, "beta/fanart.jpg"))
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width != 1280 || cfg.Height != 720 {
		t.Fatal("landscape reference not retained", cfg, err)
	}
	for _, path := range []string{"alpha/S01E01-thumb.jpg", "alpha/S01E02-thumb.jpg", "beta/S02E01-thumb.jpg", "movies/movie-poster.jpg"} {
		if s.videoAvailable(path) {
			t.Fatal("batch wrote non-series artwork", path)
		}
	}
}

func TestBatchDesignValidation(t *testing.T) {
	s := testStore(t)
	put(t, s, "show/S01E01.mp4", "source")
	ids := scan(t, s, "show", "episode")
	if _, err := s.startJob(JobRequest{Action: "design_covers", IDs: ids}); err == nil {
		t.Fatal("missing image settings accepted")
	}
	configureImages(t, s)
	put(t, s, "movies/movie.mp4", "source")
	movies := scan(t, s, "movies", "movie")
	if _, err := s.startJob(JobRequest{Action: "design_covers", IDs: movies}); err == nil {
		t.Fatal("movies-only request accepted")
	}
	jobs, err := s.listJobs()
	if err != nil || len(jobs) != 0 {
		t.Fatal("invalid request left jobs", jobs, err)
	}
	if err := os.Remove(filepath.Join(s.root, "show/S01E01.mp4")); err != nil {
		t.Fatal(err)
	}
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("unavailable video must fail before paid generation")
		return imageResponse(r, nil), nil
	})
	job := waitJob(t, s, start(t, s, "design_covers", ids, false))
	if job.Failed != 1 {
		t.Fatal(job)
	}
}

func TestBatchDesignRetryAfterRestartRetainsOverwrite(t *testing.T) {
	s, err := newStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s != nil {
			s.close()
		}
	})
	configureImages(t, s)
	ids := []string{}
	old := previewPNG(t, 640, 360)
	for _, name := range []string{"alpha", "beta"} {
		put(t, s, name+"/S01E01.mp4", "source")
		ids = append(ids, scan(t, s, name, "episode")...)
		put(t, s, name+"/poster.jpg", string(old))
	}
	portrait := previewPNG(t, 1024, 1536)
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return mockResponse(r, 400, "application/json", []byte(`{}`)), nil
		}
		return imageResponse(r, portrait), nil
	})
	jobID := start(t, s, "design_covers", ids, true)
	job := waitJob(t, s, jobID)
	if job.Failed != 1 || job.Done != 2 || job.Status != "partial" {
		t.Fatal("failure stopped other series", job)
	}
	root, data := s.root, s.data
	s.close()
	s, err = newStore(root, data)
	if err != nil {
		t.Fatal(err)
	}
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return imageResponse(r, portrait), nil
	})
	result, err := s.retryCoverJob(jobID)
	if err != nil {
		t.Fatal(err)
	}
	job = waitJob(t, s, result["id"])
	if job.Action != "design_covers" || job.Total != 1 || job.Failed != 0 || job.Skipped != 0 || calls.Load() != 3 {
		t.Fatal("retry lost generation/overwrite or repeated success", job, calls.Load())
	}
}

func TestBatchDesignConcurrencyCancelAndDuplicate(t *testing.T) {
	s := testStore(t)
	configureImages(t, s)
	c := s.getConfig()
	c.Concurrency = 2
	if _, err := s.saveSettings(SettingsRequest{Config: c}); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	old := previewPNG(t, 640, 360)
	for i := range 3 {
		name := fmt.Sprintf("show%d", i)
		put(t, s, name+"/S01E01.mp4", "source")
		ids = append(ids, scan(t, s, name, "episode")...)
		put(t, s, name+"/poster.jpg", string(old))
	}
	entered := make(chan struct{}, 4)
	var active, peak atomic.Int32
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p; p = peak.Load() {
			if peak.CompareAndSwap(p, n) {
				break
			}
		}
		entered <- struct{}{}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	first := start(t, s, "design_covers", ids[:2], true)
	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("design worker did not start")
		}
	}
	second := start(t, s, "design_covers", ids[2:], true)
	if duplicate := start(t, s, "design_covers", ids, true); duplicate != "" {
		t.Fatal("active series duplicated", duplicate)
	}
	// Automatic and AI design jobs share the same ownership exclusion.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	duplicate, _, err := queueCoverTasks(tx, []CoverTask{{VideoID: ids[0], Owner: "show:show0", Scope: "series", Path: "show0"}})
	tx.Rollback()
	if err != nil || duplicate != "" {
		t.Fatal("auto/design duplicate", duplicate, err)
	}
	if _, err := s.designCover(context.Background(), DesignCoverRequest{ID: ids[0]}); err == nil {
		t.Fatal("manual design exceeded shared cap")
	}
	// Poster work does not block unrelated NFO jobs.
	waitJob(t, s, start(t, s, "save", ids[:1], false))
	for _, id := range []string{second, first} {
		if err := s.cancelJob(id); err != nil {
			t.Fatal(err)
		}
		if job := waitJob(t, s, id); job.Status != "cancelled" {
			t.Fatal(job)
		}
	}
	if active.Load() != 0 || peak.Load() != 2 {
		t.Fatal(active.Load(), peak.Load())
	}
	for i := range 3 {
		raw, _ := os.ReadFile(filepath.Join(s.root, fmt.Sprintf("show%d/poster.jpg", i)))
		if !bytes.Equal(raw, old) {
			t.Fatal("cancel replaced original artwork")
		}
	}
}
