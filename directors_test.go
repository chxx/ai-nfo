package main

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDirectorsEditPreviewAndWrite(t *testing.T) {
	for _, kind := range []string{"movie", "episode"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			put(t, s, "course/S01E01.mp4", "video")
			id := scan(t, s, "course", kind)[0]
			v, _ := s.getVideo(id)
			body := EditRequest{ID: id, Season: v.Season, Episode: v.Episode,
				Metadata: &Metadata{Title: "标题", Directors: []string{" A & <B> ", "", "A & <B>", "导演乙"}},
				Show:     &Metadata{Title: "系列", Directors: []string{"系列导演"}}}
			app := newWebApp(s, "")
			w := request(app, "POST", "/api/edit", encode(body), nil, "")
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var saved Video
			if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
				t.Fatal(err)
			}
			want := []string{"A & <B>", "导演乙"}
			if !reflect.DeepEqual(saved.Metadata.Directors, want) {
				t.Fatal(saved.Metadata)
			}
			files, err := s.preview(id)
			if err != nil {
				t.Fatal(err)
			}
			job := waitJob(t, s, start(t, s, "save", []string{id}, false))
			if job.Failed != 0 {
				t.Fatal(job)
			}
			for _, f := range files {
				b, err := os.ReadFile(filepath.Join(s.root, f.Path))
				if err != nil || string(b) != f.XML {
					t.Fatal("preview differs from saved NFO", f.Path, err)
				}
				var doc struct {
					Directors []string `xml:"director"`
				}
				if err := xml.Unmarshal(b, &doc); err != nil {
					t.Fatal(err)
				}
				expected := want
				if filepath.Base(f.Path) == "tvshow.nfo" {
					expected = []string{"系列导演"}
				}
				if !reflect.DeepEqual(doc.Directors, expected) {
					t.Fatal("wrong director or XML escaping", f.Path, doc.Directors)
				}
			}
			body.Metadata.Directors = []string{}
			body.Show.Directors = []string{}
			if _, err := s.edit(body); err != nil {
				t.Fatal(err)
			}
			job = waitJob(t, s, start(t, s, "save", []string{id}, true))
			if job.Failed != 0 {
				t.Fatal(job)
			}
			for _, f := range files {
				b, err := os.ReadFile(filepath.Join(s.root, f.Path))
				if err != nil || strings.Contains(string(b), "<director") {
					t.Fatal("cleared director still emitted", f.Path, err)
				}
			}
		})
	}
}

func TestEmbeddedDirectorIsExplicitAndBounded(t *testing.T) {
	p := embeddedProbe{}
	p.Format.Tags = map[string]string{"DIRECTOR": "明确导演", "artist": "频道", "author": "上传者"}
	if m := p.metadata(); m["director"] != "明确导演" {
		t.Fatal(m)
	}
	delete(p.Format.Tags, "DIRECTOR")
	if m := p.metadata(); m["director"] != "" {
		t.Fatal("inferred director from unrelated role", m)
	}
	m := Metadata{Title: "标题"}
	for i := range 30 {
		m.Directors = append(m.Directors, strings.Repeat(string(rune('A'+i)), 100))
	}
	if err := m.validate(); err != nil || len(m.Directors) != 20 || len([]rune(m.Directors[0])) != 80 {
		t.Fatal("missing input limits", err, m.Directors)
	}
}
