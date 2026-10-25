package main

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func previewPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(m, m.Bounds(), &image.Uniform{C: color.RGBA{35, 124, 107, 255}}, image.Point{}, draw.Src)
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestListCoverPreviewsAndThumbnailSize(t *testing.T) {
	s := testStore(t)
	put(t, s, "course/S01E01.mp4", "")
	put(t, s, "course/S01E02.mp4", "")
	put(t, s, "movie.mp4", "")
	scan(t, s, "", "movie")
	scan(t, s, "course", "episode")
	before, err := s.listVideos()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range before {
		if len(v.Covers) != 0 {
			t.Fatal("invented cover", v.Covers)
		}
	}
	landscape, portrait := previewPNG(t, 1280, 720), previewPNG(t, 900, 1200)
	put(t, s, "course/S01E01-thumb.jpg", string(landscape))
	put(t, s, "course/poster.jpg", string(portrait))
	put(t, s, "movie-poster.jpg", string(landscape))
	videos, err := s.listVideos()
	if err != nil {
		t.Fatal(err)
	}
	app := newWebApp(s, "")
	var first CoverPreview
	for _, v := range videos {
		want := 1
		if v.Filename == "S01E01.mp4" {
			want = 2
			first = v.Covers[0]
		}
		if len(v.Covers) != want {
			t.Fatal(v.Path, v.Covers)
		}
		for _, c := range v.Covers {
			w := request(app, "GET", c.ThumbnailURL, "", nil, "")
			cfg, format, e := image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
			if w.Code != 200 || e != nil || format != "jpeg" || w.Header().Get("Cache-Control") != "private, max-age=60" {
				t.Fatal(w.Code, format, e)
			}
			width, height := 320, 180
			if c.Scope == "series" {
				width, height = 180, 240
			}
			if cfg.Width != width || cfg.Height != height {
				t.Fatal("thumbnail distorted", cfg)
			}
			w = request(app, "GET", c.URL, "", nil, "")
			cfg, _, e = image.DecodeConfig(bytes.NewReader(w.Body.Bytes()))
			width, height = 1280, 720
			if c.Scope == "series" {
				width, height = 900, 1200
			}
			if w.Code != 200 || e != nil || cfg.Width != width || cfg.Height != height {
				t.Fatal("full image changed size", cfg, e)
			}
		}
	}
	original, err := os.ReadFile(filepath.Join(s.root, first.Path))
	if err != nil || !bytes.Equal(original, landscape) {
		t.Fatal("preview changed saved cover", err)
	}
	stamp := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(s.root, first.Path), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	v, _ := s.getVideo(identifier("course/S01E01.mp4"))
	if updated := s.coverPreviews(v); updated[0].URL != first.URL {
		t.Fatal("database cover version changed with sidecar mtime")
	}
	protected := newWebApp(s, "password123")
	if w := request(protected, "GET", first.ThumbnailURL, "", nil, ""); w.Code != 401 {
		t.Fatal("thumbnail bypasses authentication")
	}
}

func TestCoverPreviewMissingSourceAndUnsafeSidecar(t *testing.T) {
	s := testStore(t)
	put(t, s, "video.mp4", "")
	id := scan(t, s, "", "movie")[0]
	put(t, s, "video-poster.jpg", string(previewPNG(t, 1280, 720)))
	os.Remove(filepath.Join(s.root, "video.mp4"))
	v, err := s.getVideo(id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Exists || len(s.coverPreviews(v)) != 1 {
		t.Fatal("saved cover unavailable without source")
	}
	if _, err := s.coverImageSized(id, "video", "", true); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(s.root, "video-poster.jpg"))
	outside := filepath.Join(t.TempDir(), "private.jpg")
	if err := os.WriteFile(outside, previewPNG(t, 1280, 720), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.root, "video-poster.jpg")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM cover_assets WHERE owner=?", "video:video.mp4"); err != nil {
		t.Fatal(err)
	}
	if len(s.coverPreviews(v)) != 0 {
		t.Fatal("listed sidecar outside media root")
	}
	if _, err := s.coverImageSized(id, "video", "", true); err == nil {
		t.Fatal("read sidecar outside media root")
	}
}
