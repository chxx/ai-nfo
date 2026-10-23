package main

import (
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
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.runJob(id, body.Action, ids, body.Overwrite) }()
	return map[string]string{"id": id, "cover_job_id": coverID}, nil
}
func (s *Store) cancelJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("UPDATE jobs SET status='cancelling' WHERE id=? AND status IN ('queued','running')", id)
	if cancel := s.coverCancels[id]; cancel != nil {
		cancel()
	}
	return err
}
func (s *Store) runJob(id, action string, ids []string, overwrite bool) {
	s.db.Exec("UPDATE jobs SET status=CASE WHEN status='cancelling' THEN status ELSE 'running' END WHERE id=?", id)
	shows := map[string]bool{}
	for _, vid := range ids {
		var status string
		if err := s.db.QueryRow("SELECT status FROM jobs WHERE id=?", id).Scan(&status); err != nil {
			return
		}
		if status == "cancelling" || s.ctx.Err() != nil {
			s.db.Exec("UPDATE jobs SET status='cancelled',message='任务已取消；已经完成的结果保留' WHERE id=?", id)
			return
		}
		skipped, err := s.processVideo(vid, action, overwrite, shows)
		if err != nil {
			message := publicError(err)
			s.db.Exec("UPDATE videos SET status='error',error=? WHERE id=?", message, vid)
			s.db.Exec("UPDATE jobs SET failed=failed+1,message=? WHERE id=?", message, id)
		}
		n := 0
		if skipped {
			n = 1
		}
		if _, err = s.db.Exec("UPDATE jobs SET done=done+1,skipped=skipped+? WHERE id=?", n, id); err != nil {
			return
		}
	}
	s.db.Exec("UPDATE jobs SET status=CASE WHEN status='cancelling' THEN 'cancelled' WHEN failed>0 THEN 'partial' ELSE 'completed' END WHERE id=?", id)
}
func (s *Store) processVideo(id, action string, overwrite bool, shows map[string]bool) (bool, error) {
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
				show, err = s.ai(s.ctx, map[string]any{"kind": "series_overview", "folder": v.Show.Title, "directory": *v.ShowPath, "episode_filenames": names, "sample_episode_embedded": evidence.Embedded, "evidence": "提供完整相对目录和文件名，可能不是完整系列；内嵌标签仅来自当前单集，不能代表整个系列；请保守概括"})
				if err != nil {
					return false, err
				}
				if _, err = s.db.Exec("UPDATE shows SET metadata=? WHERE path=?", encode(show), *v.ShowPath); err != nil {
					return false, err
				}
			}
		}
		m, err := s.ai(s.ctx, evidence)
		if err != nil {
			return false, err
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
