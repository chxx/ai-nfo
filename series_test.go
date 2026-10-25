package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectorySeriesScan(t *testing.T) {
	s := testStore(t)
	paths := []string{"Hello Carrie/01 Alphabet/01 A.mp4", "Hello Carrie/01 Alphabet/02 B.mp4", "Hello Carrie/02 Phonics/01 Words/01 C.mp4", "Other/01 Alphabet/01 A.mp4", "Seasoned Course/Season 01/S01E01.mp4", "Seasoned Course/Season 02/S02E01.mp4", "root.mp4"}
	for _, p := range paths {
		put(t, s, p, "")
	}
	app := newWebApp(s, "")
	w := request(app, "POST", "/api/scan", `{"kind":"episode","recursive":true,"group_by_directory":true}`, nil, "")
	var result ScanResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Count != len(paths) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, p := range paths {
		v, err := s.getVideo(identifier(p))
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(p)
		wantShowPath := dir
		if strings.HasPrefix(p, "Seasoned Course/") {
			wantShowPath = "Seasoned Course"
		}
		if v.ShowPath == nil || *v.ShowPath != wantShowPath {
			t.Fatalf("wrong series for %s: %+v", p, v)
		}
		want := filepath.Base(wantShowPath)
		if wantShowPath == "." {
			want = filepath.Base(s.root)
		}
		if v.Show.Title != want {
			t.Fatal("wrong default title", v.Show.Title, want)
		}
		target, err := coverTargetPath(v, "series")
		if err != nil || target.Path != filepath.Join(wantShowPath, "poster.jpg") {
			t.Fatal(target, err)
		}
	}
	// Identical directory names under different branches must not share metadata.
	_, err := s.db.Exec("UPDATE shows SET metadata=? WHERE path=?", `{"title":"Custom title","plot":"Only this branch"}`, "Hello Carrie/01 Alphabet")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.getVideo(identifier(paths[3]))
	if err != nil || other.Show.Plot != "" {
		t.Fatal("mixed branches", other, err)
	}
	// A nonrecursive rescan must not reassign nested videos.
	result, err = s.scan(ScanRequest{Kind: "episode", GroupByDirectory: true})
	if err != nil || result.Count != 1 || result.Reclassified != 0 {
		t.Fatal(result, err)
	}
	// Explicit whole-course mode still groups season/chapter directories together.
	result, err = s.scan(ScanRequest{Path: "Hello Carrie", Kind: "episode", Recursive: true})
	if err != nil || result.Count != 3 || result.Reclassified != 3 {
		t.Fatal(result, err)
	}
	w = request(app, "GET", "/series.js", "", nil, "")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Fatal("series script unavailable", w.Code, w.Header())
	}
}

func TestSeasonDirectoryLayout(t *testing.T) {
	s := testStore(t)
	paths := []string{
		"Course/Season 01/Unit 1/S01E01-intro.mp4",
		"Course/S02/02-second.mp4",
		"Course/第3季/03-third.mp4",
	}
	for _, path := range paths {
		put(t, s, path, "")
	}
	result, err := s.scan(ScanRequest{Path: "Course", Kind: "episode", Recursive: true, GroupByDirectory: true})
	if err != nil || result.Count != len(paths) {
		t.Fatal(result, err)
	}
	wantSeasons := map[string]int{paths[0]: 1, paths[1]: 2, paths[2]: 3}
	for _, path := range paths {
		v, err := s.getVideo(identifier(path))
		if err != nil || v.ShowPath == nil || *v.ShowPath != "Course" || v.Season == nil || *v.Season != wantSeasons[path] {
			t.Fatalf("wrong season grouping for %s: %+v %v", path, v, err)
		}
	}
	draft(t, s, identifier(paths[0]))
	files, err := s.preview(identifier(paths[0]))
	if err != nil || len(files) != 2 || files[0].Path != "Course/tvshow.nfo" || files[1].Path != "Course/Season 01/Unit 1/S01E01-intro.nfo" {
		t.Fatalf("wrong Kodi NFO layout: %+v %v", files, err)
	}
	if result, err = s.scan(ScanRequest{Path: "Course/Season 01", Kind: "episode", Recursive: true}); err != nil || result.Count != 1 {
		t.Fatal(result, err)
	}
	if v, err := s.getVideo(identifier(paths[0])); err != nil || v.ShowPath == nil || *v.ShowPath != "Course" {
		t.Fatalf("scanning one season split the show: %+v %v", v, err)
	}
	for _, name := range []string{"Season 01", "S02", "第3季", "01 Alphabet"} {
		if _, ok := seasonDirectory(name); ok == (name == "01 Alphabet") {
			t.Fatalf("season directory detection for %q: %v", name, ok)
		}
	}
}

func TestRepairLegacySeasonRecords(t *testing.T) {
	s := testStore(t)
	path := "Course/Season 01/S01E01.mp4"
	put(t, s, path, "")
	id := scan(t, s, "Course/Season 01", "episode")[0]
	if _, err := s.db.Exec("UPDATE videos SET show_path=? WHERE id=?", "Course/Season 01", id); err != nil {
		t.Fatal(err)
	}
	put(t, s, "Course/Season 01/tvshow.nfo", "<tvshow><title>旧资料</title></tvshow>")
	put(t, s, "Course/Season 01/poster.jpg", "poster")
	if err := s.repairSeasonRecords(); err != nil {
		t.Fatal(err)
	}
	v, err := s.getVideo(id)
	if err != nil || v.ShowPath == nil || *v.ShowPath != "Course" {
		t.Fatalf("legacy record was not moved: %+v %v", v, err)
	}
	if _, err := os.Stat(filepath.Join(s.root, "Course", "tvshow.nfo")); err != nil {
		t.Fatal("tvshow.nfo was not moved", err)
	}
	if _, err := os.Stat(filepath.Join(s.root, "Course", "poster.jpg")); err != nil {
		t.Fatal("poster was not moved", err)
	}
}
