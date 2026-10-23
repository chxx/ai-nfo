package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const nativeSearchFixture = `{"status":"completed","output":[{"type":"web_search_call","status":"completed","action":{"sources":[{"url":"https://courses.example/lesson"}]}},{"type":"message","status":"completed","content":[{"type":"output_text","text":"invented https://fake.example/nope","annotations":[{"type":"url_citation","url":"https://courses.example/lesson?utm_source=openai","title":"Alphabet A Sound Song"},{"type":"url_citation","url":"http://127.0.0.1/private"}]}]}]}`

func TestModelSearchUsesExistingConfigAndVerifiedSources(t *testing.T) {
	s := testStore(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("wrong endpoint or credential: %s", r.URL.Path)
		}
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if p["model"] != "test-model" || p["tool_choice"] != "required" || p["store"] != false || p["stream"] != false || p["response_format"] != nil {
			t.Error("invalid native search payload")
		}
		if !strings.Contains(encode(p["tools"]), "web_search") || !strings.Contains(encode(p["include"]), "web_search_call.action.sources") {
			t.Error("search tool missing")
		}
		if !strings.Contains(encode(p["input"]), "Hello Carrie") {
			t.Error("directory context missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(nativeSearchFixture))
	}))
	defer server.Close()
	configure(t, s, server.URL+"/v1", "compatible") // Empty search provider defaults to native search; no independent search key.
	mockPublicPages(t, s)
	put(t, s, "Hello Carrie/Alphabet A.mp4", "video")
	id := scan(t, s, "", "movie")[0]
	result, err := s.searchCovers(context.Background(), CoverRequest{ID: id, Scope: "video"})
	if err != nil || len(result.Candidates) != 1 || calls != 1 {
		t.Fatal(result, err, calls)
	}
	if result.Candidates[0].SourceURL != "https://courses.example/lesson" {
		t.Fatal("unverified source", result)
	}
	saved, err := s.saveCover(SaveCoverRequest{ID: id, Scope: "video", CandidateID: result.Candidates[0].ID})
	if err != nil || saved["saved"] != true {
		t.Fatal(saved, err)
	}
	c := s.getConfig()
	c.SearchProvider = "model"
	if _, err := s.saveSettings(SettingsRequest{Config: c}); err != nil {
		t.Fatal("cannot save native search mode", err)
	}
}

func TestModelSearchRejectsUnverifiedResults(t *testing.T) {
	for _, raw := range []string{
		`not json`,
		`{"status":"failed","output":[]}`,
		`{"status":"completed","output":[{"type":"message","status":"completed","content":[{"annotations":[{"type":"url_citation","url":"https://courses.example/lesson"}]}]}]}`,
		`{"status":"completed","output":[{"type":"web_search_call","status":"failed","action":{"sources":[{"url":"https://courses.example/lesson"}]}}]}`,
		`{"status":"completed","output":[{"type":"web_search_call","status":"completed"},{"type":"message","status":"completed","content":[{"text":"https://fake.example/invented"}]}]}`,
		`{"status":"completed","output":[{"type":"web_search_call","status":"completed","action":{"sources":[{"url":"file:///etc/passwd"},{"url":"http://192.168.1.1/secret"}]}}]}`,
	} {
		if _, err := parseSearchResponse([]byte(raw)); err == nil {
			t.Fatal("accepted unsupported or unverified response", raw)
		}
	}
	pages, err := parseSearchResponse([]byte(nativeSearchFixture))
	if err != nil || len(pages) != 1 || pages[0].Title != "Alphabet A Sound Song" {
		t.Fatal(pages, err)
	}
	if _, err := searchBase(Config{Provider: "ollama", BaseURL: "http://localhost:11434", Model: "local", SearchProvider: "model"}); err == nil {
		t.Fatal("native search accepted Ollama")
	}
	if _, err := searchBase(Config{Provider: "compatible", SearchProvider: "model"}); err == nil {
		t.Fatal("unconfigured native search accepted")
	}
	if _, err := searchBase(Config{Provider: "compatible", BaseURL: "http://example.com", Model: "test", SearchProvider: "none"}); err == nil {
		t.Fatal("explicitly disabled search enabled")
	}
}

func TestModelSearchErrorsDoNotLeakSecrets(t *testing.T) {
	s := testStore(t)
	response := "secret upstream text"
	status := 400
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); w.Write([]byte(response)) }))
	defer server.Close()
	configure(t, s, server.URL, "compatible")
	_, err := s.searchPages(context.Background(), "public course")
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "400") {
		t.Fatal("unsafe or missing upstream error", err)
	}
	status = 200
	response = strings.Repeat("x", 2000001)
	if _, err := s.searchPages(context.Background(), "public course"); err == nil {
		t.Fatal("oversized response accepted")
	}
}
