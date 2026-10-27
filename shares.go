package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 分享追更登记
//
// 每条导入的分享在这里登记一份：是否追更、分类、剧名、凭证。
// 条目（items.json）负责"文件在哪"，这张表负责"这个分享还要不要追"。
// ---------------------------------------------------------------------------

// ShareMeta 一条分享的追更登记。
type ShareMeta struct {
	Provider    string    `json:"provider"`
	ShareID     string    `json:"share_id"`
	// Node / Prefix：这个登记只对应分享里的某个子目录（按目录导入）。
	// 有 Node 时追更只重扫这棵子树 —— 否则会把别的目录的文件也带进来。
	Node        string    `json:"node,omitempty"`
	Prefix      string    `json:"prefix,omitempty"`
	Passcode    string    `json:"passcode,omitempty"`
	Category    string    `json:"category,omitempty"`
	Show        string    `json:"show,omitempty"`
	AutoRefresh bool      `json:"auto_refresh"`
	// Hidden = 用户在追更列表点过「移除」：列表不再显示，但已入库条目保留
	// （还能在媒体库里看），重新导入同一分享即可复活。
	Hidden  bool      `json:"hidden,omitempty"`
	AddedAt time.Time `json:"added_at"`

	// ---- 失效巡检（health.go）----
	// FailCount 连续「确认失效」轮数：网络抖动不算，只有明确报分享没了才 +1。
	// 达到阈值才真正清理，避免一次网盘抽风就把整部剧从库里抹掉。
	FailCount int       `json:"fail_count,omitempty"`
	LastCheck time.Time `json:"last_check,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

// ShareStore 分享追更登记的持久化存储（shares.json）。
type ShareStore struct {
	mu    sync.RWMutex
	path  string
	items map[string]*ShareMeta
}

var globalShares *ShareStore

func shareKey(provider, shareID string) string { return provider + "|" + shareID }

func NewShareStore(path string) (*ShareStore, error) {
	s := &ShareStore{path: path, items: map[string]*ShareMeta{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.items)
	}
	return s, nil
}

func (s *ShareStore) save() {
	b, err := json.MarshalIndent(s.items, "", "  ")
	if err == nil {
		_ = os.WriteFile(s.path, b, 0644)
	}
}

// Put 新增或更新登记。
func (s *ShareStore) Put(m ShareMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.AddedAt.IsZero() {
		m.AddedAt = time.Now()
	}
	s.items[shareKey(m.Provider, m.ShareID)] = &m
	s.save()
}

// Get 取登记；不存在返回 nil。
func (s *ShareStore) Get(provider, shareID string) *ShareMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.items[shareKey(provider, shareID)]; ok {
		return v
	}
	return nil
}

// Delete 移除登记（已入库的条目不受影响）。
func (s *ShareStore) Delete(provider, shareID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := shareKey(provider, shareID)
	if _, ok := s.items[k]; !ok {
		return false
	}
	delete(s.items, k)
	s.save()
	return true
}

// List 全部登记。
func (s *ShareStore) List() []ShareMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ShareMeta, 0, len(s.items))
	for _, v := range s.items {
		out = append(out, *v)
	}
	return out
}
