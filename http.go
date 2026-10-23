package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var assets embed.FS

type webApp struct {
	store    *Store
	password string
	mu       sync.Mutex
	sessions map[string]time.Time
	failures map[string][]time.Time
}

func newWebApp(s *Store, password string) *webApp {
	return &webApp{store: s, password: password, sessions: map[string]time.Time{}, failures: map[string][]time.Time{}}
}
func (a *webApp) authenticated(r *http.Request) bool {
	if a.password == "" {
		return true
	}
	cookie, err := r.Cookie("session")
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().Before(a.sessions[cookie.Value])
}
func sendJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
func failHTTP(w http.ResponseWriter, err error) {
	status := 500
	var ue userError
	if errors.As(err, &ue) {
		status = 400
	} else if errors.Is(err, os.ErrPermission) {
		status = 403
	}
	sendJSON(w, status, map[string]string{"error": publicError(err)})
}
func decodeBody(raw []byte, out any) error {
	if json.Unmarshal(raw, out) != nil {
		return userError("请求参数格式错误")
	}
	return nil
}
func (a *webApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		sendJSON(w, 405, map[string]string{"error": "不支持的请求方法"})
		return
	}
	path := r.URL.Path
	post := r.Method == http.MethodPost
	if !post {
		name, mime := "", ""
		switch path {
		case "/":
			name = "index.html"
			mime = "text/html"
		case "/app.js":
			name = "app.js"
			mime = "text/javascript"
		case "/covers.js":
			name = "covers.js"
			mime = "text/javascript"
		case "/style.css":
			name = "style.css"
			mime = "text/css"
		}
		if name != "" {
			raw, err := assets.ReadFile("static/" + name)
			if err != nil {
				failHTTP(w, err)
				return
			}
			w.Header().Set("Content-Type", mime+"; charset=utf-8")
			w.Write(raw)
			return
		}
		if path == "/health" {
			sendJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		if path == "/api/session" {
			sendJSON(w, 200, map[string]bool{"authenticated": a.authenticated(r), "password_required": a.password != ""})
			return
		}
	}
	raw := []byte("{}")
	if post {
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || u.Scheme != "http" && u.Scheme != "https" {
				sendJSON(w, 403, map[string]string{"error": "不接受跨站请求"})
				return
			}
		}
		if strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) != "application/json" {
			sendJSON(w, 415, map[string]string{"error": "需要 application/json"})
			return
		}
		var err error
		raw, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 2000000))
		if err != nil {
			failHTTP(w, userError("请求大小不合法"))
			return
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil || object == nil {
			failHTTP(w, userError("请求必须是 JSON 对象"))
			return
		}
	}
	if post && path == "/api/login" {
		a.login(w, r, raw)
		return
	}
	if !a.authenticated(r) {
		sendJSON(w, 401, map[string]string{"error": "请先登录"})
		return
	}
	if !post && path == "/api/cover-image" {
		q := r.URL.Query()
		raw, err := a.store.coverImage(q.Get("id"), q.Get("scope"), q.Get("candidate"))
		if err != nil {
			failHTTP(w, err)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(raw)
		return
	}
	result, err := a.dispatch(w, r, raw)
	if err != nil {
		failHTTP(w, err)
		return
	}
	if result == nil {
		sendJSON(w, 404, map[string]string{"error": "接口不存在"})
		return
	}
	sendJSON(w, 200, result)
}
func (a *webApp) login(w http.ResponseWriter, r *http.Request, raw []byte) {
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeBody(raw, &body); err != nil {
		failHTTP(w, err)
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	timestamp := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, attempts := range a.failures {
		recent := []time.Time{}
		for _, at := range attempts {
			if timestamp.Sub(at) < time.Minute {
				recent = append(recent, at)
			}
		}
		if len(recent) == 0 {
			delete(a.failures, key)
		} else {
			a.failures[key] = recent
		}
	}
	if len(a.failures[ip]) >= 10 {
		sendJSON(w, 429, map[string]string{"error": "尝试过多，请一分钟后再试"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(a.password)) != 1 {
		a.failures[ip] = append(a.failures[ip], timestamp)
		sendJSON(w, 401, map[string]string{"error": "访问密码不正确"})
		return
	}
	delete(a.failures, ip)
	for key, expiry := range a.sessions {
		if !timestamp.Before(expiry) {
			delete(a.sessions, key)
		}
	}
	token := randomID()
	a.sessions[token] = timestamp.Add(12 * time.Hour)
	http.SetCookie(w, &http.Cookie{Name: "session", Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	sendJSON(w, 200, map[string]bool{"ok": true})
}
func (a *webApp) dispatch(w http.ResponseWriter, r *http.Request, raw []byte) (any, error) {
	s := a.store
	query := r.URL.Query()
	ok := map[string]bool{"ok": true}
	if r.Method == http.MethodGet {
		switch r.URL.Path {
		case "/api/state":
			videos, err := s.listVideos()
			if err != nil {
				return nil, err
			}
			jobs, err := s.listJobs()
			if err != nil {
				return nil, err
			}
			info, err := os.Stat(s.root)
			return map[string]any{"videos": videos, "jobs": jobs, "settings": s.getConfig().public(), "media_root": s.root, "root_available": err == nil && info.IsDir()}, nil
		case "/api/directories":
			return s.directories(query.Get("path"))
		case "/api/unavailable":
			return s.unavailableVideos()
		case "/api/covers":
			return s.coverInfo(query.Get("id"), query.Get("scope"))
		case "/api/preview":
			files, err := s.preview(query.Get("id"))
			return map[string]any{"files": files}, err
		case "/api/context":
			v, err := s.getVideo(query.Get("id"))
			if err != nil {
				return nil, err
			}
			return s.evidence(v)
		}
	} else {
		switch r.URL.Path {
		case "/api/logout":
			if cookie, err := r.Cookie("session"); err == nil {
				a.mu.Lock()
				delete(a.sessions, cookie.Value)
				a.mu.Unlock()
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
			return ok, nil
		case "/api/scan":
			body := ScanRequest{Kind: "movie", Recursive: true}
			if err := decodeBody(raw, &body); err != nil {
				return nil, err
			}
			return s.scan(body)
		case "/api/cleanup":
			var body CleanupRequest
			if err := decodeBody(raw, &body); err != nil {
				return nil, err
			}
			return s.cleanupUnavailable(body)
		case "/api/records/clear":
			var body ClearRecordsRequest
			if err := decodeBody(raw, &body); err != nil {
				return nil, err
			}
			return s.clearRecords(body)
		case "/api/covers/search":
			var body CoverRequest
			if err := decodeBody(raw, &body); err != nil {
				return nil, err
			}
			return s.searchCovers(r.Context(), body)
		case "/api/covers/snapshot":
			var body SnapshotRequest
			if err := decodeBody(raw, &body); err != nil {
				return nil, err
			}
			return s.snapshotCover(r.Context(), body)
		case "/api/covers/save":
			var body SaveCoverRequest
			if err := decodeBody(raw, &body); err != nil {
				return nil, err
			}
			return s.saveCover(body)
		case "/api/settings":
			body := SettingsRequest{Config: Config{Provider: "compatible", JSONMode: true}}
			if err := decodeBody(raw, &body); err != nil {
				return nil, err
			}
			return s.saveSettings(body)
		case "/api/test-ai":
			m, err := s.ai(r.Context(), map[string]string{"filename": "接口连通性测试", "evidence": "请返回标题为连接成功的空简介 JSON"})
			return map[string]any{"ok": true, "result": m}, err
		case "/api/edit":
			var body EditRequest
			if err := decodeBody(raw, &body); err != nil {
				return nil, err
			}
			return s.edit(body)
		case "/api/jobs":
			var body JobRequest
			if err := decodeBody(raw, &body); err != nil {
				return nil, err
			}
			return s.startJob(body)
		case "/api/cancel":
			var body struct {
				ID string `json:"id"`
			}
			if err := decodeBody(raw, &body); err != nil {
				return nil, err
			}
			return ok, s.cancelJob(body.ID)
		}
	}
	return nil, nil
}
