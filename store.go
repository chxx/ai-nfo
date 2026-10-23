package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS videos (
 id TEXT PRIMARY KEY, path TEXT UNIQUE, size INTEGER, kind TEXT,
 show_path TEXT, season INTEGER, episode INTEGER, metadata TEXT,
 status TEXT DEFAULT 'new', error TEXT DEFAULT '', updated REAL);
CREATE TABLE IF NOT EXISTS shows (path TEXT PRIMARY KEY, metadata TEXT);
CREATE TABLE IF NOT EXISTS cover_candidates (
 id TEXT PRIMARY KEY, owner TEXT NOT NULL, image BLOB NOT NULL, metadata TEXT NOT NULL, created REAL NOT NULL);
CREATE TABLE IF NOT EXISTS cover_selections (
 owner TEXT PRIMARY KEY, target TEXT NOT NULL, metadata TEXT NOT NULL, updated REAL NOT NULL);
CREATE TABLE IF NOT EXISTS jobs (
 id TEXT PRIMARY KEY, action TEXT, status TEXT, ids TEXT,
 total INTEGER, done INTEGER DEFAULT 0, failed INTEGER DEFAULT 0,
 skipped INTEGER DEFAULT 0, message TEXT DEFAULT '', created REAL);
CREATE TABLE IF NOT EXISTS job_items (
 job_id TEXT NOT NULL, video_id TEXT NOT NULL, status TEXT NOT NULL,
 overwrite INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', retry_job_id TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(job_id,video_id));
UPDATE job_items SET status='interrupted',error='服务重启，等待重试' WHERE status IN ('queued','running');
CREATE TABLE IF NOT EXISTS cover_tasks (
 id TEXT PRIMARY KEY, job_id TEXT NOT NULL, video_id TEXT NOT NULL,
 owner TEXT NOT NULL, scope TEXT NOT NULL, path TEXT NOT NULL,
 status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '');
UPDATE cover_tasks SET status='interrupted',error='服务重启，等待重试' WHERE status IN ('queued','running');
CREATE UNIQUE INDEX IF NOT EXISTS cover_tasks_active_owner ON cover_tasks(owner) WHERE status IN ('queued','running');
UPDATE jobs SET status='interrupted',message='服务已重启；已完成结果保留，可重试未完成项'
 WHERE status IN ('queued','running','cancelling');
UPDATE videos SET status='error',error='生成被服务重启中断，请重试' WHERE status='generating';`

type Store struct {
	root, data   string
	db           *sql.DB
	mu           sync.Mutex // Serialize new jobs, scans and user edits.
	configMu     sync.RWMutex
	config       Config
	client       *http.Client
	webClient    *http.Client
	coverMu      sync.RWMutex
	coverLimiter *workLimiter
	aiLimiter    *workLimiter                  // Shared by metadata generation and model-powered cover operations.
	jobCancels   map[string]context.CancelFunc // Guarded by mu.
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

func newStore(root, data string) (*Store, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if resolved, e := filepath.EvalSymlinks(root); e == nil {
		root = resolved
	}
	data, err = filepath.Abs(data)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(data, 0750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(data, "library.sqlite3"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{root: root, data: data, db: db, config: Config{Provider: "compatible", JSONMode: true, RetryCount: 2}}
	if b, e := os.ReadFile(filepath.Join(data, "settings.json")); e == nil {
		if err = json.Unmarshal(b, &s.config); err != nil {
			db.Close()
			return nil, err
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		db.Close()
		return nil, e
	}
	if err := s.config.validateConcurrency(); err != nil {
		db.Close()
		return nil, err
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.coverLimiter = newWorkLimiter(s.config.Concurrency)
	s.aiLimiter = newWorkLimiter(s.config.Concurrency)
	s.jobCancels = make(map[string]context.CancelFunc)
	s.client = &http.Client{Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	s.webClient = publicWebClient()
	return s, nil
}
func (s *Store) close() {
	s.mu.Lock()
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
	s.webClient.CloseIdleConnections()
	s.db.Close()
}
func (s *Store) getConfig() Config { s.configMu.RLock(); defer s.configMu.RUnlock(); return s.config }
func (s *Store) ensureIdle() error {
	return s.ensureJobsIdle(false)
}
func (s *Store) ensureJobsIdle(allowCovers bool) error {
	if s.ctx.Err() != nil {
		return userError("服务正在关闭，请稍后重试")
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status IN ('queued','running','cancelling') AND (?=0 OR action<>'covers')", allowCovers).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return userError("已有任务执行中，请等待完成或取消后再操作")
	}
	return nil
}
func (s *Store) getVideo(id string) (Video, error) {
	var v Video
	var raw sql.NullString
	err := s.db.QueryRow("SELECT id,path,size,kind,show_path,season,episode,metadata,status,error,updated FROM videos WHERE id=?", id).
		Scan(&v.ID, &v.Path, &v.Size, &v.Kind, &v.ShowPath, &v.Season, &v.Episode, &raw, &v.Status, &v.Error, &v.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return v, userError("找不到视频记录")
	}
	if err != nil {
		return v, err
	}
	if raw.Valid {
		if err = json.Unmarshal([]byte(raw.String), &v.Metadata); err != nil {
			return v, err
		}
	}
	v.Filename = filepath.Base(v.Path)
	v.Exists = s.videoAvailable(v.Path)
	if p, e := s.safe(nfoPath(v.Path), false); e == nil {
		_, e := os.Stat(p)
		v.HasNFO = e == nil
	}
	if v.ShowPath != nil {
		v.Show, err = s.getShow(*v.ShowPath)
		if err != nil {
			return v, err
		}
		if v.Show == nil {
			name := filepath.Base(*v.ShowPath)
			if name == "." {
				name = filepath.Base(s.root)
			}
			v.Show = &Metadata{Title: name, Genres: []string{}, Tags: []string{}}
		}
	}
	return v, nil
}
func (s *Store) getShow(path string) (*Metadata, error) {
	var raw sql.NullString
	err := s.db.QueryRow("SELECT metadata FROM shows WHERE path=?", path).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !raw.Valid {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m *Metadata
	err = json.Unmarshal([]byte(raw.String), &m)
	return m, err
}
func (s *Store) listVideos() ([]Video, error) {
	rows, err := s.db.Query("SELECT id FROM videos ORDER BY path COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	videos := make([]Video, 0, len(ids))
	for _, id := range ids {
		v, err := s.getVideo(id)
		if err != nil {
			return nil, err
		}
		videos = append(videos, v)
	}
	return videos, nil
}

type EditRequest struct {
	ID       string    `json:"id"`
	Metadata *Metadata `json:"metadata"`
	Season   *int      `json:"season"`
	Episode  *int      `json:"episode"`
	Show     *Metadata `json:"show"`
}

func (s *Store) edit(body EditRequest) (Video, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureIdle(); err != nil {
		return Video{}, err
	}
	v, err := s.getVideo(body.ID)
	if err != nil {
		return v, err
	}
	if err = body.Metadata.validate(); err != nil {
		return v, err
	}
	if v.Kind == "episode" {
		for _, n := range []*int{body.Season, body.Episode} {
			if n != nil && (*n < 0 || *n > 9999) {
				return v, userError("季数、集数必须是 0 到 9999 的整数")
			}
		}
		if err = body.Show.validate(); err != nil {
			return v, err
		}
		v.Season, v.Episode = body.Season, body.Episode
	}
	tx, err := s.db.Begin()
	if err != nil {
		return v, err
	}
	defer tx.Rollback()
	if v.ShowPath != nil {
		if _, err = tx.Exec("UPDATE shows SET metadata=? WHERE path=?", encode(body.Show), *v.ShowPath); err != nil {
			return v, err
		}
	}
	_, err = tx.Exec("UPDATE videos SET metadata=?,season=?,episode=?,status='draft',error='',updated=? WHERE id=?", encode(body.Metadata), v.Season, v.Episode, now(), v.ID)
	if err != nil {
		return v, err
	}
	if err = tx.Commit(); err != nil {
		return v, err
	}
	return s.getVideo(v.ID)
}

type SettingsRequest struct {
	Config
	ClearKey       bool `json:"clear_key"`
	ClearSearchKey bool `json:"clear_search_key"`
}

func (s *Store) saveSettings(body SettingsRequest) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureIdle(); err != nil {
		return nil, err
	}
	c := body.Config
	if err := c.validateConcurrency(); err != nil {
		return nil, err
	}
	if c.Provider != "compatible" && c.Provider != "ollama" {
		return nil, userError("未知 AI 接口类型")
	}
	c.BaseURL = strings.TrimRight(cleanText(c.BaseURL, 2000), "/")
	u, err := url.Parse(c.BaseURL)
	if c.BaseURL != "" && (err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery) {
		return nil, userError("接口地址须为 http(s) 地址，不要包含密钥、查询参数或完整请求路径")
	}
	c.Model = cleanText(c.Model, 200)
	if c.SearchProvider != "" && c.SearchProvider != "model" && c.SearchProvider != "none" && c.SearchProvider != "tavily" && c.SearchProvider != "searxng" {
		return nil, userError("未知搜索接口类型")
	}
	c.SearchURL = strings.TrimRight(cleanText(c.SearchURL, 2000), "/")
	if c.SearchURL == "" && c.SearchProvider == "tavily" {
		c.SearchURL = "https://api.tavily.com"
	}
	if c.SearchURL != "" {
		u, err := url.Parse(c.SearchURL)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
			return nil, userError("搜索接口地址须为不含密钥或查询参数的 http(s) 基础地址")
		}
	}
	if body.ClearSearchKey {
		c.SearchKey = ""
	} else if c.SearchKey == "" {
		c.SearchKey = s.getConfig().SearchKey
	} else {
		c.SearchKey = cleanText(c.SearchKey, 4000)
	}
	if body.ClearKey {
		c.APIKey = ""
	} else if c.APIKey == "" {
		c.APIKey = s.getConfig().APIKey
	} else {
		c.APIKey = cleanText(c.APIKey, 4000)
	}
	if err := atomicJSON(filepath.Join(s.data, "settings.json"), c); err != nil {
		return nil, err
	}
	s.configMu.Lock()
	s.config = c
	s.configMu.Unlock()
	s.coverLimiter.setLimit(c.Concurrency)
	s.aiLimiter.setLimit(c.Concurrency)
	return c.public(), nil
}
func atomicJSON(path string, data any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = json.NewEncoder(f).Encode(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
