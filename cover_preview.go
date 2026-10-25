package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
)

type CoverPreview struct {
	Scope        string `json:"scope"`
	Path         string `json:"path"`
	URL          string `json:"url"`
	ThumbnailURL string `json:"thumbnail_url"`
}

type coverAsset struct {
	Image   []byte
	Updated float64
}

// Read the database copy first. Existing sidecars are imported lazily once so
// frontend image requests no longer depend on the media mount being online.
func (s *Store) loadCoverAsset(target coverTarget) ([]byte, float64, error) {
	var asset coverAsset
	err := s.db.QueryRow("SELECT image,updated FROM cover_assets WHERE owner=? AND target=?", target.Owner, target.Path).Scan(&asset.Image, &asset.Updated)
	if err == nil {
		return asset.Image, asset.Updated, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, 0, err
	}
	path, err := s.safe(target.Path, true)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxCoverBytes {
		if err == nil {
			err = userError("海报文件不可读取")
		}
		return nil, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxCoverBytes+1))
	if err != nil {
		return nil, 0, err
	}
	updated := now()
	if _, err = s.db.Exec("INSERT OR IGNORE INTO cover_assets(owner,target,image,updated) VALUES(?,?,?,?)", target.Owner, target.Path, raw, updated); err != nil {
		return nil, 0, err
	}
	return raw, updated, nil
}

func (s *Store) coverPreviews(v Video) []CoverPreview {
	previews := []CoverPreview{}
	for _, scope := range []string{"video", "series"} {
		target, err := coverTargetPath(v, scope)
		if err != nil {
			continue
		}
		image, updated, err := s.loadCoverAsset(target)
		if err != nil || len(image) == 0 || len(image) > maxCoverBytes {
			continue
		}
		query := url.Values{"id": {v.ID}, "scope": {scope}, "v": {fmt.Sprintf("%x-%x", int64(updated*1000000), len(image))}}
		full := "/api/cover-image?" + query.Encode()
		query.Set("size", "thumb")
		previews = append(previews, CoverPreview{Scope: scope, Path: target.Path, URL: full, ThumbnailURL: "/api/cover-image?" + query.Encode()})
	}
	return previews
}
