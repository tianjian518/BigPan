package main

// ---------------------------------------------------------------------------
// 大目录后台续扫任务
//
// 分段扫描原本靠前端一段段续调：用户一关页面/切走，剩下的目录就永远停在那里
// （实测「动画电影」停在还剩 43 个目录）。这里把「继续扫」搬进 goroutine，
// 前端只负责轮询进度 —— 关页面服务端照扫，回来点一次「解析」直接看到最新进度。
//
// 任务按「扫描断点 key」唯一（own|<slot>|<nodeID>），重复点解析不会开第二个，
// 而是复用同一个任务。
// ---------------------------------------------------------------------------

import (
	"log"
	"net/http"
	"sync"
	"time"
)

type dbScanTask struct {
	mu      sync.Mutex
	Key     string
	// Provider / ShareID 用来在扫完后把「全量文件」回写进 batchParseCache。
	// 不回写的后果：入库走的是解析缓存，里面只有第一段那几十个文件 ——
	// 这就是「扫出几千个、只导入几十个」的根因（2026-10-26 修）。
	Provider string
	ShareID  string
	Title   string
	Files   []ShareFile
	Remain  int
	Running bool
	Done    bool
	Err     string
	Started time.Time
	Updated time.Time
}

var (
	dbTasksMu sync.Mutex
	dbTasks   = map[string]*dbScanTask{}
)

func dbTaskGet(key string) *dbScanTask {
	dbTasksMu.Lock()
	defer dbTasksMu.Unlock()
	return dbTasks[key]
}

// dbTaskStart 启动（或复用）后台续扫。
//
// key 必须是扫描断点的 key（own|<slot>|<nodeID>），这样 dbScanPending 能读到
// 剩余目录数；step 就是再跑一段 scanDoubaoOwn。
func dbTaskStart(key, provider, shareID, title string, files []ShareFile, step func() (string, []ShareFile, bool, error)) *dbScanTask {
	dbTasksMu.Lock()
	if t, ok := dbTasks[key]; ok {
		t.mu.Lock()
		running := t.Running
		t.mu.Unlock()
		if running {
			dbTasksMu.Unlock()
			return t // 已经在跑，直接复用，不重复打接口
		}
	}
	t := &dbScanTask{
		Key: key, Provider: provider, ShareID: shareID, Title: title, Files: files,
		Remain: dbScanPending(key), Running: true,
		Started: time.Now(), Updated: time.Now(),
	}
	dbTasks[key] = t
	dbTasksMu.Unlock()

	go func() {
		for {
			title, files, pending, err := step()
			t.mu.Lock()
			t.Title = title
			t.Files = files
			t.Remain = dbScanPending(key)
			t.Updated = time.Now()
			provider, shareID := t.Provider, t.ShareID
			t.mu.Unlock()
			// 每段都回写解析缓存：入库时 batch/add 从这里取文件，
			// 不回写就只能拿到第一段的几十个（几千个变几十个的 bug）。
			if provider != "" && shareID != "" && len(files) > 0 {
				batchParseCache.put(ShareInfo{
					Provider: provider, ShareID: shareID, Title: title,
					Files: files, Logged: true, Pending: pending, Scanned: len(files),
				})
			}
			t.mu.Lock()
			if err != nil {
				t.Err = err.Error()
				t.Running = false
				t.Done = true
				t.mu.Unlock()
				log.Printf("[豆包] 后台续扫中止（已扫 %d 个视频）：%v", len(files), err)
				return
			}
			if !pending {
				t.Running = false
				t.Done = true
				t.mu.Unlock()
				log.Printf("[豆包] 后台续扫完成：%d 个视频", len(files))
				return
			}
			t.mu.Unlock()
			time.Sleep(300 * time.Millisecond)
		}
	}()
	return t
}

// dbTaskSnapshot 给前端的一份快照。
func (t *dbScanTask) Snapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return map[string]any{
		"ok": true, "job": t.Key, "title": t.Title,
		"files": t.Files, "count": len(t.Files), "remain": t.Remain,
		"running": t.Running, "done": t.Done, "error": t.Err,
		"elapsed": int(time.Since(t.Started).Seconds()),
	}
}

// dbTaskDrop 任务结束很久后清掉，避免一直占内存（由状态查询顺手触发）。
func dbTaskDrop(key string) {
	dbTasksMu.Lock()
	defer dbTasksMu.Unlock()
	if t, ok := dbTasks[key]; ok {
		t.mu.Lock()
		idle := t.Done && time.Since(t.Updated) > 30*time.Minute
		t.mu.Unlock()
		if idle {
			delete(dbTasks, key)
		}
	}
}

// apiShareScanStatus GET /api/share/parse/status?job=<scanKey>
// 前端轮询用：返回累计文件、剩余目录数、是否还在跑。
func apiShareScanStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("job")
		if key == "" {
			writeJSON(w, map[string]any{"ok": false, "error": "缺少 job 参数"})
			return
		}
		t := dbTaskGet(key)
		if t == nil {
			writeJSON(w, map[string]any{"ok": false, "error": "任务已结束或过期，重新点「解析」即可（进度已存，会接着扫）"})
			return
		}
		writeJSON(w, t.Snapshot())
		dbTaskDrop(key)
	}
}

// dbTaskRunningInfo 同一目录的后台任务正在跑时，返回它的当前进度。
// 用户再点一次「解析」就拿到这个，不会重复开一轮同步扫描。
func dbTaskRunningInfo(key string) (title string, files []ShareFile, ok bool) {
	t := dbTaskGet(key)
	if t == nil {
		return "", nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.Running {
		return "", nil, false
	}
	return t.Title, t.Files, true
}

// dbTaskByShare 按 provider + shareID 找后台任务（入库兜底用）。
func dbTaskByShare(provider, shareID string) *dbScanTask {
	dbTasksMu.Lock()
	list := make([]*dbScanTask, 0, len(dbTasks))
	for _, t := range dbTasks {
		list = append(list, t)
	}
	dbTasksMu.Unlock()
	for _, t := range list {
		t.mu.Lock()
		match := t.Provider == provider && t.ShareID == shareID && len(t.Files) > 0
		t.mu.Unlock()
		if match {
			return t
		}
	}
	return nil
}

// asShareInfo 把任务当前的全量文件包装成 ShareInfo（入库用）。
func (t *dbScanTask) asShareInfo() ShareInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return ShareInfo{
		Provider: t.Provider, ShareID: t.ShareID, Title: t.Title,
		Files: t.Files, Logged: true, Pending: t.Running, Scanned: len(t.Files),
	}
}
