package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeMediaTools(t *testing.T, probeJSON string, image []byte) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	for name, body := range map[string][]byte{"probe.json": []byte(probeJSON), "cover.png": image} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, file := range map[string]string{"ffprobe": "probe.json", "ffmpeg": "cover.png"} {
		script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + quote(filepath.Join(dir, name+".args")) + "\nexec /bin/cat " + quote(filepath.Join(dir, file)) + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestEmbeddedMetadataAndEvidence(t *testing.T) {
	s := testStore(t)
	dir := fakeMediaTools(t, `{"format":{"tags":{"TITLE":"真实标题","description":"文件内嵌简介","artist":"作者","secret":"never send"}},"streams":[{"index":0,"codec_type":"video","tags":{"title":"stream fallback","GENRE":"教程"},"disposition":{"attached_pic":0}},{"index":1,"codec_type":"video","tags":{"comment":"cover only"},"disposition":{"attached_pic":1}}]}`, nil)
	put(t, s, "course/video.mp4", "unchanged video")
	put(t, s, "course/video.srt", "外置字幕")
	id := scan(t, s, "course", "episode")[0]
	v, _ := s.getVideo(id)
	e, err := s.evidence(v)
	if err != nil || e.Embedded["title"] != "真实标题" || e.Embedded["genre"] != "教程" || len(e.Sources) != 1 || len(e.Warnings) != 0 {
		t.Fatal(e, err)
	}
	if strings.Contains(encode(e), "never send") || strings.Contains(encode(e), "cover only") {
		t.Fatal("unselected tags leaked")
	}
	args, _ := os.ReadFile(filepath.Join(dir, "ffprobe.args"))
	if !strings.Contains(string(args), "file,pipe") || !strings.Contains(string(args), "-format_whitelist") {
		t.Fatal("probe missing restrictions")
	}
	var p embeddedProbe
	p.Format.Tags = map[string]string{"title": strings.Repeat("长", 500), "comment": strings.Repeat("长", 5000), "encoder": "secret"}
	m := p.metadata()
	if len([]rune(m["title"])) != 300 || len([]rune(m["comment"])) != 4000 || len(m) != 2 {
		t.Fatal("tag limits", len(m))
	}
}

func TestEmbeddedCoversExtractionAndOwnership(t *testing.T) {
	s := testStore(t)
	dir := fakeMediaTools(t, `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264","disposition":{"attached_pic":0}},{"index":2,"codec_type":"video","codec_name":"png","disposition":{"attached_pic":1}}]}`, testCoverPNG(t))
	put(t, s, "course/video.mp4", "unchanged video")
	id := scan(t, s, "course", "episode")[0]
	result, err := s.embeddedCovers(context.Background(), CoverRequest{ID: id, Scope: "video"})
	if err != nil || len(result.Candidates) != 1 || result.Candidates[0].Kind != "embedded" {
		t.Fatal(result, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "ffmpeg.args"))
	if !strings.Contains(string(args), "-map\n0:2\n") || !strings.Contains(string(args), "-c:v\ncopy\n") {
		t.Fatal("wrong stream", string(args))
	}
	c := result.Candidates[0]
	if _, err := s.saveCover(SaveCoverRequest{ID: id, Scope: "series", CandidateID: c.ID}); err == nil {
		t.Fatal("episode candidate used as series without selecting scope")
	}
	saved, err := s.saveCover(SaveCoverRequest{ID: id, Scope: "video", CandidateID: c.ID})
	if err != nil || saved["saved"] != true {
		t.Fatal(saved, err)
	}
	result, err = s.embeddedCovers(context.Background(), CoverRequest{ID: id, Scope: "series"})
	if err != nil || len(result.Candidates) != 1 || !strings.Contains(result.Candidates[0].Reason, "当前单集") {
		t.Fatal(result, err)
	}
	b, err := os.ReadFile(filepath.Join(s.root, "course/video.mp4"))
	if err != nil || string(b) != "unchanged video" {
		t.Fatal("source changed")
	}
	// Attachment filenames are untrusted; extraction must always go to a pipe.
	if err := os.WriteFile(filepath.Join(dir, "probe.json"), []byte(`{"streams":[{"index":3,"codec_type":"attachment","tags":{"mimetype":"image/png","filename":"../../escape.png"}},{"index":4,"codec_type":"attachment","tags":{"mimetype":"application/x-truetype-font"}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = s.embeddedCovers(context.Background(), CoverRequest{ID: id, Scope: "video"})
	if err != nil || len(result.Candidates) != 1 {
		t.Fatal(result, err)
	}
	args, _ = os.ReadFile(filepath.Join(dir, "ffmpeg.args"))
	if !strings.Contains(string(args), "-dump_attachment:3\npipe:1\n") || strings.Contains(string(args), "escape.png") {
		t.Fatal("unsafe attachment output", string(args))
	}
}

func TestEmbeddedFallbackAndLimits(t *testing.T) {
	s := testStore(t)
	put(t, s, "video.mp4", "video")
	id := scan(t, s, "", "movie")[0]
	v, _ := s.getVideo(id)
	dir := fakeMediaTools(t, `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"}]}`, nil)
	result, err := s.embeddedCovers(context.Background(), CoverRequest{ID: id, Scope: "video"})
	if err != nil || len(result.Candidates) != 0 || len(result.Warnings) == 0 {
		t.Fatal(result, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe.json"), []byte(strings.Repeat("x", (512<<10)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := probeEmbedded(context.Background(), filepath.Join(s.root, "video.mp4")); err == nil {
		t.Fatal("oversized probe accepted")
	}
	if err := os.Remove(filepath.Join(dir, "ffprobe")); err != nil {
		t.Fatal(err)
	}
	e, err := s.evidence(v)
	if err != nil || len(e.Warnings) == 0 || e.Filename != "video.mp4" {
		t.Fatal("missing tool blocks fallback", e, err)
	}
	if _, err := s.embeddedCovers(context.Background(), CoverRequest{ID: id, Scope: "video"}); err == nil {
		t.Fatal("missing tool not reported")
	}
	s.coverMu.Lock()
	_, err = s.embeddedCovers(context.Background(), CoverRequest{ID: id, Scope: "video"})
	s.coverMu.Unlock()
	if err == nil {
		t.Fatal("concurrent extraction allowed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runMedia(ctx, "ffmpeg", 100, "-version"); err == nil {
		t.Fatal("cancel ignored")
	}
	app := newWebApp(s, "password123")
	if w := request(app, "POST", "/api/covers/embedded", encode(CoverRequest{ID: id, Scope: "video"}), nil, ""); w.Code != 401 {
		t.Fatal("unauthenticated extraction", w.Code)
	}
	// A blocked child process is bounded by the caller's deadline.
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte("#!/bin/sh\nexec /bin/sleep 5\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := runMedia(ctx, "ffmpeg", 100); err == nil || time.Since(started) > time.Second {
		t.Fatal("timeout not enforced", err)
	}
}
