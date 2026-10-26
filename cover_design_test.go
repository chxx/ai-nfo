package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func configureImages(t *testing.T, s *Store) {
	t.Helper()
	c := s.getConfig()
	c.ImageBaseURL, c.ImageModel, c.ImageAPIKey = "https://images.example/v1", "test-image-model", "image-secret"
	c.APIKey, c.RetryCount = "text-secret", 0
	if _, err := s.saveSettings(SettingsRequest{Config: c}); err != nil {
		t.Fatal(err)
	}
}

func imageResponse(r *http.Request, raw []byte) *http.Response {
	return mockResponse(r, 200, "application/json", []byte(encode(map[string]any{"data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(raw)}}})))
}

func TestDesignPortraitFromReferenceAndRestoreArtwork(t *testing.T) {
	s := testStore(t)
	configureImages(t, s)
	put(t, s, "show/S01E01.mp4", "")
	id := scan(t, s, "show", "episode")[0]
	draft(t, s, id)
	ref, err := s.cacheCover("show:show", CoverCandidate{Title: "横图", Kind: "snapshot"}, previewPNG(t, 1280, 720))
	if err != nil {
		t.Fatal(err)
	}
	portrait := previewPNG(t, 1024, 1536)
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Path != "/v1/images/edits" || r.Header.Get("Authorization") != "Bearer image-secret" {
			t.Error("wrong image endpoint/key", r.URL.Path)
		}
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			t.Error(err)
			return nil, err
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("model") != "test-image-model" || r.FormValue("size") != "1024x1536" || r.FormValue("n") != "1" || r.FormValue("response_format") != "" || !strings.Contains(r.FormValue("prompt"), "总览") {
			t.Error("wrong image request")
		}
		file, header, err := r.FormFile("image[]")
		if err != nil {
			t.Error(err)
			return nil, err
		}
		defer file.Close()
		raw, _ := io.ReadAll(file)
		cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil || format != "jpeg" || cfg.Width != 1280 || cfg.Height != 720 || header.Header.Get("Content-Type") != "image/jpeg" {
			t.Error("reference lost")
		}
		response := imageResponse(r, portrait)
		// An ordinary text response is capped at 2 MB; image responses need more.
		response.Body = io.NopCloser(strings.NewReader(encode(map[string]any{"padding": strings.Repeat("x", 2100000), "data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(portrait)}}})))
		return response, nil
	})
	app := newWebApp(s, "")
	w := request(app, "POST", "/api/covers/design", encode(DesignCoverRequest{ID: id, CandidateID: ref.ID}), nil, "")
	var candidate CoverCandidate
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &candidate) != nil || candidate.Kind != "generated" || candidate.Width != 1024 || candidate.Height != 1536 || calls.Load() != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if s.videoAvailable("show/poster.jpg") {
		t.Fatal("design automatically saved sidecar")
	}
	if _, err = s.saveCover(SaveCoverRequest{ID: id, Scope: "series", CandidateID: candidate.ID}); err != nil {
		t.Fatal(err)
	}
	check := func(path string, wantW, wantH int) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(s.root, path))
		if err != nil {
			t.Fatal(err)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil || cfg.Width != wantW || cfg.Height != wantH {
			t.Fatal(path, cfg, err)
		}
	}
	for _, path := range []string{"show/poster.jpg", "show/season01-poster.jpg"} {
		check(path, 1024, 1536)
	}
	for _, path := range []string{"show/fanart.jpg", "show/season01-fanart.jpg"} {
		check(path, 1280, 720)
	}
	for _, path := range []string{"show/poster.jpg", "show/fanart.jpg", "show/season01-poster.jpg", "show/season01-fanart.jpg"} {
		if err = os.Remove(filepath.Join(s.root, path)); err != nil {
			t.Fatal(err)
		}
	}
	v, _ := s.getVideo(id)
	if err = s.restoreCoverAssets(v); err != nil {
		t.Fatal(err)
	}
	check("show/poster.jpg", 1024, 1536)
	check("show/fanart.jpg", 1280, 720)
	check("show/season01-fanart.jpg", 1280, 720)
	if _, err = s.clearRecords(ClearRecordsRequest{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM cover_assets WHERE owner='show-fanart:show'").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("orphan fanart", remaining, err)
	}
}

func TestAutomaticSeriesPortraitDesign(t *testing.T) {
	for _, embedded := range []bool{true, false} {
		t.Run(map[bool]string{true: "embedded", false: "snapshot"}[embedded], func(t *testing.T) {
			s := testStore(t)
			configureImages(t, s)
			probe := `{"streams":[]}`
			if embedded {
				probe = `{"streams":[{"index":1,"codec_type":"video","codec_name":"png","disposition":{"attached_pic":1}}]}`
			}
			fakeMediaTools(t, probe, previewPNG(t, 1280, 720))
			put(t, s, "show/S01E01.mp4", "")
			id := scan(t, s, "show", "episode")[0]
			var calls atomic.Int32
			portrait := previewPNG(t, 600, 900)
			s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.URL.Path != "/v1/images/edits" {
					t.Error("unexpected search", r.URL.Path)
				}
				return imageResponse(r, portrait), nil
			})
			job := waitJob(t, s, start(t, s, "covers", []string{id}, false))
			if job.Failed != 0 || calls.Load() != 1 {
				t.Fatal(job, calls.Load())
			}
			b, err := os.ReadFile(filepath.Join(s.root, "show/poster.jpg"))
			if err != nil {
				t.Fatal(err)
			}
			cfg, _, _ := image.DecodeConfig(bytes.NewReader(b))
			if cfg.Width != 600 || cfg.Height != 900 {
				t.Fatal(cfg)
			}
		})
	}
}

func TestDesignWithoutReferenceAndInvalidResponses(t *testing.T) {
	for _, mode := range []string{"text_only", "landscape", "wrong_ratio", "invalid_json", "invalid_base64", "url_only", "http_error", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			configureImages(t, s)
			fakeMediaTools(t, `{"streams":[]}`, nil)
			put(t, s, "show/S01E01.mp4", "")
			id := scan(t, s, "show", "episode")[0]
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			portrait := previewPNG(t, 600, 900)
			landscape := previewPNG(t, 900, 600)
			wrongRatio := previewPNG(t, 600, 1000)
			s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/v1/images/generations" {
					t.Error("should use text generation")
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["size"] != "1024x1536" {
					t.Error("invalid JSON body", err)
				}
				switch mode {
				case "landscape":
					return imageResponse(r, landscape), nil
				case "wrong_ratio":
					return imageResponse(r, wrongRatio), nil
				case "invalid_json":
					return mockResponse(r, 200, "application/json", []byte(`{`)), nil
				case "invalid_base64":
					return mockResponse(r, 200, "application/json", []byte(`{"data":[{"b64_json":"!"}]}`)), nil
				case "url_only":
					return mockResponse(r, 200, "application/json", []byte(`{"data":[{"url":"http://127.0.0.1/private"}]}`)), nil
				case "http_error":
					return mockResponse(r, 401, "application/json", []byte("image-secret")), nil
				case "cancelled":
					cancel()
				}
				return imageResponse(r, portrait), nil
			})
			candidate, err := s.designCover(ctx, DesignCoverRequest{ID: id})
			if mode == "text_only" {
				if err != nil || candidate.ReferenceID != "" {
					t.Fatal(candidate, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal("invalid response accepted/leaked", mode, err)
			}
			if s.videoAvailable("show/poster.jpg") {
				t.Fatal("design wrote poster")
			}
		})
	}
}

func TestDesignSettingsAndOwnership(t *testing.T) {
	s := testStore(t)
	configureImages(t, s)
	c := s.getConfig()
	c.ImageAPIKey = ""
	public, err := s.saveSettings(SettingsRequest{Config: c})
	if err != nil || s.getConfig().ImageAPIKey != "image-secret" || strings.Contains(encode(public), "secret") || public["has_image_key"] != true {
		t.Fatal("image key persistence/leak", err)
	}
	for _, base := range []string{"file:///tmp/image", "https://user:secret@example.com", "https://example.com?key=secret"} {
		bad := c
		bad.ImageBaseURL = base
		if _, err := s.saveSettings(SettingsRequest{Config: bad}); err == nil {
			t.Fatal("invalid image URL accepted")
		}
	}
	for _, path := range []string{"show/S01E01.mp4", "other/S01E01.mp4"} {
		put(t, s, path, "")
	}
	id := scan(t, s, "show", "episode")[0]
	scan(t, s, "other", "episode")
	ref, err := s.cacheCover("show:other", CoverCandidate{}, previewPNG(t, 1280, 720))
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return mockResponse(r, 500, "text/plain", nil), nil
	})
	if _, err = s.designCover(context.Background(), DesignCoverRequest{ID: id, CandidateID: ref.ID}); err == nil || calls.Load() != 0 {
		t.Fatal("foreign reference sent")
	}
	if _, err = s.saveSettings(SettingsRequest{Config: c, ClearImageKey: true}); err != nil || s.getConfig().ImageAPIKey != "" {
		t.Fatal("key not cleared", err)
	}
	c.ImageModel = ""
	if _, err = s.saveSettings(SettingsRequest{Config: c}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.designCover(context.Background(), DesignCoverRequest{ID: id}); err == nil || calls.Load() != 0 {
		t.Fatal("missing model called API")
	}
	posters := []CoverCandidate{{ID: "huge-wide", Width: 8000, Height: 2000}, {ID: "portrait", Width: 600, Height: 900}}
	if got := bestAutoCover(posters, false, "series", "episode"); got == nil || got.ID != "portrait" {
		t.Fatal("landscape outranked portrait", got)
	}
}
