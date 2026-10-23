package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := newStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return s
}
func put(t *testing.T, s *Store, path, text string) {
	t.Helper()
	p := filepath.Join(s.root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}
func scan(t *testing.T, s *Store, path, kind string) []string {
	t.Helper()
	r, err := s.scan(ScanRequest{Path: path, Kind: kind, Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	return r.IDs
}
func draft(t *testing.T, s *Store, id string) {
	t.Helper()
	v, err := s.getVideo(id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.edit(EditRequest{ID: id, Metadata: &Metadata{Title: "标题 & <安全>", Plot: "简介\n第二行"}, Show: &Metadata{Title: "系列 & 总览"}, Season: v.Season, Episode: v.Episode})
	if err != nil {
		t.Fatal(err)
	}
}
func waitJob(t *testing.T, s *Store, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		jobs, err := s.listJobs()
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range jobs {
			if job.ID == id && job.Status != "queued" && job.Status != "running" && job.Status != "cancelling" {
				return job
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Job{}
}
func start(t *testing.T, s *Store, action string, ids []string, overwrite bool) string {
	t.Helper()
	r, err := s.startJob(JobRequest{Action: action, IDs: ids, Overwrite: overwrite})
	if err != nil {
		t.Fatal(err)
	}
	return r["id"]
}

func TestScanAndPathBoundaries(t *testing.T) {
	s := testStore(t)
	put(t, s, "课程/Season 02/01-开场.mp4", "")
	put(t, s, "课程/S03E02-介绍.mkv", "")
	put(t, s, "课程/没有编号.mp4", "")
	put(t, s, "课程/S01E01E02.mp4", "")
	put(t, s, "课程/.hidden/03.mp4", "")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../escape", "/etc/passwd", "link/new.nfo"} {
		if _, err := s.safe(p, false); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
	result, err := s.scan(ScanRequest{Path: "课程", Kind: "episode", Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 3 || len(result.Warnings) != 1 {
		t.Fatalf("scan=%+v", result)
	}
	v, err := s.getVideo(identifier("课程/Season 02/01-开场.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if *v.Season != 2 || *v.Episode != 1 {
		t.Fatalf("wrong numbering: %+v", v)
	}
	draft(t, s, v.ID)
	scan(t, s, "课程", "movie")
	v, _ = s.getVideo(v.ID)
	if v.Metadata == nil || v.Kind != "episode" {
		t.Fatal("rescan discarded edits")
	}
	listing, err := s.directories("")
	if err != nil || len(listing.Directories) != 1 {
		t.Fatalf("directories: %+v %v", listing, err)
	}
}
func TestNFOValidationAndAtomicWrites(t *testing.T) {
	s := testStore(t)
	put(t, s, "video.mp4", "")
	id := scan(t, s, "", "movie")[0]
	draft(t, s, id)
	files, err := s.preview(id)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		XMLName xml.Name
		Title   string `xml:"title"`
	}
	if err = xml.Unmarshal([]byte(files[0].XML), &parsed); err != nil || parsed.Title != "标题 & <安全>" || parsed.XMLName.Local != "movie" {
		t.Fatalf("invalid XML: %s %v", files[0].XML, err)
	}
	job := waitJob(t, s, start(t, s, "save", []string{id}, false))
	if job.Status != "completed" {
		t.Fatal(job)
	}
	put(t, s, "video.nfo", "original")
	job = waitJob(t, s, start(t, s, "save", []string{id}, false))
	if job.Skipped != 1 {
		t.Fatal(job)
	}
	b, _ := os.ReadFile(filepath.Join(s.root, "video.nfo"))
	if string(b) != "original" {
		t.Fatal("overwrote file without consent")
	}
	job = waitJob(t, s, start(t, s, "save", []string{id}, true))
	if job.Failed != 0 {
		t.Fatal(job)
	}
	backups, _ := filepath.Glob(filepath.Join(s.root, "video.nfo.bak.*"))
	if len(backups) != 1 {
		t.Fatal(backups)
	}
	b, _ = os.ReadFile(backups[0])
	if string(b) != "original" {
		t.Fatal("backup differs")
	}
	put(t, s, "video.mkv", "")
	if _, err = s.preview(id); err == nil {
		t.Fatal("same-stem collision accepted")
	}
	if err = os.Symlink(filepath.Join(s.root, "video.nfo"), filepath.Join(s.root, "linked.nfo")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeNFO("linked.nfo", "bad", true); err == nil {
		t.Fatal("symlink write accepted")
	}
}
func TestEpisodeConflicts(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"course/S01E01.mp4", "course/01-again.mp4", "course/unknown.mp4"} {
		put(t, s, name, "")
	}
	ids := scan(t, s, "course", "episode")
	for _, id := range ids {
		draft(t, s, id)
		if _, err := s.preview(id); err == nil {
			t.Fatal("missing or duplicate episode allowed")
		}
	}
	id := identifier("course/unknown.mp4")
	_, err := s.edit(EditRequest{ID: id, Metadata: &Metadata{Title: "特别篇"}, Show: &Metadata{Title: "系列"}, Season: intPointer(0), Episode: intPointer(0)})
	if err != nil {
		t.Fatal(err)
	}
	files, err := s.preview(id)
	if err != nil || len(files) != 2 {
		t.Fatalf("preview: %v %v", files, err)
	}
	if !strings.Contains(files[1].XML, "<season>0</season>") {
		t.Fatal("zero season lost")
	}
}
func TestEvidenceLimitsAndLiteralFilenames(t *testing.T) {
	s := testStore(t)
	put(t, s, "[demo].mp4", "")
	put(t, s, "[demo].zh.srt", "1\n00:00:00,000 --> 00:00:01,000\n真实字幕\n")
	put(t, s, "[demo].info.json", `{"title":"简介","description":"内容","secret":"must not send"}`)
	id := scan(t, s, "", "movie")[0]
	v, _ := s.getVideo(id)
	e, err := s.evidence(v)
	if err != nil || len(e.Sources) != 2 {
		t.Fatalf("evidence: %+v %v", e, err)
	}
	if strings.Contains(encode(e), "must not send") || strings.Contains(encode(e), "-->") {
		t.Fatal("unselected data leaked")
	}
	put(t, s, "[demo].txt", strings.Repeat("长", 30000))
	e, err = s.evidence(v)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, source := range e.Sources {
		count += len([]rune(source.Text))
	}
	if count != 24000 || !e.Sources[0].Truncated {
		t.Fatal("source budget not respected")
	}
}
func configure(t *testing.T, s *Store, url, provider string) {
	t.Helper()
	_, err := s.saveSettings(SettingsRequest{Config: Config{Provider: provider, BaseURL: url, Model: "test-model", APIKey: "test-secret", JSONMode: true}})
	if err != nil {
		t.Fatal(err)
	}
}
func TestAIProtocolsAndGeneration(t *testing.T) {
	for _, provider := range []string{"compatible", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			s := testStore(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				expected := "/chat/completions"
				if provider == "ollama" {
					expected = "/api/chat"
					if body["format"] != "json" {
						t.Error("missing Ollama format")
					}
				} else if body["response_format"] == nil {
					t.Error("missing JSON mode")
				}
				if r.URL.Path != expected || r.Header.Get("Authorization") != "Bearer test-secret" || body["model"] != "test-model" {
					t.Errorf("wrong request: %s %+v", r.URL.Path, body)
				}
				m := map[string]any{"content": encode(Metadata{Title: "生成标题", Plot: "仅根据提供资料"})}
				result := map[string]any{"choices": []any{map[string]any{"message": m}}}
				if provider == "ollama" {
					result = map[string]any{"message": m}
				}
				sendJSON(w, 200, result)
			}))
			defer server.Close()
			configure(t, s, server.URL, provider)
			put(t, s, "course/S01E01.mp4", "")
			put(t, s, "course/S01E02.mp4", "")
			ids := scan(t, s, "course", "episode")
			job := waitJob(t, s, start(t, s, "generate", ids, false))
			if job.Failed != 0 || job.Done != 2 || calls.Load() != 3 {
				t.Fatalf("generation: %+v calls=%d", job, calls.Load())
			}
			job = waitJob(t, s, start(t, s, "generate", ids, false))
			if job.Skipped != 2 || calls.Load() != 3 {
				t.Fatal("existing drafts regenerated")
			}
			job = waitJob(t, s, start(t, s, "save", ids, false))
			if job.Failed != 0 {
				t.Fatal(job)
			}
			for _, p := range []string{"course/tvshow.nfo", "course/S01E01.nfo", "course/S01E02.nfo"} {
				if _, err := os.Stat(filepath.Join(s.root, p)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestAIFailuresPreserveDraftAndHideSecrets(t *testing.T) {
	s := testStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "test-secret", 401) }))
	defer server.Close()
	configure(t, s, server.URL, "compatible")
	put(t, s, "one.mp4", "")
	id := scan(t, s, "", "movie")[0]
	draft(t, s, id)
	job := waitJob(t, s, start(t, s, "generate", []string{id}, true))
	v, _ := s.getVideo(id)
	if job.Failed != 1 || v.Metadata == nil || strings.Contains(v.Error, "test-secret") || !strings.Contains(v.Error, "401") {
		t.Fatalf("failure handling: %+v %+v", job, v)
	}
	if strings.Contains(encode(s.getConfig().public()), "test-secret") {
		t.Fatal("settings leak key")
	}
	info, err := os.Stat(filepath.Join(s.data, "settings.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("settings permission", err)
	}
}
func TestJobCancellationAndMutualExclusion(t *testing.T) {
	s := testStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		sendJSON(w, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"title":"完成"}`}}}})
	}))
	defer server.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	configure(t, s, server.URL, "compatible")
	put(t, s, "one.mp4", "")
	put(t, s, "two.mp4", "")
	ids := scan(t, s, "", "movie")
	id := start(t, s, "generate", ids, false)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("AI never called")
	}
	if _, err := s.startJob(JobRequest{Action: "save", IDs: ids}); err == nil {
		t.Fatal("parallel job accepted")
	}
	if _, err := s.saveSettings(SettingsRequest{Config: Config{Provider: "ollama"}}); err == nil {
		t.Fatal("settings changed during job")
	}
	if err := s.cancelJob(id); err != nil {
		t.Fatal(err)
	}
	close(release)
	job := waitJob(t, s, id)
	if job.Status != "cancelled" || job.Done != 1 || calls.Load() != 1 {
		t.Fatal(job)
	}
}
func request(a http.Handler, method, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}
func TestHTTPAuthenticationAndWorkflow(t *testing.T) {
	s := testStore(t)
	put(t, s, "movie.mp4", "")
	a := newWebApp(s, "password123")
	if w := request(a, "GET", "/", "", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "AI NFO") {
		t.Fatal("embedded page unavailable")
	}
	if w := request(a, "GET", "/api/state", "", nil, ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := request(a, "POST", "/api/login", `{"password":"password123"}`, nil, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("weak cookie")
	}
	w = request(a, "POST", "/api/scan", `{"kind":"movie","recursive":true}`, cookie, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = request(a, "POST", "/api/edit", encode(EditRequest{ID: identifier("movie.mp4"), Metadata: &Metadata{Title: "Web 编辑"}}), cookie, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = request(a, "GET", "/api/preview?id="+identifier("movie.mp4"), "", cookie, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Web 编辑") {
		t.Fatal(w.Body.String())
	}
	w = request(a, "GET", "/api/state", "", cookie, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "api_key") {
		t.Fatal(w.Body.String())
	}
	for _, body := range []string{"null", "[]", `{} {}`, `{"recursive":"yes"}`} {
		if w = request(a, "POST", "/api/scan", body, cookie, ""); w.Code != 400 {
			t.Errorf("accepted %s: %d", body, w.Code)
		}
	}
	if w = request(a, "POST", "/api/scan", "{}", cookie, "https://foreign.invalid"); w.Code != 403 {
		t.Fatal("cross-site request accepted")
	}
	request(a, "POST", "/api/logout", "{}", cookie, "")
	if w = request(a, "GET", "/api/state", "", cookie, ""); w.Code != 401 {
		t.Fatal("session survived logout")
	}
	for i := 0; i < 10; i++ {
		request(a, "POST", "/api/login", `{"password":"wrong"}`, nil, "")
	}
	if w = request(a, "POST", "/api/login", `{"password":"password123"}`, nil, ""); w.Code != 429 {
		t.Fatal("login throttle failed")
	}
}
func TestAIRejectsRedirectsAndMalformedOutput(t *testing.T) {
	s := testStore(t)
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer redirect.Close()
	configure(t, s, redirect.URL, "compatible")
	if _, err := s.ai(context.Background(), nil); err == nil || targetCalls.Load() != 0 {
		t.Fatal("redirect followed")
	}
	for _, content := range []string{"null", `{"plot":"no title"}`, `{"title":4}`, "not JSON"} {
		t.Run(content, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}}))
			}))
			defer server.Close()
			configure(t, s, server.URL, "compatible")
			if _, err := s.ai(context.Background(), nil); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}
func TestRestartMarksUnfinishedJobs(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	s, err := newStore(root, data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO jobs(id,status) VALUES('unfinished','running')"); err != nil {
		t.Fatal(err)
	}
	s.close()
	s, err = newStore(root, data)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var status string
	if err = s.db.QueryRow("SELECT status FROM jobs WHERE id='unfinished'").Scan(&status); err != nil || status != "interrupted" {
		t.Fatal(status, err)
	}
}
