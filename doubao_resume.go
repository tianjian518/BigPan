package main

// ---------------------------------------------------------------------------
// 豆包扫描断点续传
//
// 全量扫一个几千文件的目录要打上千次接口（400ms 一次 ≈ 7 分钟），
// 中途撞上风控是常态，不是意外。没有断点的话每次重来都从第一个目录开始，
// 每次都撞在同一堵墙上 —— 永远扫不完。
//
// 这里把进度每 30 个目录落一次盘，风控恢复后再点一次导入就接着扫。
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// dbScanTTL 断点有效期。超过就丢弃重扫（盘里的文件可能已经变了）。
const dbScanTTL = 24 * time.Hour

// dbScanJob 是队列里的一项：待扫目录 + 它下面文件的路径前缀。
type dbScanJob struct {
	ID     string `json:"id"`
	Prefix string `json:"prefix"`
}

// dbScanState 是一次扫描的完整现场。
type dbScanState struct {
	Key     string      `json:"key"`
	Title   string      `json:"title"`
	Queue   []dbScanJob `json:"queue"`
	Seen    []string    `json:"seen"`
	Files   []ShareFile `json:"files"`
	// Soft 连续软限流次数：限流不算失败（等会儿接着扫），但要有个头，
	// 否则一直限流会变成无限重试。
	Soft    int   `json:"soft,omitempty"`
	SavedAt int64 `json:"saved_at"`
}

var (
	dbScanOnce sync.Once
	dbScanMu   sync.Mutex
	dbScanAll  = map[string]*dbScanState{}
)

func dbScanFile() string {
	return filepath.Join(filepath.Dir(*flagData), "doubao_scan.json")
}

func dbScanInit() {
	dbScanOnce.Do(func() {
		if raw, err := os.ReadFile(dbScanFile()); err == nil {
			if err := json.Unmarshal(raw, &dbScanAll); err != nil {
				log.Printf("[豆包] 扫描断点文件损坏，已忽略：%v", err)
				dbScanAll = map[string]*dbScanState{}
			}
		}
		if dbScanAll == nil {
			dbScanAll = map[string]*dbScanState{}
		}
	})
}

// dbScanSaveLocked 落盘（调用方持锁）。
func dbScanSaveLocked() {
	raw, err := json.Marshal(dbScanAll)
	if err != nil {
		return
	}
	p := dbScanFile()
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0644); err != nil {
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		log.Printf("[豆包] 扫描断点写盘失败：%v", err)
	}
}

// dbScanTake 取出该 key 的断点；没有、或已过期，就返回一个全新的空状态。
func dbScanTake(key string) *dbScanState {
	dbScanInit()
	dbScanMu.Lock()
	defer dbScanMu.Unlock()
	if st, ok := dbScanAll[key]; ok {
		if time.Since(time.Unix(st.SavedAt, 0)) < dbScanTTL {
			return st
		}
		delete(dbScanAll, key) // 过期了，别拿昨天的现场接着扫
		dbScanSaveLocked()
	}
	return &dbScanState{Key: key}
}

// dbScanPut 保存进度。
func dbScanPut(st *dbScanState) {
	dbScanInit()
	st.SavedAt = time.Now().Unix()
	dbScanMu.Lock()
	dbScanAll[st.Key] = st
	dbScanSaveLocked()
	dbScanMu.Unlock()
}

// dbScanDone 扫描完成，删掉断点。
func dbScanDone(key string) {
	dbScanInit()
	dbScanMu.Lock()
	if _, ok := dbScanAll[key]; ok {
		delete(dbScanAll, key)
		dbScanSaveLocked()
	}
	dbScanMu.Unlock()
}

// dbScanPending 返回某个扫描还没扫的目录数（后台任务给前端报进度用）。
func dbScanPending(key string) int {
	dbScanInit()
	dbScanMu.Lock()
	defer dbScanMu.Unlock()
	if st, ok := dbScanAll[key]; ok {
		return len(st.Queue)
	}
	return 0
}
