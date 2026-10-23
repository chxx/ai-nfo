package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func removeTestVideo(t *testing.T, s *Store, path string) {
	t.Helper()
	if err := os.Remove(filepath.Join(s.root, path)); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupPreviewAndConfirmedIDs(t *testing.T) {
	s := testStore(t)
	for _, path := range []string{"missing.mp4", "restored.mp4", "available.mp4", "later.mp4"} {
		put(t, s, path, "video")
	}
	scan(t, s, "", "movie")
	draft(t, s, identifier("missing.mp4"))
	put(t, s, "missing.nfo", "keep NFO")
	put(t, s, "missing.srt", "keep subtitle")
	removeTestVideo(t, s, "missing.mp4")
	removeTestVideo(t, s, "restored.mp4")
	app := newWebApp(s, "")
	w := request(app, "GET", "/api/unavailable", "", nil, "")
	var preview CleanupPreview
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &preview) != nil || preview.Total != 2 || len(preview.Videos) != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := s.getVideo(identifier("missing.mp4")); err != nil {
		t.Fatal("preview removed a record", err)
	}

	put(t, s, "restored.mp4", "restored")
	removeTestVideo(t, s, "later.mp4")
	ids := []string{identifier("missing.mp4"), identifier("restored.mp4"), identifier("available.mp4"), identifier("missing.mp4"), "unknown"}
	w = request(app, "POST", "/api/cleanup", encode(CleanupRequest{IDs: ids}), nil, "")
	var result CleanupResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Removed != 1 || result.Skipped != 3 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := s.getVideo(identifier("missing.mp4")); err == nil {
		t.Fatal("missing record remains")
	}
	for _, name := range []string{"available.mp4", "restored.mp4", "later.mp4"} {
		if _, err := s.getVideo(identifier(name)); err != nil {
			t.Fatal("removed an unconfirmed or restored record", name, err)
		}
	}
	for path, expected := range map[string]string{"missing.nfo": "keep NFO", "missing.srt": "keep subtitle", "restored.mp4": "restored", "available.mp4": "video"} {
		b, err := os.ReadFile(filepath.Join(s.root, path))
		if err != nil || string(b) != expected {
			t.Fatalf("file changed: %s %v", path, err)
		}
	}
	w = request(app, "POST", "/api/cleanup", encode(CleanupRequest{IDs: []string{identifier("missing.mp4")}}), nil, "")
	if w.Code != 200 {
		t.Fatal("repeat cleanup failed", w.Body.String())
	}
}

func TestCleanupSeriesAndSymlinks(t *testing.T) {
	s := testStore(t)
	put(t, s, "course/01-first.mp4", "")
	put(t, s, "course/02-second.mp4", "")
	ids := scan(t, s, "course", "episode")
	for _, id := range ids {
		draft(t, s, id)
	}
	put(t, s, "course/tvshow.nfo", "keep series NFO")
	removeTestVideo(t, s, "course/01-first.mp4")
	result, err := s.cleanupUnavailable(CleanupRequest{IDs: []string{identifier("course/01-first.mp4")}})
	if err != nil || result.Removed != 1 {
		t.Fatal(result, err)
	}
	show, err := s.getShow("course")
	if err != nil || show == nil {
		t.Fatal("shared series deleted", err)
	}

	removeTestVideo(t, s, "course/02-second.mp4")
	external := filepath.Join(t.TempDir(), "external.mp4")
	if err := os.WriteFile(external, []byte("external"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(s.root, "course/02-second.mp4")); err != nil {
		t.Fatal(err)
	}
	result, err = s.cleanupUnavailable(CleanupRequest{IDs: []string{identifier("course/02-second.mp4")}})
	if err != nil || result.Removed != 1 {
		t.Fatal(result, err)
	}
	var count int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM shows WHERE path='course'").Scan(&count); err != nil || count != 0 {
		t.Fatal("empty series remains", err)
	}
	for _, path := range []string{external, filepath.Join(s.root, "course/tvshow.nfo"), filepath.Join(s.root, "course/02-second.mp4")} {
		if _, err = os.Lstat(path); err != nil {
			t.Fatal("cleanup changed filesystem", path, err)
		}
	}
}

func TestCleanupGuards(t *testing.T) {
	s := testStore(t)
	put(t, s, "missing.mp4", "")
	id := scan(t, s, "", "movie")[0]
	removeTestVideo(t, s, "missing.mp4")
	app := newWebApp(s, "password123")
	if w := request(app, "GET", "/api/unavailable", "", nil, ""); w.Code != 401 {
		t.Fatal("unauthenticated preview", w.Code)
	}
	if w := request(app, "POST", "/api/cleanup", encode(CleanupRequest{IDs: []string{id}}), nil, ""); w.Code != 401 {
		t.Fatal("unauthenticated cleanup", w.Code)
	}
	if _, err := s.cleanupUnavailable(CleanupRequest{}); err == nil {
		t.Fatal("empty request accepted")
	}
	if _, err := s.cleanupUnavailable(CleanupRequest{IDs: make([]string, 5001)}); err == nil {
		t.Fatal("oversized request accepted")
	}
	if _, err := s.db.Exec("INSERT INTO jobs(id,status) VALUES('busy','running')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.unavailableVideos(); err == nil {
		t.Fatal("preview allowed during job")
	}
	if _, err := s.cleanupUnavailable(CleanupRequest{IDs: []string{id}}); err == nil {
		t.Fatal("cleanup allowed during job")
	}
	if _, err := s.db.Exec("DELETE FROM jobs WHERE id='busy'"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.unavailableVideos(); err == nil {
		t.Fatal("preview allowed with unavailable root")
	}
	if _, err := s.cleanupUnavailable(CleanupRequest{IDs: []string{id}}); err == nil {
		t.Fatal("cleanup allowed with unavailable root")
	}
	if _, err := s.getVideo(id); err != nil {
		t.Fatal("record lost after rejected cleanup", err)
	}
}
