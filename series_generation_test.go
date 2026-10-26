package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readGenerationInput(t *testing.T, r *http.Request) seriesGenerationInput {
	t.Helper()
	var body struct{ Messages []struct{ Content string } }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 {
		t.Error("invalid request", err)
		return seriesGenerationInput{}
	}
	var input seriesGenerationInput
	if err := json.Unmarshal([]byte(body.Messages[1].Content), &input); err != nil {
		t.Error(err)
	}
	return input
}

func generationFixture(input seriesGenerationInput) seriesGenerationResponse {
	m := Metadata{Title: "生成标题", Plot: "仅根据提供资料", Directors: []string{"导演甲", "导演乙"}, Genres: []string{"教学"}, Tags: []string{"入门"}}
	response := seriesGenerationResponse{Series: &m}
	for _, entry := range input.Episodes {
		number := entry.episodeNumber
		if number.Season == nil {
			number.Season = intPointer(1)
		}
		if number.Episode == nil {
			for i, sibling := range input.AllEpisodes {
				if sibling.ID == entry.ID {
					number.Episode = intPointer(i + 1)
				}
			}
		}
		response.Episodes = append(response.Episodes, generatedEpisode{number, m})
	}
	return response
}

func generationModel(t *testing.T, s *Store, respond func(seriesGenerationInput) string) *atomic.Int32 {
	t.Helper()
	configure(t, s, "https://ai.example", "compatible")
	calls := &atomic.Int32{}
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		input := readGenerationInput(t, r)
		if input.Kind != "series_batch" || input.SeriesTitle == "" || input.Directory == "" {
			t.Error("missing series context", input)
		}
		content := respond(input)
		return mockResponse(r, 200, "application/json", []byte(encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}}))), nil
	})
	return calls
}

func TestUnifiedGenerationPreservesEditsAndSelection(t *testing.T) {
	s := testStore(t)
	paths := []string{"教程/基础/第一讲.mp4", "教程/进阶/第二讲.mp4", "教程/未选/第三讲.mp4", "教程/S00E00.mp4"}
	for _, path := range paths {
		put(t, s, path, "")
	}
	scan(t, s, "教程", "episode")
	put(t, s, "另一剧集/一.mp4", "")
	scan(t, s, "另一剧集", "episode")
	selected := []string{identifier(paths[0]), identifier(paths[1]), identifier(paths[3])}
	draft(t, s, selected[0])
	v, _ := s.getVideo(selected[0])
	v.Metadata.Plot = ""
	v.Metadata.Tags = []string{"手动标签"}
	if _, err := s.edit(EditRequest{ID: v.ID, Metadata: v.Metadata, Show: v.Show, Season: v.Season, Episode: v.Episode}); err != nil {
		t.Fatal(err)
	}
	put(t, s, "教程/基础/第一讲.txt", "第一集独有内容")
	put(t, s, "教程/进阶/第二讲.txt", "第二集独有内容")
	put(t, s, "教程/未选/第三讲.txt", "不得发送未选集正文")
	calls := generationModel(t, s, func(input seriesGenerationInput) string {
		if len(input.AllEpisodes) != 4 || len(input.Episodes) != 3 || input.SeriesTitle != "系列 & 总览" {
			t.Error("wrong context", input)
		}
		if strings.Contains(encode(input), "不得发送") || strings.Contains(encode(input), "另一剧集") {
			t.Error("unselected evidence leaked")
		}
		for _, entry := range input.Episodes {
			if entry.ID == selected[0] && (!strings.Contains(encode(entry.Evidence), "第一集独有内容") || strings.Contains(encode(entry.Evidence), "第二集独有内容")) {
				t.Error("mixed episode evidence")
			}
		}
		return encode(generationFixture(input))
	})
	job := waitJob(t, s, start(t, s, "generate", selected, false))
	if job.Failed != 0 || job.Done != 3 || job.Skipped != 0 || calls.Load() != 1 {
		t.Fatal(job, calls.Load())
	}
	v, _ = s.getVideo(selected[0])
	if v.Metadata.Title != "标题 & <安全>" || v.Metadata.Plot != "仅根据提供资料" || v.Metadata.Tags[0] != "手动标签" || v.Show.Title != "系列 & 总览" || v.Show.Plot != "仅根据提供资料" {
		t.Fatal("lost edits", v)
	}
	unselected, _ := s.getVideo(identifier(paths[2]))
	if unselected.Episode != nil || unselected.Metadata != nil {
		t.Fatal("changed unselected video")
	}
	zero, _ := s.getVideo(selected[2])
	if *zero.Season != 0 || *zero.Episode != 0 {
		t.Fatal("lost zero numbers")
	}
	scan(t, s, "教程", "episode")
	job = waitJob(t, s, start(t, s, "generate", selected, false))
	if job.Skipped != 3 || calls.Load() != 1 {
		t.Fatal("regenerated complete drafts", job, calls.Load())
	}
	job = waitJob(t, s, start(t, s, "save", selected, false))
	if job.Failed != 0 {
		t.Fatal(job)
	}
	raw, err := os.ReadFile(filepath.Join(s.root, "教程/基础/第一讲.nfo"))
	if err != nil || !strings.Contains(string(raw), "<plot>仅根据提供资料</plot>") || !strings.Contains(string(raw), "<episode>") {
		t.Fatal(string(raw), err)
	}
}

func TestUnifiedGenerationBatchesAndRetry(t *testing.T) {
	s := testStore(t)
	fakeMediaTools(t, `{"streams":[]}`, nil)
	for i := 1; i <= seriesBatchSize+2; i++ {
		put(t, s, fmt.Sprintf("course/lesson-%d.mp4", i), "")
	}
	ids := scan(t, s, "course", "episode")
	var batches atomic.Int32
	fail := true
	calls := generationModel(t, s, func(input seriesGenerationInput) string {
		if len(input.AllEpisodes) != seriesBatchSize+2 || len(input.Episodes) > seriesBatchSize {
			t.Error("lost full hierarchy")
		}
		n := batches.Add(1)
		if n > 1 {
			known := 0
			for _, entry := range input.AllEpisodes {
				if entry.Episode != nil {
					known++
				}
			}
			if known != seriesBatchSize {
				t.Error("earlier batch numbers missing from context", known)
			}
		}
		if n == 2 && fail {
			return `{"series":`
		}
		return encode(generationFixture(input))
	})
	job := waitJob(t, s, start(t, s, "generate", ids, false))
	if job.Failed != 2 || job.Done != len(ids) || calls.Load() != 2 {
		t.Fatal(job, calls.Load())
	}
	var completed int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM videos WHERE metadata IS NOT NULL").Scan(&completed); err != nil || completed != seriesBatchSize {
		t.Fatal(completed, err)
	}
	fail = false
	retry, err := s.retryJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	job = waitJob(t, s, retry["id"])
	if job.Failed != 0 || job.Done != 2 || calls.Load() != 3 {
		t.Fatal(job, calls.Load())
	}
	for i := 1; i <= seriesBatchSize+2; i++ {
		v, _ := s.getVideo(identifier(fmt.Sprintf("course/lesson-%d.mp4", i)))
		if v.Episode == nil || *v.Episode != i {
			t.Fatal("lost natural/global numbering", v)
		}
	}
}

func TestUnifiedGenerationSplitsLongEvidence(t *testing.T) {
	s := testStore(t)
	fakeMediaTools(t, `{"streams":[]}`, nil)
	for i := 1; i <= 3; i++ {
		put(t, s, fmt.Sprintf("course/%02d.mp4", i), "")
		put(t, s, fmt.Sprintf("course/%02d.txt", i), strings.Repeat("长", 24000))
	}
	ids := scan(t, s, "course", "episode")
	calls := generationModel(t, s, func(input seriesGenerationInput) string {
		if len(input.Episodes) != 1 || len(encode(input)) > seriesBatchBytes || len(input.Episodes[0].Evidence.Sources[0].Text) != 72000 {
			t.Error("incorrect evidence budget")
		}
		return encode(generationFixture(input))
	})
	job := waitJob(t, s, start(t, s, "generate", ids, false))
	if job.Failed != 0 || calls.Load() != 3 {
		t.Fatal(job, calls.Load())
	}
}

func TestUnifiedGenerationRejectsInvalidBatchAtomically(t *testing.T) {
	for _, mode := range []string{"missing", "extra", "duplicate_id", "unknown_id", "duplicate_number", "existing_changed", "negative", "too_large", "fraction", "missing_title", "missing_series", "truncated", "null", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			for _, path := range []string{"course/a.mp4", "course/b.mp4", "course/S01E09.mp4"} {
				put(t, s, path, "")
			}
			scan(t, s, "course", "episode")
			ids := []string{identifier("course/a.mp4"), identifier("course/b.mp4")}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			generationModel(t, s, func(input seriesGenerationInput) string {
				r := generationFixture(input)
				switch mode {
				case "missing":
					r.Episodes = r.Episodes[:1]
				case "extra":
					r.Episodes = append(r.Episodes, r.Episodes[0])
				case "duplicate_id":
					r.Episodes[1].ID = r.Episodes[0].ID
				case "unknown_id":
					r.Episodes[1].ID = identifier("course/S01E09.mp4")
				case "duplicate_number":
					r.Episodes[1].Episode = intPointer(9)
				case "existing_changed":
					r.Episodes[0].Season = intPointer(2)
				case "negative":
					r.Episodes[0].Episode = intPointer(-1)
				case "too_large":
					r.Episodes[0].Episode = intPointer(10000)
				case "fraction":
					return `{"episodes":[{"id":"x","episode":1.5}]}`
				case "missing_title":
					r.Episodes[0].Title = ""
				case "missing_series":
					r.Series = nil
				case "truncated":
					return `{"series":`
				case "null":
					return "null"
				case "cancelled":
					cancel()
				}
				return encode(r)
			})
			v, _ := s.getVideo(ids[0])
			results := map[string]generationResult{}
			s.generateSeriesBatch(ctx, v, ids, false, results)
			if results[ids[0]].err == nil {
				t.Fatal("accepted invalid batch")
			}
			for _, id := range ids {
				v, _ := s.getVideo(id)
				if v.Episode != nil || v.Metadata != nil {
					t.Fatal("partially wrote invalid batch", v)
				}
			}
			if show, err := s.getShow("course"); err != nil || show != nil {
				t.Fatal("partial series write", show, err)
			}
		})
	}
}

func TestUnifiedGenerationUnresolvedNumberRetainsMetadata(t *testing.T) {
	s := testStore(t)
	put(t, s, "course/a.mp4", "")
	ids := scan(t, s, "course", "episode")
	generationModel(t, s, func(input seriesGenerationInput) string {
		r := generationFixture(input)
		r.Episodes[0].Episode = nil
		return encode(r)
	})
	job := waitJob(t, s, start(t, s, "generate", ids, false))
	v, _ := s.getVideo(ids[0])
	if job.Failed != 1 || v.Episode != nil || v.Metadata == nil || !strings.Contains(job.Message, "无法确定") {
		t.Fatal(job, v)
	}
}

func TestUnifiedGenerationFullHierarchyAndOverwrite(t *testing.T) {
	s := testStore(t)
	fakeMediaTools(t, `{"streams":[]}`, nil)
	for i := 0; i < 205; i++ {
		put(t, s, fmt.Sprintf("course/层级/章节/lesson-%03d.mp4", i), "")
	}
	ids := scan(t, s, "course", "episode")
	draft(t, s, ids[0])
	calls := generationModel(t, s, func(input seriesGenerationInput) string {
		if len(input.AllEpisodes) != 205 || len(input.Episodes) != 1 || input.AllEpisodes[204].Path != "course/层级/章节/lesson-204.mp4" {
			t.Error("truncated hierarchy")
		}
		return encode(generationFixture(input))
	})
	job := waitJob(t, s, start(t, s, "generate", ids[:1], true))
	v, _ := s.getVideo(ids[0])
	if job.Failed != 0 || calls.Load() != 1 || v.Metadata.Title != "生成标题" || v.Show.Title != "系列 & 总览" {
		t.Fatal(job, v)
	}
}

func TestUnifiedGenerationCancelRetainsCompletedBatches(t *testing.T) {
	s := testStore(t)
	fakeMediaTools(t, `{"streams":[]}`, nil)
	for i := 1; i <= seriesBatchSize+2; i++ {
		put(t, s, fmt.Sprintf("course/lesson-%d.mp4", i), "")
	}
	ids := scan(t, s, "course", "episode")
	generationModel(t, s, func(input seriesGenerationInput) string { return encode(generationFixture(input)) })
	transport := s.client.Transport
	entered := make(chan struct{})
	var requests atomic.Int32
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if requests.Add(1) == 2 {
			close(entered)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return transport.RoundTrip(r)
	})
	id := start(t, s, "generate", ids, false)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("second batch never started")
	}
	if err := s.cancelJob(id); err != nil {
		t.Fatal(err)
	}
	job := waitJob(t, s, id)
	if job.Status != "cancelled" || job.Done != seriesBatchSize {
		t.Fatal("lost completed batch progress", job)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM videos WHERE metadata IS NOT NULL").Scan(&count); err != nil || count != seriesBatchSize {
		t.Fatal(count, err)
	}
	retry, err := s.retryJob(id)
	if err != nil {
		t.Fatal(err)
	}
	job = waitJob(t, s, retry["id"])
	if job.Done != 2 || job.Failed != 0 {
		t.Fatal("cancel retry regenerated completed batches", job)
	}
}

func TestUnifiedGenerationOversizeAndMissingFileIsolation(t *testing.T) {
	s := testStore(t)
	fakeMediaTools(t, `{"streams":[]}`, nil)
	for _, path := range []string{"course/01.mp4", "course/02.mp4", "course/03.mp4"} {
		put(t, s, path, "")
	}
	ids := scan(t, s, "course", "episode")
	put(t, s, "course/01.txt", strings.Repeat("<", 24000))
	if err := os.Remove(filepath.Join(s.root, "course/02.mp4")); err != nil {
		t.Fatal(err)
	}
	calls := generationModel(t, s, func(input seriesGenerationInput) string {
		if len(input.Episodes) != 1 || input.Episodes[0].Path != "course/03.mp4" {
			t.Error("invalid item sent to model")
		}
		return encode(generationFixture(input))
	})
	job := waitJob(t, s, start(t, s, "generate", ids, false))
	if job.Failed != 2 || job.Done != 3 || calls.Load() != 1 {
		t.Fatal(job, calls.Load())
	}
}
