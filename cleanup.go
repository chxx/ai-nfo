package main

import (
	"database/sql"
	"errors"
	"io"
	"os"
)

type UnavailableVideo struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}
type CleanupPreview struct {
	Videos []UnavailableVideo `json:"videos"`
	Total  int                `json:"total"`
}
type CleanupRequest struct {
	IDs []string `json:"ids"`
}
type ClearRecordsRequest struct {
	IDs []string `json:"ids"`
	All bool     `json:"all"`
}
type CleanupResult struct {
	Removed int `json:"removed"`
	Skipped int `json:"skipped"`
}

func (s *Store) videoAvailable(rel string) bool {
	p, err := s.safe(rel, false)
	if err != nil {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func (s *Store) checkCleanupRoot() error {
	f, err := os.Open(s.root)
	if err != nil {
		return userError("视频根目录不可访问，请恢复挂载和权限后再清理")
	}
	defer f.Close()
	if _, err = f.ReadDir(1); err != nil && !errors.Is(err, io.EOF) {
		return userError("视频根目录不可访问，请恢复挂载和权限后再清理")
	}
	return nil
}

func (s *Store) unavailableVideos() (CleanupPreview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := CleanupPreview{Videos: []UnavailableVideo{}}
	if err := s.ensureIdle(); err != nil {
		return result, err
	}
	if err := s.checkCleanupRoot(); err != nil {
		return result, err
	}
	rows, err := s.db.Query("SELECT id,path FROM videos ORDER BY path COLLATE NOCASE")
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var v UnavailableVideo
		if err := rows.Scan(&v.ID, &v.Path); err != nil {
			return result, err
		}
		if !s.videoAvailable(v.Path) {
			result.Total++
			if len(result.Videos) < 5000 {
				result.Videos = append(result.Videos, v)
			}
		}
	}
	return result, rows.Err()
}

func (s *Store) cleanupUnavailable(body CleanupRequest) (CleanupResult, error) {
	return s.removeRecords(ClearRecordsRequest{IDs: body.IDs}, true)
}

func (s *Store) clearRecords(body ClearRecordsRequest) (CleanupResult, error) {
	return s.removeRecords(body, false)
}

func (s *Store) removeRecords(body ClearRecordsRequest, unavailableOnly bool) (CleanupResult, error) {
	result := CleanupResult{}
	if body.All && (unavailableOnly || len(body.IDs) != 0) {
		return result, userError("清空全部记录时不能同时指定所选记录")
	}
	if !body.All && (len(body.IDs) == 0 || len(body.IDs) > 5000) {
		return result, userError("请选择 1–5000 条待清理记录")
	}
	// Prevent an in-flight cover lookup from recreating cache entries after deletion.
	if !s.coverMu.TryLock() {
		return result, userError("正在查找海报或截图，请完成后再清除记录")
	}
	defer s.coverMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureIdle(); err != nil {
		return result, err
	}
	if unavailableOnly {
		if err := s.checkCleanupRoot(); err != nil {
			return result, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if body.All {
		if err = tx.QueryRow("SELECT COUNT(*) FROM videos").Scan(&result.Removed); err != nil {
			return CleanupResult{}, err
		}
		for _, table := range []string{"videos", "shows", "cover_candidates", "cover_selections", "cover_assets"} {
			if _, err = tx.Exec("DELETE FROM " + table); err != nil {
				return CleanupResult{}, err
			}
		}
		if err = tx.Commit(); err != nil {
			return CleanupResult{}, err
		}
		return result, nil
	}
	seen, shows := map[string]bool{}, map[string]bool{}
	for _, id := range body.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		var path string
		var show sql.NullString
		err = tx.QueryRow("SELECT path,show_path FROM videos WHERE id=?", id).Scan(&path, &show)
		if errors.Is(err, sql.ErrNoRows) {
			result.Skipped++
			continue
		}
		if err != nil {
			return CleanupResult{}, err
		}
		// Recheck the file at confirmation time; only remove the requested records.
		if unavailableOnly && s.videoAvailable(path) {
			result.Skipped++
			continue
		}
		if _, err = tx.Exec("DELETE FROM videos WHERE id=?", id); err != nil {
			return CleanupResult{}, err
		}
		for _, table := range []string{"cover_candidates", "cover_selections", "cover_assets"} {
			if _, err = tx.Exec("DELETE FROM "+table+" WHERE owner=?", "video:"+path); err != nil {
				return CleanupResult{}, err
			}
		}
		result.Removed++
		if show.Valid {
			shows[show.String] = true
		}
	}
	for path := range shows {
		for _, table := range []string{"cover_candidates", "cover_selections", "cover_assets"} {
			if _, err = tx.Exec("DELETE FROM "+table+" WHERE owner=? AND NOT EXISTS (SELECT 1 FROM videos WHERE show_path=?)", "show:"+path, path); err != nil {
				return CleanupResult{}, err
			}
		}
		if _, err = tx.Exec("DELETE FROM shows WHERE path=? AND NOT EXISTS (SELECT 1 FROM videos WHERE show_path=?)", path, path); err != nil {
			return CleanupResult{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return CleanupResult{}, err
	}
	return result, nil
}
