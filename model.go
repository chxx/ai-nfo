package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"strings"
	"time"
)

type userError string

func (e userError) Error() string { return string(e) }

func publicError(err error) string {
	var e userError
	if errors.As(err, &e) {
		return e.Error()
	}
	return "文件或数据操作失败，请检查挂载、读写权限和剩余空间"
}

func now() float64 { return float64(time.Now().UnixMilli()) / 1000 }
func identifier(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}
func randomID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func cleanText(s string, limit int) string {
	runes := []rune(strings.TrimSpace(s))
	out := make([]rune, 0, min(len(runes), limit))
	for _, c := range runes {
		if c == '\n' || c == '\r' || c == '\t' || c >= 32 && c <= 0xD7FF || c >= 0xE000 && c <= 0xFFFD || c >= 0x10000 && c <= 0x10FFFF {
			out = append(out, c)
			if len(out) == limit {
				break
			}
		}
	}
	return string(out)
}

type Metadata struct {
	Title     string   `json:"title"`
	Plot      string   `json:"plot"`
	Directors []string `json:"directors"`
	Genres    []string `json:"genres"`
	Tags      []string `json:"tags"`
	Notes     string   `json:"notes"`
}

func (m *Metadata) validate() error {
	if m == nil {
		return userError("请先生成或编辑元数据")
	}
	m.Title, m.Plot, m.Notes = cleanText(m.Title, 300), cleanText(m.Plot, 12000), cleanText(m.Notes, 12000)
	if m.Title == "" {
		return userError("标题不能为空")
	}
	normalize := func(values []string) []string {
		result, seen := []string{}, map[string]bool{}
		for _, v := range values {
			v = cleanText(v, 80)
			if v != "" && !seen[v] {
				result = append(result, v)
				seen[v] = true
			}
			if len(result) == 20 {
				break
			}
		}
		return result
	}
	m.Genres, m.Tags = normalize(m.Genres), normalize(m.Tags)
	m.Directors = normalize(m.Directors)
	return nil
}
func encode(value any) string { b, _ := json.Marshal(value); return string(b) }

type Video struct {
	ID       string    `json:"id"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Kind     string    `json:"kind"`
	ShowPath *string   `json:"show_path"`
	Season   *int      `json:"season"`
	Episode  *int      `json:"episode"`
	Metadata *Metadata `json:"metadata"`
	Status   string    `json:"status"`
	Error    string    `json:"error"`
	Updated  float64   `json:"updated"`
	Filename string    `json:"filename"`
	Exists   bool      `json:"exists"`
	HasNFO   bool      `json:"has_nfo"`
	Show     *Metadata `json:"show,omitempty"`
}
type Job struct {
	ID      string  `json:"id"`
	Action  string  `json:"action"`
	Status  string  `json:"status"`
	Total   int     `json:"total"`
	Done    int     `json:"done"`
	Failed  int     `json:"failed"`
	Skipped int     `json:"skipped"`
	Message string  `json:"message"`
	Created float64 `json:"created"`
}
type Config struct {
	Concurrency    int    `json:"concurrency"`
	Provider       string `json:"provider"`
	BaseURL        string `json:"base_url"`
	Model          string `json:"model"`
	APIKey         string `json:"api_key"`
	JSONMode       bool   `json:"json_mode"`
	SearchProvider string `json:"search_provider"`
	SearchURL      string `json:"search_url"`
	SearchKey      string `json:"search_key"`
}

func (c Config) public() map[string]any {
	return map[string]any{"concurrency": c.Concurrency, "provider": c.Provider, "base_url": c.BaseURL, "model": c.Model, "json_mode": c.JSONMode, "has_key": c.APIKey != "", "search_provider": c.SearchProvider, "search_url": c.SearchURL, "has_search_key": c.SearchKey != ""}
}

type NFOFile struct {
	Path string `json:"path"`
	XML  string `json:"xml"`
}
type nfoID struct {
	Type    string `xml:"type,attr"`
	Default string `xml:"default,attr"`
	Value   string `xml:",chardata"`
}
type nfoDocument struct {
	XMLName   xml.Name
	Title     string   `xml:"title"`
	Plot      string   `xml:"plot"`
	Directors []string `xml:"director,omitempty"`
	ID        nfoID    `xml:"uniqueid"`
	Genres    []string `xml:"genre"`
	Tags      []string `xml:"tag"`
	ShowTitle string   `xml:"showtitle,omitempty"`
	Season    *int     `xml:"season,omitempty"`
	Episode   *int     `xml:"episode,omitempty"`
	Thumb     string   `xml:"thumb,omitempty"`
}

func nfoXML(kind string, m *Metadata, id string, season, episode *int, show, thumb string) (string, error) {
	if m == nil {
		return "", userError("请先生成或编辑元数据")
	}
	doc := nfoDocument{XMLName: xml.Name{Local: kind}, Title: m.Title, Plot: m.Plot, Directors: m.Directors, ID: nfoID{"local", "true", id}, Genres: m.Genres, Tags: m.Tags}
	doc.Thumb = thumb
	if kind == "episodedetails" {
		if season == nil || episode == nil {
			return "", userError("请先填写季数和集数，再预览或保存剧集 NFO")
		}
		doc.Season, doc.Episode, doc.ShowTitle = season, episode, show
	}
	b, err := xml.MarshalIndent(doc, "", "  ")
	return xml.Header + string(b) + "\n", err
}
