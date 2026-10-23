package main

import (
	"fmt"
	"net/url"
	"os"
)

type CoverPreview struct {
	Scope        string `json:"scope"`
	Path         string `json:"path"`
	URL          string `json:"url"`
	ThumbnailURL string `json:"thumbnail_url"`
}

// Listing only stats the sidecars. Image decoding happens lazily when requested.
func (s *Store) coverPreviews(v Video) []CoverPreview {
	previews := []CoverPreview{}
	for _, scope := range []string{"video", "series"} {
		target, err := coverTargetPath(v, scope)
		if err != nil {
			continue
		}
		path, err := s.safe(target.Path, true)
		if err != nil {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxCoverBytes {
			continue
		}
		query := url.Values{"id": {v.ID}, "scope": {scope}, "v": {fmt.Sprintf("%x-%x", info.ModTime().UnixNano(), info.Size())}}
		full := "/api/cover-image?" + query.Encode()
		query.Set("size", "thumb")
		previews = append(previews, CoverPreview{Scope: scope, Path: target.Path, URL: full, ThumbnailURL: "/api/cover-image?" + query.Encode()})
	}
	return previews
}
