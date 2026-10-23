package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const mediaFormats = "mov,matroska,avi,mpegts,flv,asf,mpeg,mpegvideo,h264,hevc"

type embeddedStream struct {
	Index       int               `json:"index"`
	CodecType   string            `json:"codec_type"`
	CodecName   string            `json:"codec_name"`
	Tags        map[string]string `json:"tags"`
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
}
type embeddedProbe struct {
	Format struct {
		Tags map[string]string `json:"tags"`
	} `json:"format"`
	Streams []embeddedStream `json:"streams"`
}

func mediaInputOptions() []string {
	return []string{"-protocol_whitelist", "file,pipe", "-format_whitelist", mediaFormats, "-probesize", "5000000", "-analyzeduration", "1000000"}
}

// Only local input and explicit streams are allowed; metadata never becomes a command or path.
func runMedia(ctx context.Context, tool string, limit int, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	output := &limitedBuffer{limit: limit}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, userError("未安装 " + tool + "；Docker 镜像已包含，本机调试需安装 FFmpeg 工具包")
		}
		if ctx.Err() != nil {
			return nil, userError("读取视频内嵌资料超时或已取消")
		}
		return nil, userError("无法读取视频内嵌资料，请检查文件格式、完整性及访问权限")
	}
	return output.Bytes(), nil
}

func probeEmbedded(ctx context.Context, path string) (embeddedProbe, error) {
	var result embeddedProbe
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := append([]string{"-v", "error", "-threads", "2"}, mediaInputOptions()...)
	args = append(args, "-show_entries", "format_tags:stream=index,codec_type,codec_name:stream_disposition=attached_pic:stream_tags", "-of", "json", "-i", path)
	raw, err := runMedia(ctx, "ffprobe", 512<<10, args...)
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return result, userError("视频内嵌资料格式无法识别")
	}
	return result, nil
}

func (p embeddedProbe) metadata() map[string]string {
	result := map[string]string{}
	// Prefer container tags; a main video stream may supply missing fields.
	groups := []map[string]string{p.Format.Tags}
	for _, stream := range p.Streams {
		if stream.CodecType == "video" && stream.Disposition.AttachedPic == 0 {
			groups = append(groups, stream.Tags)
			break
		}
	}
	for _, tags := range groups {
		keys := make([]string, 0, len(tags))
		for key := range tags {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			normalized := strings.ToLower(strings.TrimSpace(key))
			limit := 300
			switch normalized {
			case "title", "director", "artist", "author", "album", "album_artist", "date", "year", "creation_time", "copyright", "genre", "show", "episode_id", "season_number", "episode_sort":
			case "description", "synopsis", "comment":
				limit = 4000
			default:
				continue
			}
			value := cleanText(tags[key], limit)
			if result[normalized] == "" && value != "" {
				result[normalized] = value
			}
		}
	}
	return result
}

func (s *Store) embeddedCovers(ctx context.Context, body CoverRequest) (CoverResults, error) {
	release, err := s.acquireCover(ctx, false)
	if err != nil {
		return CoverResults{}, err
	}
	defer release()
	return s.embeddedCoversWork(ctx, body)
}

func (s *Store) embeddedCoversWork(ctx context.Context, body CoverRequest) (CoverResults, error) {
	result := CoverResults{Candidates: []CoverCandidate{}, Warnings: []string{}}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	v, err := s.getVideo(body.ID)
	if err != nil {
		return result, err
	}
	target, err := s.targetCover(v, body.Scope)
	if err != nil {
		return result, err
	}
	path, err := s.safe(v.Path, true)
	if err != nil {
		return result, err
	}
	probe, err := probeEmbedded(ctx, path)
	if err != nil {
		return result, err
	}
	attempts := 0
	for _, stream := range probe.Streams {
		attachment := stream.CodecType == "attachment"
		if stream.Index < 0 || (!attachment && stream.Disposition.AttachedPic != 1) {
			continue
		}
		if attachment {
			mime := ""
			for k, v := range stream.Tags {
				if strings.EqualFold(k, "mimetype") {
					mime = strings.ToLower(v)
				}
			}
			if mime != "image/jpeg" && mime != "image/png" && mime != "image/webp" {
				continue
			}
		} else if stream.CodecName != "mjpeg" && stream.CodecName != "png" && stream.CodecName != "webp" {
			result.Warnings = append(result.Warnings, "跳过不支持格式的内嵌图片（支持 JPEG、PNG、WebP）")
			continue
		}
		if attempts >= 4 {
			result.Warnings = append(result.Warnings, "本次最多读取 4 张内嵌图片")
			break
		}
		attempts++
		args := append([]string{"-nostdin", "-v", "error", "-threads", "2"}, mediaInputOptions()...)
		index := strconv.Itoa(stream.Index)
		if attachment {
			args = append(args, "-dump_attachment:"+index, "pipe:1", "-i", path, "-map", "0:V:0", "-frames:v", "0", "-f", "null", "-")
		} else {
			args = append(args, "-i", path, "-map", "0:"+index, "-frames:v", "1", "-c:v", "copy", "-f", "image2pipe", "pipe:1")
		}
		raw, extractErr := runMedia(ctx, "ffmpeg", maxCoverBytes, args...)
		if extractErr != nil {
			result.Warnings = append(result.Warnings, publicError(extractErr))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		reason := "直接提取自当前视频文件，未使用 AI 或联网搜索"
		if body.Scope == "series" {
			reason += "；这张图片属于当前单集，请确认适合作为系列封面"
		}
		c, cacheErr := s.cacheCover(target.Owner, CoverCandidate{Title: v.Filename + " · 内嵌封面", Kind: "embedded", Match: "embedded", Reason: reason}, raw)
		if cacheErr != nil {
			result.Warnings = append(result.Warnings, publicError(cacheErr))
			continue
		}
		result.Candidates = append(result.Candidates, c)
	}
	if len(result.Candidates) == 0 {
		result.Warnings = append(result.Warnings, "未找到可提取的内嵌封面，可继续联网查找或从视频截图")
	}
	return result, nil
}
