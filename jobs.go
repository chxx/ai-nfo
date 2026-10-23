package main

import (
	"context"
	"path/filepath"
)

func (s *Store) listJobs() ([]Job, error) {
	rows, err := s.db.Query("SELECT id,action,status,total,done,failed,skipped,message,created FROM jobs WHERE status IN ('queued','running','cancelling') OR id IN (SELECT id FROM jobs ORDER BY created DESC LIMIT 12) ORDER BY created DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Job{}
	for rows.Next() {
		var j Job
		if err = rows.Scan(&j.ID, &j.Action, &j.Status, &j.Total, &j.Done, &j.Failed, &j.Skipped, &j.Message, &j.Created); err != nil {
			return nil, err
		}
		result = append(result, j)
	}
	return result, rows.Err()
}

type JobRequest struct {
	Action     string   `json:"action"`
	IDs        []string `json:"ids"`
	Overwrite  bool     `json:"overwrite"`
	AutoCovers *bool    `json:"auto_covers"`
}

func (s *Store) startJob(body JobRequest) (map[string]string, error) {
	if body.Action != "generate" && body.Action != "save" && body.Action != "covers" || len(body.IDs) == 0 || len(body.IDs) > 5000 {
		return nil, userError("请选择 1–5000 个视频")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startJobLocked(body, "")
}

func (s *Store) startJobLocked(body JobRequest, retryOf string) (map[string]string, error) {
	if s.ctx.Err() != nil {
		return nil, userError("服务正在关闭")
	}
	if body.Action != "covers" {
		if err := s.ensureJobsIdle(true); err != nil {
			return nil, err
		}
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, id := range body.IDs {
		if seen[id] {
			continue
		}
		if _, err := s.getVideo(id); err != nil {
			return nil, err
		}
		seen[id] = true
		ids = append(ids, id)
	}
	c := s.getConfig()
	if body.Action == "generate" && (c.Model == "" || c.BaseURL == "") {
		return nil, userError("请先在设置中填写 AI 接口地址和模型名称")
	}
	id := randomID()
	var targets []CoverTask
	var err error
	if body.Action == "covers" || body.Action == "generate" && (body.AutoCovers == nil || *body.AutoCovers) {
		targets, err = s.coverTargets(ids)
		if err != nil {
			return nil, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if body.Action != "covers" {
		if _, err = tx.Exec("INSERT INTO jobs(id,action,status,ids,total,created) VALUES(?,?,'queued',?,?,?)", id, body.Action, encode(ids), len(ids), now()); err != nil {
			return nil, err
		}
	}
	if body.Action != "covers" {
		for _, vid := range ids {
			if _, err = tx.Exec("INSERT INTO job_items(job_id,video_id,status,overwrite) VALUES(?,?,'queued',?)", id, vid, body.Overwrite); err != nil {
				return nil, err
			}
			if retryOf != "" {
				if _, err = tx.Exec("UPDATE job_items SET retry_job_id=? WHERE job_id=? AND video_id=?", id, retryOf, vid); err != nil {
					return nil, err
				}
			}
		}
	}
	coverID, tasks, err := queueCoverTasks(tx, targets)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.launchCoverJob(coverID, tasks)
	if body.Action == "covers" {
		return map[string]string{"id": coverID}, nil
	}
	s.launchTextJob(id, body.Action, ids, body.Overwrite)
	return map[string]string{"id": id, "cover_job_id": coverID}, nil
}
func (s *Store) cancelJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("UPDATE jobs SET status='cancelling' WHERE id=? AND status IN ('queued','running')", id)
	if cancel := s.jobCancels[id]; cancel != nil {
		cancel()
	}
	return err
}
func (s *Store) launchTextJob(id, action string, ids []string, overwrite bool) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.jobCancels[id] = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		s.runJob(s.retryContext(ctx, id), id, action, ids, overwrite)
		s.mu.Lock()
		delete(s.jobCancels, id)
		s.mu.Unlock()
	}()
}

func (s *Store) runJob(ctx context.Context, id, action string, ids []string, overwrite bool) {
	s.db.Exec("UPDATE jobs SET status='running' WHERE id=? AND status='queued'", id)
	defer func() {
		tx, err := s.db.Begin()
		if err != nil {
			return
		}
		defer tx.Rollback()
		terminal := "failed"
		message := "处理被中断，请重试"
		if ctx.Err() != nil {
			terminal = "cancelled"
			message = "已取消，等待重试"
		}
		if _, err = tx.Exec("UPDATE job_items SET status=?,error=? WHERE job_id=? AND status IN ('queued','running')", terminal, message, id); err != nil {
			return
		}
		if _, err = tx.Exec("UPDATE jobs SET done=(SELECT COUNT(*) FROM job_items WHERE job_id=? AND status IN ('completed','skipped','failed')),failed=(SELECT COUNT(*) FROM job_items WHERE job_id=? AND status='failed'),skipped=(SELECT COUNT(*) FROM job_items WHERE job_id=? AND status='skipped') WHERE id=?", id, id, id, id); err != nil {
			return
		}
		if ctx.Err() != nil {
			_, err = tx.Exec("UPDATE jobs SET status='cancelled',message='已取消；已完成结果保留，可重试未完成项' WHERE id=?", id)
		} else {
			_, err = tx.Exec("UPDATE jobs SET status=CASE WHEN failed>0 THEN 'partial' ELSE 'completed' END,message=CASE WHEN failed=0 THEN '任务已完成' ELSE COALESCE((SELECT error FROM job_items WHERE job_id=? AND status='failed' LIMIT 1),message) END WHERE id=?", id, id)
		}
		if err == nil {
			tx.Commit()
		}
	}()
	shows := map[string]bool{}
	for _, vid := range ids {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.db.Exec("UPDATE job_items SET status='running' WHERE job_id=? AND video_id=?", id, vid); err != nil {
			return
		}
		skipped, err := s.processVideo(ctx, vid, action, overwrite, shows)
		if err != nil && ctx.Err() != nil {
			s.db.Exec("UPDATE videos SET status='error',error='任务已取消，请重试' WHERE id=? AND status='generating'", vid)
			return
		}
		status, message, failed, skip := "completed", "", 0, 0
		if err != nil {
			status, message, failed = "failed", publicError(err), 1
			s.db.Exec("UPDATE videos SET status='error',error=? WHERE id=?", message, vid)
		} else if skipped {
			status, skip = "skipped", 1
		}
		tx, e := s.db.Begin()
		if e != nil {
			return
		}
		_, e = tx.Exec("UPDATE job_items SET status=?,error=? WHERE job_id=? AND video_id=?", status, message, id, vid)
		if e == nil {
			_, e = tx.Exec("UPDATE jobs SET done=done+1,failed=failed+?,skipped=skipped+?,message=CASE WHEN ?<>'' THEN ? ELSE message END WHERE id=?", failed, skip, message, message, id)
		}
		if e != nil {
			tx.Rollback()
			return
		}
		if tx.Commit() != nil {
			return
		}
	}
}

func (s *Store) retryJob(id string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var action, status string
	if err := s.db.QueryRow("SELECT action,status FROM jobs WHERE id=?", id).Scan(&action, &status); err != nil {
		return nil, userError("找不到任务")
	}
	if action != "generate" && action != "save" {
		return nil, userError("请使用封面任务的重试入口")
	}
	if status == "queued" || status == "running" || status == "cancelling" {
		return nil, userError("请等待任务结束后再重试")
	}
	rows, err := s.db.Query("SELECT i.video_id,i.overwrite FROM job_items i JOIN videos v ON v.id=i.video_id WHERE i.job_id=? AND i.status IN ('failed','cancelled','interrupted') AND i.retry_job_id='' ORDER BY v.path", id)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	overwrite := false
	for rows.Next() {
		var vid string
		if err = rows.Scan(&vid, &overwrite); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, vid)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return map[string]string{"id": ""}, nil
	}
	auto := false
	return s.startJobLocked(JobRequest{Action: action, IDs: ids, Overwrite: overwrite, AutoCovers: &auto}, id)
}

func (s *Store) processVideo(ctx context.Context, id, action string, overwrite bool, shows map[string]bool) (bool, error) {
	v, err := s.getVideo(id)
	if err != nil {
		return false, err
	}
	if _, err = s.safe(v.Path, true); err != nil {
		return false, err
	}
	if action == "generate" {
		if v.Metadata != nil && !overwrite {
			return true, nil
		}
		if _, err = s.db.Exec("UPDATE videos SET status='generating',error='' WHERE id=?", id); err != nil {
			return false, err
		}
		evidence, err := s.evidence(v)
		if err != nil {
			return false, err
		}
		if v.ShowPath != nil {
			show, err := s.getShow(*v.ShowPath)
			if err != nil {
				return false, err
			}
			if show == nil {
				rows, err := s.db.Query("SELECT path FROM videos WHERE show_path=? ORDER BY path LIMIT 200", *v.ShowPath)
				if err != nil {
					return false, err
				}
				names := []string{}
				for rows.Next() {
					var p string
					if err = rows.Scan(&p); err != nil {
						rows.Close()
						return false, err
					}
					names = append(names, filepath.Base(p))
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					return false, err
				}
				show, err = s.ai(ctx, map[string]any{"kind": "series_overview", "folder": v.Show.Title, "directory": *v.ShowPath, "episode_filenames": names, "sample_episode_embedded": evidence.Embedded, "evidence": "提供完整相对目录和文件名，可能不是完整系列；内嵌标签仅来自当前单集，不能代表整个系列；请保守概括"})
				if err != nil {
					return false, err
				}
				if _, err = s.db.Exec("UPDATE shows SET metadata=? WHERE path=?", encode(show), *v.ShowPath); err != nil {
					return false, err
				}
			}
		}
		m, err := s.ai(ctx, evidence)
		if err != nil {
			return false, err
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		_, err = s.db.Exec("UPDATE videos SET metadata=?,status='draft',error='',updated=? WHERE id=?", encode(m), now(), id)
		return false, err
	}
	files, err := s.preview(id)
	if err != nil {
		return false, err
	}
	episodeWritten, showSkipped := false, false
	for _, file := range files {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		isShow := file.Path != nfoPath(v.Path)
		if wrote, seen := shows[file.Path]; isShow && seen {
			showSkipped = !wrote
			continue
		}
		wrote, err := s.writeNFO(file.Path, file.XML, overwrite)
		if err != nil {
			return false, err
		}
		if isShow {
			shows[file.Path] = wrote
			showSkipped = !wrote
		} else {
			episodeWritten = wrote
		}
	}
	if !episodeWritten {
		_, err = s.db.Exec("UPDATE videos SET error='已有 NFO 已保留，本次草稿未写入' WHERE id=?", id)
		return true, err
	}
	message := ""
	if showSkipped {
		message = "已有 tvshow.nfo 已保留；系列介绍未覆盖"
	}
	_, err = s.db.Exec("UPDATE videos SET status='saved',error=?,updated=? WHERE id=?", message, now(), id)
	return false, err
}
