package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func mockResponse(r *http.Request, status int, mime string, b []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {mime}}, Body: io.NopCloser(bytes.NewReader(b)), Request: r}
}
func testCoverPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 96, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 96; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 2), G: uint8(y * 3), B: 90, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func mockPublicPages(t *testing.T, s *Store) {
	t.Helper()
	pngData := testCoverPNG(t)
	s.webClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Error("secret sent to webpage")
		}
		switch r.URL.Path {
		case "/lesson":
			return mockResponse(r, 200, "text/html", []byte(`<title>Alphabet A Sound Song</title><meta property="og:image" content="/poster.png"><meta name="description" content="Hello Carrie: Alphabet A sound lesson">`)), nil
		case "/poster.png":
			return mockResponse(r, 200, "image/png", pngData), nil
		default:
			return mockResponse(r, 404, "text/plain", nil), nil
		}
	})}
}

func TestPublicCoverURLGuards(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1/x", "http://[::1]/x", "http://[::ffff:192.168.1.2]/x", "http://169.254.169.254/latest", "file:///etc/passwd", "https://user:secret@public.example/image", "data:image/svg+xml,test"} {
		if _, err := publicURL(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"10.0.0.1", "100.64.1.1", "172.20.0.1", "192.168.1.2", "0.0.0.0", "224.0.0.1", "::", "fe80::1", "fc00::1", "64:ff9b::a00:1"} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Fatal("private/reserved IP accepted", raw)
		}
	}
	if !publicIP(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public IP rejected")
	}
	client := publicWebClient()
	defer client.CloseIdleConnections()
	r, _ := http.NewRequest("GET", "http://127.0.0.1/image", nil)
	if err := client.CheckRedirect(r, nil); err == nil {
		t.Fatal("private redirect accepted")
	}
	r, _ = http.NewRequest("GET", "http://localhost/image", nil)
	if _, err := client.Do(r); err == nil {
		t.Fatal("private DNS result accepted")
	}
}

func TestPageCoverExtraction(t *testing.T) {
	raw := []byte(`<title>Page title</title><meta property="og:title" content="Hello &amp; Carrie"><meta property="og:image" content="../poster.png?a=1&amp;b=2"><meta name="twitter:image" content="https://courses.example/poster.png?a=1&amp;b=2"><meta property="og:image" content="http://127.0.0.1/private"><script type="application/ld+json">{"@graph":[{"thumbnailUrl":"/thumb.webp","image":{"url":"/cover.jpg"}}]}</script>`)
	result, err := extractPageImages(raw, "https://courses.example/course/lesson")
	if err != nil || result.Title != "Hello & Carrie" || len(result.Images) != 3 || result.Images[0] != "https://courses.example/poster.png?a=1&b=2" {
		t.Fatalf("extraction: %+v %v", result, err)
	}
	if _, _, _, err := normalizeCover([]byte(`<svg onload="alert(1)"></svg>`)); err == nil {
		t.Fatal("SVG accepted")
	}
	if _, _, _, err := normalizeCover(make([]byte, maxCoverBytes+1)); err == nil {
		t.Fatal("oversized image accepted")
	}
}

func TestCoverSearchAIAndFullDirectory(t *testing.T) {
	s := testStore(t)
	directory := "Hello Carrie自然拼读/01 Alphabet A to Z 自然拼读A-Z"
	path := directory + "/01 Alphabet A Sound Song.mp4"
	put(t, s, path, "")
	id := scan(t, s, directory, "episode")[0]
	v, _ := s.getVideo(id)
	e, err := s.evidence(v)
	if err != nil || e.Directory != directory {
		t.Fatal("full directory missing", e, err)
	}
	mockPublicPages(t, s)
	_, err = s.saveSettings(SettingsRequest{Config: Config{Provider: "compatible", BaseURL: "https://ai.example", Model: "test", APIKey: "ai-secret", SearchProvider: "tavily", SearchURL: "https://search.example", SearchKey: "search-secret", JSONMode: true}})
	if err != nil {
		t.Fatal(err)
	}
	aiCalls, searchCalls := 0, 0
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Host == "search.example" {
			searchCalls++
			if r.Header.Get("Authorization") != "Bearer search-secret" || body["query"] != "Hello Carrie Alphabet A Sound Song" {
				t.Errorf("search request: %+v", body)
			}
			return mockResponse(r, 200, "application/json", []byte(`{"results":[{"title":"Alphabet A Sound Song","url":"https://courses.example/lesson","content":"Hello Carrie"}]}`)), nil
		}
		aiCalls++
		if r.Header.Get("Authorization") != "Bearer ai-secret" {
			t.Error("wrong AI secret")
		}
		messages := body["messages"].([]any)
		var context map[string]any
		json.Unmarshal([]byte(messages[1].(map[string]any)["content"].(string)), &context)
		response := map[string]any{"query": "Hello Carrie Alphabet A Sound Song"}
		if aiCalls == 1 {
			if context["directory"] != directory {
				t.Errorf("query lacks directory: %+v", context)
			}
		} else {
			video := context["video"].(map[string]any)
			if video["directory"] != directory || video["series_directory"] != directory {
				t.Errorf("ranking lacks directory: %+v", video)
			}
			candidate := context["candidates"].([]any)[0].(map[string]any)
			response = map[string]any{"matches": []any{map[string]any{"id": "invented-id", "match": "likely", "reason": "ignore"}, map[string]any{"id": candidate["id"], "match": "likely", "reason": "课程及集名一致"}}}
		}
		return mockResponse(r, 200, "application/json", []byte(encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": encode(response)}}}}))), nil
	})}
	result, err := s.searchCovers(context.Background(), CoverRequest{ID: id, Scope: "video", UseAI: true})
	if err != nil || len(result.Candidates) != 1 || aiCalls != 2 || searchCalls != 1 {
		t.Fatalf("search: %+v %v calls=%d/%d", result, err, aiCalls, searchCalls)
	}
	candidate := result.Candidates[0]
	if candidate.Match != "likely" || candidate.SourceURL != "https://courses.example/lesson" || candidate.Width != 96 {
		t.Fatal(candidate)
	}
	if strings.Contains(encode(s.getConfig().public()), "secret") {
		t.Fatal("search key exposed")
	}
	info, err := s.coverInfo(id, "video")
	if err != nil {
		t.Fatal(err)
	}
	cached, ok := info["candidates"].([]CoverCandidate)
	if !ok || len(cached) != 1 || cached[0].ID != candidate.ID || !strings.Contains(cached[0].PreviewURL, "candidate=") {
		t.Fatalf("cached candidates: %+v", info["candidates"])
	}
	draft(t, s, id)
	saved, err := s.saveCover(SaveCoverRequest{ID: id, Scope: "video", CandidateID: candidate.ID})
	if err != nil || saved["saved"] != true {
		t.Fatal(saved, err)
	}
	thumb, err := os.ReadFile(filepath.Join(s.root, directory, "01 Alphabet A Sound Song-thumb.jpg"))
	if err != nil {
		t.Fatal("episode still was not written", err)
	}
	fanart, err := os.ReadFile(filepath.Join(s.root, directory, "01 Alphabet A Sound Song-fanart.jpg"))
	if err != nil || !bytes.Equal(thumb, fanart) {
		t.Fatalf("episode fanart was not written: %v", err)
	}
	files, err := s.preview(id)
	if err != nil || !strings.Contains(files[1].XML, `<thumb aspect="thumb">01 Alphabet A Sound Song-thumb.jpg</thumb>`) || !strings.Contains(files[1].XML, "<fanart>") || !strings.Contains(files[1].XML, `<thumb>01 Alphabet A Sound Song-fanart.jpg</thumb>`) || strings.Contains(files[1].XML, "<art>") {
		t.Fatal(files, err)
	}
	// Series context uses the full hierarchy without treating the chosen episode as the whole series.
	seriesContext := coverContext(v, "series")
	if seriesContext["directory"] != directory || seriesContext["filename"] != nil {
		t.Fatal(seriesContext)
	}
}

func TestSearXNGSearchAndFailures(t *testing.T) {
	s := testStore(t)
	_, err := s.saveSettings(SettingsRequest{Config: Config{Provider: "compatible", SearchProvider: "searxng", SearchURL: "http://search.lan:8080"}})
	if err != nil {
		t.Fatal(err)
	}
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Query().Get("q") != "课程 A" || r.URL.Query().Get("format") != "json" || r.Header.Get("Authorization") != "" {
			t.Error("incorrect SearXNG request")
		}
		return mockResponse(r, 200, "application/json", []byte(`{"results":[{"url":"http://127.0.0.1/private"},{"url":"https://courses.example/lesson"}]}`)), nil
	})}
	pages, err := s.searchPages(context.Background(), "课程 A")
	if err != nil || len(pages) != 1 {
		t.Fatal(pages, err)
	}
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return mockResponse(r, 403, "text/plain", []byte("search-secret")), nil
	})}
	_, err = s.searchPages(context.Background(), "课程 A")
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "403") {
		t.Fatal(err)
	}
}

func TestCoverSaveOwnershipBackupAndAuth(t *testing.T) {
	s := testStore(t)
	put(t, s, "course/01-first.mp4", "")
	put(t, s, "course/02-second.mp4", "")
	ids := scan(t, s, "course", "episode")
	for _, id := range ids {
		draft(t, s, id)
	}
	mockPublicPages(t, s)
	app := newWebApp(s, "")
	w := request(app, "POST", "/api/covers/search", encode(CoverRequest{ID: ids[0], Scope: "series", SourceURL: "https://courses.example/lesson"}), nil, "")
	var results CoverResults
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &results) != nil || len(results.Candidates) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	c := results.Candidates[0]
	if _, err := s.saveCover(SaveCoverRequest{ID: ids[0], Scope: "video", CandidateID: c.ID}); err == nil {
		t.Fatal("series candidate used for video")
	}
	body := SaveCoverRequest{ID: ids[1], Scope: "series", CandidateID: c.ID}
	w = request(app, "POST", "/api/covers/save", encode(body), nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"saved":true`) {
		t.Fatal(w.Body.String())
	}
	imagePath := filepath.Join(s.root, "course/poster.jpg")
	fanartPath := filepath.Join(s.root, "course/fanart.jpg")
	original, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, format, err := image.DecodeConfig(bytes.NewReader(original)); err != nil || format != "jpeg" {
		t.Fatal("not normalized JPEG", err)
	}
	if fanart, err := os.ReadFile(fanartPath); err != nil || !bytes.Equal(original, fanart) {
		t.Fatalf("series fanart was not written: %v", err)
	}
	var stored []byte
	if err := s.db.QueryRow("SELECT image FROM cover_assets WHERE owner=?", "show:course").Scan(&stored); err != nil || !bytes.Equal(original, stored) {
		t.Fatalf("selected cover was not stored in database: %v", err)
	}
	w = request(app, "POST", "/api/covers/save", encode(body), nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"saved":false`) {
		t.Fatal(w.Body.String())
	}
	body.Overwrite = true
	w = request(app, "POST", "/api/covers/save", encode(body), nil, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	backups, _ := filepath.Glob(imagePath + ".bak.*")
	if len(backups) != 1 {
		t.Fatal(backups)
	}
	backup, _ := os.ReadFile(backups[0])
	if !bytes.Equal(original, backup) {
		t.Fatal("backup changed")
	}
	files, err := s.preview(ids[0])
	if err != nil || !strings.Contains(files[0].XML, `<thumb aspect="poster">poster.jpg</thumb>`) || !strings.Contains(files[0].XML, "<fanart>") || !strings.Contains(files[0].XML, `<thumb>fanart.jpg</thumb>`) || strings.Contains(files[0].XML, "<art>") {
		t.Fatal(files, err)
	}
	if err := os.Remove(imagePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fanartPath); err != nil {
		t.Fatal(err)
	}
	files, err = s.preview(ids[0])
	if err != nil || !strings.Contains(files[0].XML, `<thumb aspect="poster">poster.jpg</thumb>`) || !strings.Contains(files[0].XML, `<thumb>fanart.jpg</thumb>`) {
		t.Fatal("database artwork was not used for NFO references", files, err)
	}
	w = request(app, "GET", "/api/cover-image?candidate="+c.ID, "", nil, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatal(w.Code)
	}
	w = request(newWebApp(s, "password123"), "GET", "/api/cover-image?candidate="+c.ID, "", nil, "")
	if w.Code != 401 {
		t.Fatal("unauthenticated image access")
	}
	if _, err = s.db.Exec("UPDATE cover_candidates SET created=0 WHERE id=?", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.saveCover(body); err == nil {
		t.Fatal("expired candidate accepted")
	}
	if _, err = s.coverImage("", "", c.ID); err == nil {
		t.Fatal("expired candidate image returned")
	}
	if err = os.Symlink(filepath.Join(s.root, "course/01-first.mp4"), imagePath); err != nil {
		t.Fatal(err)
	}
	newCandidate, err := s.cacheCover("show:course", CoverCandidate{}, testCoverPNG(t))
	if err != nil {
		t.Fatal(err)
	}
	body.CandidateID = newCandidate.ID
	if _, err = s.saveCover(body); err == nil {
		t.Fatal("cover target symlink accepted")
	}
}

func TestCoverErrorsAndSnapshotMissingTool(t *testing.T) {
	s := testStore(t)
	put(t, s, "video.mp4", "")
	id := scan(t, s, "", "movie")[0]
	mockPublicPages(t, s)
	if _, err := s.searchCovers(context.Background(), CoverRequest{ID: id, Scope: "video"}); err == nil {
		t.Fatal("search without provider accepted")
	}
	result, err := s.searchCovers(context.Background(), CoverRequest{ID: id, Scope: "video", SourceURL: "https://courses.example/poster.png", UseAI: true})
	if err != nil || len(result.Candidates) != 1 || len(result.Warnings) == 0 {
		t.Fatal(result, err)
	}
	put(t, s, "video.mkv", "")
	if _, err = s.saveCover(SaveCoverRequest{ID: id, Scope: "video", CandidateID: result.Candidates[0].ID}); err == nil {
		t.Fatal("same stem cover collision accepted")
	}
	removeTestVideo(t, s, "video.mkv")
	t.Setenv("PATH", t.TempDir())
	if _, err = s.snapshotCover(context.Background(), SnapshotRequest{ID: id, Scope: "video", Seconds: 1}); err == nil || !strings.Contains(err.Error(), "FFmpeg") {
		t.Fatal(err)
	}
	if _, err = s.snapshotCover(context.Background(), SnapshotRequest{ID: id, Scope: "video", Seconds: -1}); err == nil {
		t.Fatal("negative seek accepted")
	}
}

func TestScreenshotOutputLimit(t *testing.T) {
	output := &limitedBuffer{limit: 4}
	if _, ok := any(output).(io.ReaderFrom); ok {
		t.Fatal("ReaderFrom must not bypass the output limit")
	}
	_, err := io.Copy(output, struct{ io.Reader }{strings.NewReader("too much screenshot data")})
	if err == nil || len(output.Bytes()) > 4 {
		t.Fatal("screenshot output was not bounded", err)
	}
}
