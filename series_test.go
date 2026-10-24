package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectorySeriesScan(t *testing.T) {
	s := testStore(t)
	paths := []string{"Hello Carrie/01 Alphabet/01 A.mp4", "Hello Carrie/01 Alphabet/02 B.mp4", "Hello Carrie/02 Phonics/01 Words/01 C.mp4", "Other/01 Alphabet/01 A.mp4", "root.mp4"}
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
		if v.ShowPath == nil || *v.ShowPath != dir {
			t.Fatalf("wrong series for %s: %+v", p, v)
		}
		want := filepath.Base(dir)
		if dir == "." {
			want = filepath.Base(s.root)
		}
		if v.Show.Title != want {
			t.Fatal("wrong default title", v.Show.Title, want)
		}
		target, err := coverTargetPath(v, "series")
		if err != nil || target.Path != filepath.Join(dir, "poster.jpg") {
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
