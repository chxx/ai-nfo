package main

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"strings"
	"sync"
)

type CoverTask struct {
	ID      string `json:"id"`
	VideoID string `json:"video_id"`
	Owner   string `json:"owner"`
	Scope   string `json:"scope"`
	Path    string `json:"path"`
	Status  string `json:"status"`
	Error   string `json:"error"`
}

// A shared gate covers the whole pipeline, including manual requests.
func (s *Store) acquireCover(ctx context.Context, wait bool) (func(), error) {
	release, err := s.coverLimiter.acquire(ctx, s.ctx, wait)
	if err != nil {
		return nil, err
	}
	if !s.coverMu.TryRLock() {
		release()
		return nil, userError("正在清理记录，请稍后重试封面操作")
	}
	return func() { s.coverMu.RUnlock(); release() }, nil
}

func (s *Store) coverTargets(ids []string) ([]CoverTask, error) {
	tasks := []CoverTask{}
	seen := map[string]bool{}
	for _, id := range ids {
		v, err := s.getVideo(id)
		if err != nil {
			return nil, err
		}
		candidates := []CoverTask{{VideoID: id, Owner: "video:" + v.Path, Scope: "video", Path: v.Path}}
		if v.Kind == "episode" && v.ShowPath != nil {
			candidates = append(candidates, CoverTask{VideoID: id, Owner: "show:" + *v.ShowPath, Scope: "series", Path: *v.ShowPath})
		}
		for _, task := range candidates {
			if !seen[task.Owner] {
				seen[task.Owner] = true
				tasks = append(tasks, task)
			}
		}
	}
	return tasks, nil
}

func queueCoverTasks(tx *sql.Tx, tasks []CoverTask) (string, []CoverTask, error) {
	jobID := randomID()
	queued := []CoverTask{}
	for _, task := range tasks {
		task.ID = randomID()
		task.Status = "queued"
		task.Error = ""
		result, err := tx.Exec("INSERT OR IGNORE INTO cover_tasks(id,job_id,video_id,owner,scope,path,status) VALUES(?,?,?,?,?,?,'queued')", task.ID, jobID, task.VideoID, task.Owner, task.Scope, task.Path)
		if err != nil {
			return "", nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return "", nil, err
		}
		if n > 0 {
			queued = append(queued, task)
		}
	}
	if len(queued) == 0 {
		return "", queued, nil
	}
	_, err := tx.Exec("INSERT INTO jobs(id,action,status,ids,total,created,message) VALUES(?,'covers','queued',?,?,?,'自动获取封面；按配置的并发上限执行，已有封面保留')", jobID, encode(queued), len(queued), now())
	return jobID, queued, err
}

// Called while mu is held, preventing WaitGroup.Add from racing with shutdown.
func (s *Store) launchCoverJob(id string, tasks []CoverTask) {
	if id == "" {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.coverCancels[id] = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		s.runCoverJob(ctx, id, tasks)
		s.mu.Lock()
		delete(s.coverCancels, id)
		s.mu.Unlock()
	}()
}

func (s *Store) runCoverJob(ctx context.Context, id string, tasks []CoverTask) {
	s.db.Exec("UPDATE jobs SET status='running' WHERE id=? AND status='queued'", id)
	queue := make(chan CoverTask, len(tasks))
	for _, task := range tasks {
		queue <- task
	}
	close(queue)
	var workers sync.WaitGroup
	for range min(s.getConfig().Concurrency, len(tasks)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for task := range queue {
				if ctx.Err() != nil {
					return
				}
				release, err := s.acquireCover(ctx, true)
				if err != nil {
					return
				}
				if ctx.Err() != nil {
					release()
					return
				}
				s.db.Exec("UPDATE cover_tasks SET status='running' WHERE id=?", task.ID)
				skipped, err := s.autoCover(ctx, task)
				release()
				status, message := "saved", ""
				failed, skip, done := 0, 0, 1
				if err != nil {
					if ctx.Err() != nil {
						status = "cancelled"
						done = 0
						message = "已取消，已保存的封面保留"
					} else {
						status = "failed"
						failed = 1
						message = publicError(err)
					}
				} else if skipped {
					status = "skipped"
					skip = 1
					message = "已有封面已保留"
				}
				tx, e := s.db.Begin()
				if e != nil {
					return
				}
				_, e = tx.Exec("UPDATE cover_tasks SET status=?,error=? WHERE id=?", status, message, task.ID)
				if e == nil {
					_, e = tx.Exec("UPDATE jobs SET done=done+?,failed=failed+?,skipped=skipped+?,message=CASE WHEN ?<>'' THEN ? ELSE message END WHERE id=?", done, failed, skip, message, message, id)
				}
				if e != nil {
					tx.Rollback()
					return
				}
				if tx.Commit() != nil {
					return
				}
			}
		}()
	}
	workers.Wait()
	if ctx.Err() != nil {
		s.db.Exec("UPDATE cover_tasks SET status='cancelled',error='已取消，等待重试' WHERE job_id=? AND status IN ('queued','running')", id)
		s.db.Exec("UPDATE jobs SET status='cancelled',message='封面任务已取消；已保存结果保留，可重试未完成项' WHERE id=?", id)
	} else {
		// Unexpected storage failures must not look like successful completion.
		s.db.Exec("UPDATE cover_tasks SET status='failed',error='处理被中断，请重试' WHERE job_id=? AND status IN ('queued','running')", id)
		s.db.Exec("UPDATE jobs SET done=(SELECT COUNT(*) FROM cover_tasks WHERE job_id=? AND status IN ('saved','skipped','failed')),failed=(SELECT COUNT(*) FROM cover_tasks WHERE job_id=? AND status='failed'),status=CASE WHEN EXISTS(SELECT 1 FROM cover_tasks WHERE job_id=? AND status='failed') THEN 'partial' ELSE 'completed' END WHERE id=?", id, id, id, id)
	}
}

func bestAutoCover(candidates []CoverCandidate, web bool, scope string) *CoverCandidate {
	eligible := []CoverCandidate{}
	for _, c := range candidates {
		// Text matching is not image recognition. Uncertain web matches fall back to a real video frame.
		if web && c.Match != "likely" {
			continue
		}
		eligible = append(eligible, c)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		score := func(c CoverCandidate) float64 {
			ratio := float64(c.Width) / float64(max(1, c.Height))
			ideal := 16.0 / 9
			if scope == "series" {
				ideal = 2.0 / 3
			}
			return math.Log2(float64(max(1, c.Width*c.Height))) - math.Abs(math.Log(ratio/ideal))
		}
		return score(eligible[i]) > score(eligible[j])
	})
	if len(eligible) == 0 {
		return nil
	}
	return &eligible[0]
}

func (s *Store) autoCover(ctx context.Context, task CoverTask) (bool, error) {
	v, err := s.getVideo(task.VideoID)
	if err != nil {
		return false, err
	}
	target, err := s.targetCover(v, task.Scope)
	if err != nil {
		return false, err
	}
	if target.Owner != task.Owner {
		return false, userError("视频所属系列已变化，请重新创建封面任务")
	}
	if s.videoAvailable(target.Path) {
		return true, nil
	}
	if !v.Exists {
		return false, userError("视频不可访问，无法自动获取封面")
	}
	var chosen *CoverCandidate
	failures := []string{}
	embedded, err := s.embeddedCoversWork(ctx, CoverRequest{ID: v.ID, Scope: task.Scope})
	if err == nil {
		chosen = bestAutoCover(embedded.Candidates, false, task.Scope)
	} else {
		failures = append(failures, publicError(err))
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if chosen == nil {
		web, err := s.searchCoversWork(ctx, CoverRequest{ID: v.ID, Scope: task.Scope, UseAI: true})
		if err == nil {
			chosen = bestAutoCover(web.Candidates, true, task.Scope)
			if chosen == nil {
				failures = append(failures, "联网候选未获得明确匹配")
			}
		} else {
			failures = append(failures, publicError(err))
		}
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if chosen == nil {
		for _, seconds := range []float64{5, 0} {
			candidate, e := s.snapshotCoverWork(ctx, SnapshotRequest{ID: v.ID, Scope: task.Scope, Seconds: seconds})
			if e == nil {
				chosen = &candidate
				break
			}
			err = e
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
		}
		if chosen == nil {
			failures = append(failures, publicError(err))
			return false, userError(cleanText(strings.Join(failures, "；"), 1200))
		}
	}
	// Cancellation and file writing are serialized; once cancel returns no new writes begin.
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	saved, err := s.saveCoverWork(SaveCoverRequest{ID: v.ID, Scope: task.Scope, CandidateID: chosen.ID})
	if err != nil {
		return false, err
	}
	return saved["saved"] != true, nil
}

func (s *Store) coverTaskList(id string) ([]CoverTask, error) {
	rows, err := s.db.Query("SELECT id,video_id,owner,scope,path,status,error FROM cover_tasks WHERE job_id=? ORDER BY CASE status WHEN 'failed' THEN 0 WHEN 'running' THEN 1 WHEN 'queued' THEN 2 ELSE 3 END,path LIMIT 200", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []CoverTask{}
	for rows.Next() {
		var t CoverTask
		if err = rows.Scan(&t.ID, &t.VideoID, &t.Owner, &t.Scope, &t.Path, &t.Status, &t.Error); err != nil {
			return nil, err
		}
		items = append(items, t)
	}
	return items, rows.Err()
}

func (s *Store) retryCoverJob(id string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil, userError("服务正在关闭")
	}
	var status string
	if err := s.db.QueryRow("SELECT status FROM jobs WHERE id=? AND action='covers'", id).Scan(&status); err != nil {
		return nil, userError("找不到封面任务")
	}
	if status == "queued" || status == "running" || status == "cancelling" {
		return nil, userError("请等待任务结束后再重试")
	}
	rows, err := s.db.Query("SELECT video_id,owner,scope,path FROM cover_tasks WHERE job_id=? AND status IN ('failed','interrupted','cancelled')", id)
	if err != nil {
		return nil, err
	}
	tasks := []CoverTask{}
	for rows.Next() {
		var t CoverTask
		if err = rows.Scan(&t.VideoID, &t.Owner, &t.Scope, &t.Path); err != nil {
			rows.Close()
			return nil, err
		}
		tasks = append(tasks, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, queued, err := queueCoverTasks(tx, tasks)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.launchCoverJob(job, queued)
	return map[string]string{"id": job}, nil
}
