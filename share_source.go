package main

// ---------------------------------------------------------------------------
// 分享收藏（别人的豆包分享）
//
// 用户 2026-10-27：「分享链接不能固定放在里面。应该像天剑豆包一样，连接一次，
// 下次直接点浏览就行了。和上面的豆包一、豆包二并列放置。」
//
// 所以这里把「粘过的分享链接」持久化一份：豆包盘页与账号行并排显示，
// 点「浏览」直接进目录树，不用再粘一次。只存链接与标题，不存任何凭证。
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ShareSource 一条分享收藏。
type ShareSource struct {
	ShareID string    `json:"share_id"`
	Link    string    `json:"link"`
	Title   string    `json:"title"`           // 分享自带标题（如「芦苇豆包」）
	Alias   string    `json:"alias,omitempty"` // 用户可改名
	AddedAt time.Time `json:"added_at"`
}

// ShareSourceStore 分享收藏的持久化（share_sources.json）。
type ShareSourceStore struct {
	mu    sync.RWMutex
	path  string
	items map[string]*ShareSource
}

var globalSources *ShareSourceStore

func NewShareSourceStore(path string) (*ShareSourceStore, error) {
	s := &ShareSourceStore{path: path, items: map[string]*ShareSource{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.items)
	}
	return s, nil
}

func (s *ShareSourceStore) save() {
	b, err := json.MarshalIndent(s.items, "", "  ")
	if err == nil {
		_ = os.WriteFile(s.path, b, 0644)
	}
}

func (s *ShareSourceStore) Put(x ShareSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if x.AddedAt.IsZero() {
		x.AddedAt = time.Now()
	}
	if old, ok := s.items[x.ShareID]; ok && x.Alias == "" {
		x.Alias = old.Alias // 重新添加不冲掉用户改的名
	}
	s.items[x.ShareID] = &x
	s.save()
}

func (s *ShareSourceStore) Delete(shareID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[shareID]; !ok {
		return false
	}
	delete(s.items, shareID)
	s.save()
	return true
}

func (s *ShareSourceStore) List() []ShareSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ShareSource, 0, len(s.items))
	for _, v := range s.items {
		out = append(out, *v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AddedAt.Before(out[j].AddedAt) })
	return out
}

// apiShareSources 分享收藏的增删查：GET 列表 / POST 添加 / DELETE ?id= 移除。
func apiShareSources(as *AccountStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if globalSources == nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "分享收藏未初始化"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, map[string]interface{}{"ok": true, "sources": globalSources.List()})

		case http.MethodPost:
			var req struct {
				Link string `json:"link"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "请求格式错误"})
				return
			}
			link := strings.TrimSpace(req.Link)
			dp, ok := findProvider(link).(doubaoProvider)
			if !ok {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "这不是豆包分享链接"})
				return
			}
			shareID, err := parseDoubaoLink(link)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}
			if strings.HasPrefix(shareID, doubaoShrPrefix) {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "新格式分享（/drive/shr/）直接用「分享导入」页，这里只收老的 /drive/s/ 链接"})
				return
			}
			cli := dp.client(as)
			if cli == nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": providerCN(dp.Name()) + " 还没粘 Cookie"})
				return
			}
			// 添加时校验一次（顺便拿标题）：链接失效当场就能发现。
			title, _, err := cli.dbShareOverview(shareID)
			if err != nil {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "这个分享打不开：" + err.Error()})
				return
			}
			globalSources.Put(ShareSource{
				ShareID: shareID, Link: "https://www.doubao.com/drive/s/" + shareID, Title: title,
			})
			writeJSON(w, map[string]interface{}{"ok": true, "sources": globalSources.List()})

		case http.MethodDelete:
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" {
				writeJSON(w, map[string]interface{}{"ok": false, "error": "缺少 id"})
				return
			}
			globalSources.Delete(id)
			writeJSON(w, map[string]interface{}{"ok": true, "sources": globalSources.List()})

		default:
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		}
	}
}
