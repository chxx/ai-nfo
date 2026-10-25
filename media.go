package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

var videoExtensions = map[string]bool{".mp4": true, ".mkv": true, ".avi": true, ".mov": true, ".m4v": true, ".webm": true, ".ts": true, ".flv": true, ".wmv": true}
var episodePattern = regexp.MustCompile(`(?i)S(\d{1,3})E(\d{1,4})(?:[^0-9]|$)`)
var seasonDirectoryPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(?:season[\s._-]*|s[\s._-]*)(\d{1,3})(?:\s*[._-].*)?$`),
	regexp.MustCompile(`^第\s*(\d{1,3})\s*季(?:\s*[._-].*)?$`),
}
var numberPattern = regexp.MustCompile(`^(?:第\s*)?(\d{1,4})(?:[集课讲章._\s-]|$)`)
var multiPattern = regexp.MustCompile(`(?i)E\d+`)
var digitsPattern = regexp.MustCompile(`^\d+$`)

func nfoPath(path string) string { return strings.TrimSuffix(path, filepath.Ext(path)) + ".nfo" }

type NFOViewFile struct {
	Path   string `json:"path"`
	XML    string `json:"xml"`
	Exists bool   `json:"exists"`
}

type NFOEditRequest struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	XML  string `json:"xml"`
}

func nfoPaths(v Video) []string {
	paths := []string{}
	if v.Kind == "episode" && v.ShowPath != nil {
		paths = append(paths, filepath.ToSlash(filepath.Join(*v.ShowPath, "tvshow.nfo")))
	}
	return append(paths, nfoPath(v.Path))
}

func (s *Store) nfoFiles(id string) (map[string]any, error) {
	v, err := s.getVideo(id)
	if err != nil {
		return nil, err
	}
	files := make([]NFOViewFile, 0, 2)
	for _, rel := range nfoPaths(v) {
		path, err := s.safe(rel, false)
		if err != nil {
			return nil, err
		}
		raw, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			files = append(files, NFOViewFile{Path: rel})
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		if len(raw) > 2<<20 {
			return nil, userError("NFO 文件超过 2 MB，暂不支持在网页编辑")
		}
		files = append(files, NFOViewFile{Path: rel, XML: string(raw), Exists: true})
	}
	return map[string]any{"id": id, "files": files}, nil
}

func (s *Store) saveNFO(body NFOEditRequest) (map[string]any, error) {
	if strings.TrimSpace(body.XML) == "" {
		return nil, userError("NFO 内容不能为空")
	}
	if len([]byte(body.XML)) > 2<<20 {
		return nil, userError("NFO 文件超过 2 MB")
	}
	var root struct{ XMLName xml.Name }
	if err := xml.Unmarshal([]byte(body.XML), &root); err != nil {
		return nil, userError("NFO XML 格式错误：" + err.Error())
	}
	switch root.XMLName.Local {
	case "movie", "tvshow", "episodedetails":
	default:
		return nil, userError("NFO 根节点必须是 movie、tvshow 或 episodedetails")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureIdle(); err != nil {
		return nil, err
	}
	v, err := s.getVideo(body.ID)
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, path := range nfoPaths(v) {
		if path == filepath.ToSlash(body.Path) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, userError("只能编辑当前视频或所属剧集的 NFO 文件")
	}
	expectedRoot := "movie"
	if v.Kind == "episode" {
		if v.ShowPath != nil && filepath.ToSlash(body.Path) == filepath.ToSlash(filepath.Join(*v.ShowPath, "tvshow.nfo")) {
			expectedRoot = "tvshow"
		} else {
			expectedRoot = "episodedetails"
		}
	}
	if root.XMLName.Local != expectedRoot {
		return nil, userError("NFO 根节点与文件类型不匹配，应为 " + expectedRoot)
	}
	wrote, err := s.writeNFO(body.Path, body.XML, true)
	if err != nil {
		return nil, err
	}
	return map[string]any{"saved": wrote, "path": body.Path}, nil
}
func (s *Store) safe(rel string, mustExist bool) (string, error) {
	if filepath.IsAbs(rel) || strings.ContainsRune(rel, 0) {
		return "", userError("目录必须位于已挂载的视频根目录内")
	}
	p := s.root
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".." {
			return "", userError("目录越过视频根目录")
		}
		if part == "" || part == "." {
			continue
		}
		p = filepath.Join(p, part)
		info, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", userError("不访问符号链接，请直接挂载目标目录")
		}
	}
	if mustExist {
		if _, err := os.Stat(p); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", userError("文件或目录不存在；请检查 NAS 挂载")
			}
			return "", err
		}
	}
	return p, nil
}
func (s *Store) relative(path string) string {
	p, _ := filepath.Rel(s.root, path)
	return filepath.ToSlash(p)
}

type Directory struct {
	Name string `json:"name"`
	Path string `json:"path"`
}
type DirectoryList struct {
	Path        string      `json:"path"`
	Directories []Directory `json:"directories"`
}

func (s *Store) directories(rel string) (DirectoryList, error) {
	result := DirectoryList{Directories: []Directory{}}
	p, err := s.safe(rel, true)
	if err != nil {
		return result, err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return result, err
	}
	result.Path = s.relative(p)
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && entry.Type()&os.ModeSymlink == 0 {
			result.Directories = append(result.Directories, Directory{entry.Name(), s.relative(filepath.Join(p, entry.Name()))})
		}
	}
	sort.Slice(result.Directories, func(i, j int) bool {
		return strings.ToLower(result.Directories[i].Name) < strings.ToLower(result.Directories[j].Name)
	})
	if len(result.Directories) > 1000 {
		result.Directories = result.Directories[:1000]
	}
	return result, nil
}

type ScanRequest struct {
	Path             string `json:"path"`
	Kind             string `json:"kind"`
	Recursive        bool   `json:"recursive"`
	GroupByDirectory bool   `json:"group_by_directory"`
}
type ScanResult struct {
	Count        int      `json:"count"`
	Reclassified int      `json:"reclassified"`
	IDs          []string `json:"ids"`
	Warnings     []string `json:"warnings"`
}

func intPointer(v int) *int     { return &v }
func parseNumber(s string) *int { v, _ := strconv.Atoi(s); return &v }

// seasonDirectory recognizes the common season folder spellings without
// mistaking numbered lesson folders (for example "01 Alphabet") for a season.
// The season folder may contain another level of folders, so callers walk its
// ancestors instead of checking only the video's direct parent.
func seasonDirectory(name string) (int, bool) {
	name = strings.TrimSpace(name)
	for _, pattern := range seasonDirectoryPatterns {
		if match := pattern.FindStringSubmatch(name); match != nil {
			season, _ := strconv.Atoi(match[1])
			return season, true
		}
	}
	return 0, false
}

func (s *Store) seasonAncestor(path, stop string) (string, int, bool) {
	dir := filepath.Dir(path)
	stopPath := stop
	if stopPath != "" {
		if p, err := s.safe(stopPath, false); err == nil {
			stopPath = p
		}
	}
	for {
		if season, ok := seasonDirectory(filepath.Base(dir)); ok {
			return dir, season, true
		}
		if dir == s.root || stopPath != "" && dir == stopPath {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", 0, false
}

func (s *Store) seriesPathForVideo(path, scanRoot string, groupByDirectory bool) (string, int, bool) {
	if !groupByDirectory {
		return "", 0, false
	}
	if seasonDir, season, ok := s.seasonAncestor(path, scanRoot); ok {
		return s.relative(filepath.Dir(seasonDir)), season, true
	}
	return s.relative(filepath.Dir(path)), 0, false
}

// moveSeasonSidecars repairs the layout produced by older versions that put
// the show files inside Season 01. Only the well-known Kodi/Zidoo sidecars are
// considered; episode files and episode NFOs stay in their season directory.
func (s *Store) moveSeasonSidecars(seasonDir, showDir string, season int) error {
	if filepath.Clean(seasonDir) == filepath.Clean(showDir) {
		return nil
	}
	names := []string{"tvshow.nfo", "poster.jpg", "fanart.jpg"}
	if season > 0 {
		names = append(names, fmt.Sprintf("season%02d-poster.jpg", season), fmt.Sprintf("season%02d-fanart.jpg", season))
	}
	for _, name := range names {
		source, target := filepath.Join(seasonDir, name), filepath.Join(showDir, name)
		sourceInfo, err := os.Lstat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.Mode().IsRegular() {
			continue
		}
		targetInfo, targetErr := os.Lstat(target)
		if errors.Is(targetErr, os.ErrNotExist) {
			if err = os.Rename(source, target); err != nil {
				return err
			}
			continue
		}
		if targetErr != nil || targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.Mode().IsRegular() {
			continue
		}
		// If both copies are identical, remove only the stale season copy.
		sourceBytes, readSourceErr := os.ReadFile(source)
		targetBytes, readTargetErr := os.ReadFile(target)
		if readSourceErr == nil && readTargetErr == nil && bytes.Equal(sourceBytes, targetBytes) {
			if err = os.Remove(source); err != nil {
				return err
			}
		}
	}
	return nil
}

// repairSeasonRecords upgrades records created before season folders were
// treated as part of their parent show. It also moves old show-level sidecars
// to that parent so a rescan/restart does not leave Zidoo seeing two shows.
func (s *Store) repairSeasonRecords() error {
	rows, err := s.db.Query("SELECT id,path,show_path,season FROM videos WHERE kind='episode' AND show_path IS NOT NULL")
	if err != nil {
		return err
	}
	type record struct {
		id, path, oldShow string
		season            sql.NullInt64
	}
	records := []record{}
	for rows.Next() {
		var r record
		if err = rows.Scan(&r.id, &r.path, &r.oldShow, &r.season); err != nil {
			rows.Close()
			return err
		}
		records = append(records, r)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	type change struct {
		record
		newShow, seasonDir string
		season             int
		updateSeason       bool
	}
	changes := []change{}
	for _, r := range records {
		seasonDir, season, ok := s.seasonAncestor(filepath.Join(s.root, filepath.FromSlash(r.path)), "")
		if !ok {
			continue
		}
		newShow := s.relative(filepath.Dir(seasonDir))
		stem := strings.TrimSuffix(filepath.Base(r.path), filepath.Ext(r.path))
		updateSeason := !episodePattern.MatchString(stem) && (!r.season.Valid || int(r.season.Int64) != season)
		if filepath.ToSlash(newShow) == filepath.ToSlash(r.oldShow) && !updateSeason {
			continue
		}
		changes = append(changes, change{record: r, newShow: newShow, seasonDir: seasonDir, season: season, updateSeason: updateSeason})
	}
	repairArtwork := func() {
		for _, r := range records {
			v, getErr := s.getVideo(r.id)
			if getErr != nil || v.ShowPath == nil {
				continue
			}
			seasonDir, season, ok := s.seasonAncestor(filepath.Join(s.root, filepath.FromSlash(r.path)), "")
			if !ok {
				continue
			}
			_ = s.moveSeasonSidecars(seasonDir, filepath.Join(s.root, filepath.FromSlash(*v.ShowPath)), season)
			_ = s.ensureSeasonArtworkAliases(v)
		}
	}
	if len(changes) == 0 {
		repairArtwork()
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range changes {
		if _, err = tx.Exec("INSERT OR IGNORE INTO shows(path,metadata) VALUES(?,(SELECT metadata FROM shows WHERE path=?))", c.newShow, c.oldShow); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE videos SET show_path=? WHERE id=?", c.newShow, c.id); err != nil {
			return err
		}
		if c.updateSeason {
			if _, err = tx.Exec("UPDATE videos SET season=? WHERE id=?", c.season, c.id); err != nil {
				return err
			}
		}
		newOwner, oldOwner := "show:"+c.newShow, "show:"+c.oldShow
		newPoster := filepath.ToSlash(filepath.Join(c.newShow, "poster.jpg"))
		var image []byte
		var updated float64
		if readErr := tx.QueryRow("SELECT image,updated FROM cover_assets WHERE owner=?", oldOwner).Scan(&image, &updated); readErr == nil {
			if _, err = tx.Exec("INSERT OR IGNORE INTO cover_assets(owner,target,image,updated) VALUES(?,?,?,?)", newOwner, newPoster, image, updated); err != nil {
				return err
			}
		}
		var metadata string
		if readErr := tx.QueryRow("SELECT metadata,updated FROM cover_selections WHERE owner=?", oldOwner).Scan(&metadata, &updated); readErr == nil {
			if _, err = tx.Exec("INSERT OR IGNORE INTO cover_selections(owner,target,metadata,updated) VALUES(?,?,?,?)", newOwner, newPoster, metadata, updated); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, c := range changes {
		// A read-only or partially mounted NAS must not prevent the service from
		// starting; the next scan/save will report the sidecar repair failure.
		_ = s.moveSeasonSidecars(c.seasonDir, filepath.Join(s.root, filepath.FromSlash(c.newShow)), c.season)
		if v, getErr := s.getVideo(c.id); getErr == nil {
			_ = s.ensureSeasonArtworkAliases(v)
		}
	}
	// Also repair records that were already normalized by an earlier version:
	// their root poster may exist while season aliases were never written.
	repairArtwork()
	return nil
}

func (s *Store) scan(body ScanRequest) (ScanResult, error) {
	result := ScanResult{IDs: []string{}, Warnings: []string{}}
	if body.Kind != "movie" && body.Kind != "episode" {
		return result, userError("请选择独立视频或剧集教程")
	}
	start, err := s.safe(body.Path, true)
	if err != nil {
		return result, err
	}
	info, err := os.Stat(start)
	if err != nil {
		return result, err
	}
	if !info.IsDir() {
		return result, userError("请选择目录")
	}
	showPath := s.relative(start)
	found, dirs := []Video{}, 0
	warn := func(message string) {
		if len(result.Warnings) < 20 {
			result.Warnings = append(result.Warnings, message)
		}
	}
	err = filepath.WalkDir(start, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == start {
				return walkErr
			}
			warn("无法读取：" + filepath.Base(path))
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			if path != start && (!body.Recursive || strings.HasPrefix(entry.Name(), ".")) {
				return filepath.SkipDir
			}
			dirs++
			if dirs > 20000 {
				return userError("目录过大，请选择更具体的子目录")
			}
			return nil
		}
		if !videoExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		rel := s.relative(path)
		if _, err := s.safe(rel, true); err != nil {
			warn("无法读取：" + entry.Name())
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			warn("无法读取：" + entry.Name())
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		stem := strings.TrimSuffix(entry.Name(), filepath.Ext(path))
		v := Video{ID: identifier(rel), Path: rel, Size: info.Size(), Kind: body.Kind, Updated: now()}
		if body.Kind == "episode" {
			if len(multiPattern.FindAllString(stem, -1)) > 1 {
				warn("多集合并文件需手动拆分元数据，已跳过：" + entry.Name())
				return nil
			}
			seriesPath := showPath
			seasonDir, seasonFromDirectory, hasSeasonDirectory := s.seasonAncestor(path, start)
			if body.GroupByDirectory {
				seriesPath, _, _ = s.seriesPathForVideo(path, start, true)
			} else if hasSeasonDirectory && filepath.Clean(seasonDir) == filepath.Clean(start) {
				// Even when the user scans one season folder as a whole, keep
				// tvshow.nfo and show artwork in the parent show directory.
				seriesPath = s.relative(filepath.Dir(seasonDir))
			}
			v.ShowPath = &seriesPath
			if m := episodePattern.FindStringSubmatch(stem); m != nil {
				v.Season, v.Episode = parseNumber(m[1]), parseNumber(m[2])
			} else {
				v.Season = intPointer(max(1, seasonFromDirectory))
				if m := numberPattern.FindStringSubmatch(stem); m != nil {
					v.Episode = parseNumber(m[1])
				}
			}
		}
		found = append(found, v)
		if len(found) > 5000 {
			return userError("单次最多扫描 5000 个视频，请选择更具体的目录")
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.ensureIdle(); err != nil {
		return result, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	for _, v := range found {
		if v.ShowPath != nil {
			if _, err = tx.Exec("INSERT OR IGNORE INTO shows(path) VALUES (?)", *v.ShowPath); err != nil {
				return result, err
			}
		}
		var changed int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM videos WHERE path=? AND
 (kind<>? OR show_path IS NOT ?)`, v.Path, v.Kind, v.ShowPath).Scan(&changed); err != nil {
			return result, err
		}
		_, err = tx.Exec(`INSERT INTO videos(id,path,size,kind,show_path,season,episode,updated) VALUES(?,?,?,?,?,?,?,?)
 ON CONFLICT(path) DO UPDATE SET
 size=excluded.size,
 kind=excluded.kind,
 show_path=excluded.show_path,
 season=CASE WHEN videos.kind<>excluded.kind OR videos.show_path IS NOT excluded.show_path THEN excluded.season ELSE videos.season END,
 episode=CASE WHEN videos.kind<>excluded.kind OR videos.show_path IS NOT excluded.show_path THEN excluded.episode ELSE videos.episode END,
 status=CASE WHEN videos.kind<>excluded.kind OR videos.show_path IS NOT excluded.show_path THEN CASE WHEN videos.metadata IS NULL THEN 'new' ELSE 'draft' END ELSE videos.status END,
 error=CASE WHEN videos.kind<>excluded.kind OR videos.show_path IS NOT excluded.show_path THEN '' ELSE videos.error END,
 updated=excluded.updated`, v.ID, v.Path, v.Size, v.Kind, v.ShowPath, v.Season, v.Episode, v.Updated)
		if err != nil {
			return result, err
		}
		result.IDs = append(result.IDs, v.ID)
		result.Reclassified += changed
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	for _, v := range found {
		if v.Kind != "episode" || v.ShowPath == nil {
			continue
		}
		seasonDir, season, ok := s.seasonAncestor(filepath.Join(s.root, filepath.FromSlash(v.Path)), start)
		if !ok {
			continue
		}
		if err = s.moveSeasonSidecars(seasonDir, filepath.Join(s.root, filepath.FromSlash(*v.ShowPath)), season); err != nil {
			warn("无法整理剧集海报或 tvshow.nfo：" + filepath.Base(seasonDir))
		}
		if err = s.ensureSeasonArtworkAliases(v); err != nil {
			warn("无法补齐季海报：" + filepath.Base(filepath.Dir(v.Path)))
		}
	}
	result.Count = len(found)
	return result, nil
}

type Source struct {
	File      string `json:"file"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}
type Evidence struct {
	Filename  string            `json:"filename"`
	Folder    string            `json:"folder"`
	Directory string            `json:"directory"`
	Kind      string            `json:"kind"`
	Sources   []Source          `json:"sources"`
	Evidence  string            `json:"evidence"`
	Embedded  map[string]string `json:"embedded,omitempty"`
	Warnings  []string          `json:"warnings,omitempty"`
}

func (s *Store) evidence(v Video) (Evidence, error) {
	result := Evidence{Filename: v.Filename, Kind: v.Kind, Sources: []Source{}, Evidence: "仅文件名和目录；不得扩写具体内容"}
	p, err := s.safe(v.Path, true)
	if err != nil {
		return result, err
	}
	result.Folder = filepath.Base(filepath.Dir(p))
	result.Directory = s.relative(filepath.Dir(p))
	if probe, err := probeEmbedded(s.ctx, p); err != nil {
		result.Warnings = append(result.Warnings, publicError(err))
	} else {
		result.Embedded = probe.metadata()
	}
	stem := strings.TrimSuffix(p, filepath.Ext(p))
	candidates := []string{}
	for _, ext := range []string{".srt", ".vtt", ".txt", ".description", ".info.json"} {
		candidates = append(candidates, stem+ext)
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, filepath.Base(stem)+".") && (strings.HasSuffix(name, ".srt") || strings.HasSuffix(name, ".vtt")) {
			candidates = append(candidates, filepath.Join(filepath.Dir(p), name))
		}
	}
	for _, name := range []string{"README.md", "简介.txt", "description.txt"} {
		candidates = append(candidates, filepath.Join(filepath.Dir(p), name))
	}
	remaining, seen := 24000, map[string]bool{}
	for _, path := range candidates {
		if seen[path] || remaining <= 0 {
			continue
		}
		seen[path] = true
		if _, err := s.safe(s.relative(path), true); err != nil {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(f, 160000))
		f.Close()
		if err != nil {
			continue
		}
		text := strings.TrimPrefix(string(raw), "\uFEFF")
		if !utf8.Valid(raw) {
			decoded, e := simplifiedchinese.GB18030.NewDecoder().Bytes(raw)
			if e != nil {
				continue
			}
			text = string(decoded)
		}
		if filepath.Ext(path) == ".json" {
			var obj map[string]any
			if json.Unmarshal([]byte(text), &obj) != nil || obj == nil {
				continue
			}
			selected := map[string]any{}
			for _, key := range []string{"title", "description", "uploader", "upload_date"} {
				if value, ok := obj[key]; ok {
					selected[key] = value
				}
			}
			text = encode(selected)
		}
		lines := []string{}
		for _, line := range strings.Split(text, "\n") {
			if !digitsPattern.MatchString(strings.TrimSpace(line)) && !strings.Contains(line, "-->") {
				lines = append(lines, line)
			}
		}
		runes := []rune(strings.TrimSpace(strings.Join(lines, "\n")))
		if len(runes) == 0 {
			continue
		}
		sample := runes[:min(remaining, len(runes))]
		result.Sources = append(result.Sources, Source{filepath.Base(path), string(sample), len(sample) < len(runes) || len(raw) == 160000})
		remaining -= len(sample)
	}
	if len(result.Sources) > 0 {
		result.Evidence = "字幕或简介（可能截取）"
	}
	if len(result.Embedded) > 0 {
		result.Evidence = "含文件内嵌标签；结合目录和外置资料核对，标签可能缺失或不准确，不代表已识别视频内容"
	}
	return result, nil
}
func (s *Store) preview(id string) ([]NFOFile, error) {
	v, err := s.getVideo(id)
	if err != nil {
		return nil, err
	}
	return s.previewVideo(v)
}

func (s *Store) previewEdit(body EditRequest) ([]NFOFile, error) {
	v, err := s.getVideo(body.ID)
	if err != nil {
		return nil, err
	}
	if err = body.Metadata.validate(); err != nil {
		return nil, err
	}
	v.Metadata = body.Metadata
	if v.Kind == "episode" {
		for _, n := range []*int{body.Season, body.Episode} {
			if n != nil && (*n < 0 || *n > 9999) {
				return nil, userError("季数、集数必须是 0 到 9999 的整数")
			}
		}
		if err = body.Show.validate(); err != nil {
			return nil, err
		}
		v.Season, v.Episode, v.Show = body.Season, body.Episode, body.Show
	}
	return s.previewVideo(v)
}

func (s *Store) previewVideo(v Video) ([]NFOFile, error) {
	if v.Metadata == nil {
		return nil, userError("请先生成或编辑元数据")
	}
	if err := s.checkVideoStem(v.Path); err != nil {
		return nil, err
	}
	files, kind, showTitle := []NFOFile{}, "movie", ""
	if v.Kind == "episode" {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM videos WHERE show_path=? AND season=? AND episode=? AND id<>?", v.ShowPath, v.Season, v.Episode, v.ID).Scan(&count); err != nil {
			return nil, err
		}
		if count > 0 {
			return nil, userError("同剧集存在重复季数和集数，请先调整分集信息")
		}
		if v.ShowPath == nil || v.Show == nil {
			return nil, userError("剧集信息缺失，请检查草稿")
		}
		text, err := nfoXML("tvshow", v.Show, identifier("show:"+*v.ShowPath), nil, nil, "", s.coverThumbnail(v, "series"), s.coverThumbnail(v, "series-fanart"))
		if err != nil {
			return nil, err
		}
		files = append(files, NFOFile{filepath.ToSlash(filepath.Join(*v.ShowPath, "tvshow.nfo")), text})
		kind, showTitle = "episodedetails", v.Show.Title
	}
	text, err := nfoXML(kind, v.Metadata, v.ID, v.Season, v.Episode, showTitle, s.coverThumbnail(v, "video"), s.coverThumbnail(v, "video-fanart"))
	if err != nil {
		return nil, err
	}
	return append(files, NFOFile{nfoPath(v.Path), text}), nil
}
func (s *Store) checkVideoStem(rel string) error {
	p, err := s.safe(rel, true)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		return err
	}
	stem := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
	for _, entry := range entries {
		name := entry.Name()
		if name != filepath.Base(p) && !entry.IsDir() && strings.TrimSuffix(name, filepath.Ext(name)) == stem && videoExtensions[strings.ToLower(filepath.Ext(name))] {
			return userError("同目录有同名不同扩展名的视频，会共用 NFO 或海报；请先区分文件名")
		}
	}
	return nil
}
func (s *Store) writeNFO(rel, text string, overwrite bool) (bool, error) {
	return s.writeFile(rel, []byte(text), overwrite)
}
func (s *Store) writeFile(rel string, data []byte, overwrite bool) (bool, error) {
	target, err := s.safe(rel, false)
	if err != nil {
		return false, err
	}
	info, statErr := os.Stat(target)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	if statErr == nil && !overwrite {
		return false, nil
	}
	if statErr == nil && !info.Mode().IsRegular() {
		return false, userError("保存目标不是普通文件")
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".ai-nfo-*.tmp")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	mode := os.FileMode(0644)
	if statErr == nil {
		mode = info.Mode().Perm()
	}
	if err = f.Chmod(mode); err != nil {
		return false, err
	}
	if _, err = f.Write(data); err != nil {
		return false, err
	}
	if err = f.Sync(); err != nil {
		return false, err
	}
	if err = f.Close(); err != nil {
		return false, err
	}
	if _, err = s.safe(rel, false); err != nil {
		return false, err
	}
	if overwrite {
		err = os.Rename(f.Name(), target)
	} else {
		err = os.Link(f.Name(), target)
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
	}
	return err == nil, err
}
