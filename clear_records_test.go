package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestClearSelectedRecordsAndSharedSeries(t *testing.T) {
	s := testStore(t)
	for _, path := range []string{"course/01.mp4", "course/02.mp4"} {
		put(t, s, path, "video")
	}
	scan(t, s, "course", "episode")
	draft(t, s, identifier("course/01.mp4"))
	draft(t, s, identifier("course/02.mp4"))
	files := map[string]string{"course/01.mp4": "video", "course/01.nfo": "nfo", "course/01.srt": "subtitle", "course/01-thumb.jpg": "cover", "course/poster.jpg": "poster", "course/tvshow.nfo": "show"}
	for path, content := range files {
		put(t, s, path, content)
	}
	for _, owner := range []string{"video:course/01.mp4", "video:course/02.mp4", "show:course"} {
		if _, err := s.db.Exec("INSERT INTO cover_candidates VALUES(?,?,?, ?,?)", owner, owner, []byte("image"), "{}", now()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("INSERT INTO cover_selections VALUES(?,?,?,?)", owner, "poster.jpg", "{}", now()); err != nil {
			t.Fatal(err)
		}
	}
	app := newWebApp(s, "")
	body := ClearRecordsRequest{IDs: []string{identifier("course/01.mp4"), identifier("course/01.mp4"), "unknown"}}
	w := request(app, "POST", "/api/records/clear", encode(body), nil, "")
	var result CleanupResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Removed != 1 || result.Skipped != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := s.getVideo(identifier("course/01.mp4")); err == nil {
		t.Fatal("selected video remains")
	}
	v, err := s.getVideo(identifier("course/02.mp4"))
	if err != nil || v.Metadata == nil || v.Show == nil {
		t.Fatal("unselected draft or series lost", err)
	}
	for _, table := range []string{"cover_candidates", "cover_selections"} {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 2 {
			t.Fatal("shared cover lost", table, count, err)
		}
	}
	if _, err := s.clearRecords(ClearRecordsRequest{IDs: []string{identifier("course/02.mp4")}}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"videos", "shows", "cover_candidates", "cover_selections"} {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("orphan remains", table, count, err)
		}
	}
	for path, content := range files {
		b, err := os.ReadFile(filepath.Join(s.root, path))
		if err != nil || string(b) != content {
			t.Fatal("media file changed", path, err)
		}
	}
	scan(t, s, "course", "episode")
	v, err = s.getVideo(identifier("course/01.mp4"))
	if err != nil || v.Metadata != nil || !v.HasNFO {
		t.Fatal("rescan failed", v, err)
	}
}

func TestClearAllRecordsOfflineAndRollback(t *testing.T) {
	s := testStore(t)
	put(t, s, "video.mp4", "video")
	id := scan(t, s, "", "movie")[0]
	draft(t, s, id)
	if _, err := s.db.Exec("INSERT INTO jobs(id,status) VALUES('history','completed')"); err != nil {
		t.Fatal(err)
	}
	config := Config{Provider: "compatible", BaseURL: "https://example.com", Model: "test", APIKey: "test-only-key"}
	s.config = config
	settingsPath := filepath.Join(s.data, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(encode(config)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("CREATE TRIGGER prevent_clear BEFORE DELETE ON shows BEGIN SELECT RAISE(ABORT, 'test failure'); END; INSERT INTO shows(path,metadata) VALUES('orphan','{}')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.clearRecords(ClearRecordsRequest{All: true}); err == nil {
		t.Fatal("expected transactional failure")
	}
	if _, err := s.getVideo(id); err != nil {
		t.Fatal("partial deletion after rollback", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER prevent_clear"); err != nil {
		t.Fatal(err)
	}
	offline := s.root + "-offline"
	if err := os.Rename(s.root, offline); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(offline, s.root) })
	result, err := s.clearRecords(ClearRecordsRequest{All: true})
	if err != nil || result.Removed != 1 {
		t.Fatal(result, err)
	}
	for _, table := range []string{"videos", "shows", "cover_candidates", "cover_selections"} {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
	var history int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE id='history'").Scan(&history); err != nil || history != 1 {
		t.Fatal("history changed", err)
	}
	b, err := os.ReadFile(settingsPath)
	if err != nil || string(b) != encode(config) || s.getConfig() != config {
		t.Fatal("settings changed", err)
	}
	if b, err := os.ReadFile(filepath.Join(offline, "video.mp4")); err != nil || string(b) != "video" {
		t.Fatal("offline media changed", err)
	}
	result, err = s.clearRecords(ClearRecordsRequest{All: true})
	if err != nil || result.Removed != 0 {
		t.Fatal("clear empty library", result, err)
	}
}

func TestClearRecordsGuards(t *testing.T) {
	s := testStore(t)
	put(t, s, "video.mp4", "video")
	id := scan(t, s, "", "movie")[0]
	app := newWebApp(s, "password123")
	payload := encode(ClearRecordsRequest{All: true})
	if w := request(app, "POST", "/api/records/clear", payload, nil, ""); w.Code != 401 {
		t.Fatal("unauthenticated clear", w.Code)
	}
	login := request(app, "POST", "/api/login", `{"password":"password123"}`, nil, "")
	cookie := login.Result().Cookies()[0]
	if w := request(app, "POST", "/api/records/clear", payload, cookie, "https://foreign.invalid"); w.Code != 403 {
		t.Fatal("cross-site clear", w.Code)
	}
	for _, body := range []ClearRecordsRequest{{}, {All: true, IDs: []string{id}}, {IDs: make([]string, 5001)}} {
		if _, err := s.clearRecords(body); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	for _, status := range []string{"queued", "running", "cancelling"} {
		if _, err := s.db.Exec("INSERT OR REPLACE INTO jobs(id,status) VALUES('busy',?)", status); err != nil {
			t.Fatal(err)
		}
		if _, err := s.clearRecords(ClearRecordsRequest{All: true}); err == nil {
			t.Fatal("cleared during job", status)
		}
	}
	if _, err := s.db.Exec("DELETE FROM jobs"); err != nil {
		t.Fatal(err)
	}
	s.coverMu.Lock()
	_, err := s.clearRecords(ClearRecordsRequest{All: true})
	s.coverMu.Unlock()
	if err == nil {
		t.Fatal("cleared during cover lookup")
	}
	if _, err := s.getVideo(id); err != nil {
		t.Fatal("record lost after rejected clear", err)
	}
}
