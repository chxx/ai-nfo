package main

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMetadataAndNativeSearchShareOneRequestSlot(t *testing.T) {
	s := testStore(t)
	configure(t, s, "https://ai.example", "compatible")
	entered := make(chan string, 4)
	resume := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		entered <- r.URL.Path
		if r.URL.Path == "/responses" {
			// Headers arrive immediately; the slot must stay occupied while the body is pending.
			reader, writer := io.Pipe()
			go func() {
				select {
				case <-resume:
					_, _ = io.WriteString(writer, nativeSearchFixture)
				case <-r.Context().Done():
				}
				writer.Close()
			}()
			response := mockResponse(r, 200, "application/json", nil)
			response.Body = reader
			return response, nil
		}
		return mockResponse(r, 200, "application/json", []byte(`{"choices":[{"message":{"content":"{\"title\":\"标题\"}"}}]}`)), nil
	})
	searchDone := make(chan error, 1)
	go func() { _, err := s.modelSearchPages(context.Background(), s.getConfig(), "course"); searchDone <- err }()
	select {
	case path := <-entered:
		if path != "/responses" {
			t.Fatal(path)
		}
	case <-time.After(time.Second):
		t.Fatal("search did not start")
	}
	// A cancelled waiter must leave without sending a request or consuming the slot.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var cancelledOutput Metadata
	if err := s.aiJSON(ctx, instruction, nil, &cancelledOutput); err == nil || calls.Load() != 1 {
		t.Fatal("cancelled waiter sent model request", err, calls.Load())
	}
	textDone := make(chan error, 1)
	go func() { var output Metadata; textDone <- s.aiJSON(context.Background(), instruction, nil, &output) }()
	select {
	case path := <-entered:
		t.Fatal("overlapping model request", path)
	case <-time.After(60 * time.Millisecond):
	}
	unblock()
	for _, done := range []chan error{searchDone, textDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("slot not released")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected model calls", calls.Load())
	}
	s.cancel()
	var output Metadata
	if err := s.aiJSON(context.Background(), instruction, nil, &output); err == nil || calls.Load() != 2 {
		t.Fatal("request after shutdown")
	}
}

func TestConcurrencySettingsPersistAndResize(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	s, err := newStore(root, data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if s.getConfig().Concurrency != 1 {
		t.Fatal("default must be one")
	}
	c := s.getConfig()
	for _, invalid := range []int{-1, 9} {
		c.Concurrency = invalid
		if _, err := s.saveSettings(SettingsRequest{Config: c}); err == nil {
			t.Fatal("accepted invalid limit", invalid)
		}
	}
	app := newWebApp(s, "")
	if w := request(app, "POST", "/api/settings", `{"provider":"compatible","concurrency":1.5}`, nil, ""); w.Code != 400 {
		t.Fatal("fractional concurrency accepted", w.Code)
	}
	c.Concurrency = 4
	result, err := s.saveSettings(SettingsRequest{Config: c})
	if err != nil || result["concurrency"] != 4 {
		t.Fatal(result, err)
	}
	// Simulate manual operations in flight while the settings are changed.
	for _, limiter := range []*workLimiter{s.aiLimiter, s.coverLimiter} {
		releases := []func(){}
		for range 4 {
			release, err := limiter.acquire(context.Background(), s.ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			releases = append(releases, release)
		}
		if release, err := limiter.acquire(context.Background(), s.ctx, false); err == nil {
			release()
			t.Fatal("configured limit exceeded")
		}
		limiter.setLimit(1)
		for _, release := range releases[:3] {
			release()
		}
		if release, err := limiter.acquire(context.Background(), s.ctx, false); err == nil {
			release()
			t.Fatal("reduced limit exceeded")
		}
		releases[3]()
		release, err := limiter.acquire(context.Background(), s.ctx, false)
		if err != nil {
			t.Fatal("slot leaked", err)
		}
		release()
	}
	c.Concurrency = 3
	if _, err := s.saveSettings(SettingsRequest{Config: c}); err != nil {
		t.Fatal(err)
	}
	reopened, err := newStore(root, data)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if reopened.getConfig().Concurrency != 3 || reopened.aiLimiter.limit != 3 || reopened.coverLimiter.limit != 3 {
		t.Fatal("limit not restored on restart")
	}
}
