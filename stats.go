package main

// ---------------------------------------------------------------------------
// 播放健康统计 + 降速探针
//
// 背景（2026-10-22 与用户定调）：
//   UC/夸克这类「TV 逆向接口」的白嫖，官方掌握着速度通道和接口开关，
//   既没有计量额度可算，也不会提前通知。所以不能靠猜、只能靠数据：
//     ① 统计：每次真正解析直链，记成功/失败（按网盘 + 按天），落盘 7 天；
//     ② 探针：每 3 小时拿「最近播过的那部 UC 片」拉 2MB 实测速度，
//        速度掉档（如从 2.5MB/s 掉到 145KB/s）能在当天日志里看出来。
//
// 探针成本：2MB × 8 次/天 ≈ 16MB/天，可忽略；只测 UC，不打扰其他网盘。
// 302 模式下数据不经过本机，这是唯一能提前发现「悄悄限速」的手段。
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type playStat struct {
	OK        int    `json:"ok"`
	Fail      int    `json:"fail"`
	LastError string `json:"last_error,omitempty"`
}

var (
	statsMu   sync.Mutex
	statsData = map[string]map[string]*playStat{} // provider -> date -> stat
	statsFile string

	probeMu   sync.Mutex
	probeItem *Item // 最近一次成功解析的 UC 条目（探针用）

	statsLastSave time.Time // 落盘节流：飞牛是 5400rpm 机械盘，不能每次播放都写
)

// 落盘最小间隔：10 秒内的连续解析合并成一次写盘（崩溃最多丢 10 秒计数）。
const statsSaveInterval = 10 * time.Second

const statsKeepDays = 7

// initPlayStats 挂载统计文件（data 目录由 main 传入，与 accounts.json 同目录）。
func initPlayStats(path string) {
	statsFile = path
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &statsData)
	}
}

// statPlay 记一次解析结果。provider 空则按 "unknown" 归类。
func statPlay(provider string, ok bool, err error) {
	if provider == "" {
		provider = "unknown"
	}
	day := time.Now().Format("2006-01-02")
	statsMu.Lock()
	if statsData[provider] == nil {
		statsData[provider] = map[string]*playStat{}
	}
	st := statsData[provider][day]
	if st == nil {
		st = &playStat{}
		statsData[provider][day] = st
	}
	if ok {
		st.OK++
	} else {
		st.Fail++
		if err != nil {
			st.LastError = truncate(err.Error(), 160)
		}
	}
	snapshotOK, snapshotFail := st.OK, st.Fail
	needSave := true
	statsMu.Unlock()

	if needSave {
		statsMu.Lock()
		due := time.Since(statsLastSave) >= statsSaveInterval
		if due {
			statsLastSave = time.Now()
		}
		statsMu.Unlock()
		if due {
			savePlayStats()
		}
	}
	if !ok {
		log.Printf("[统计] %s 今日 %d 成功 / %d 失败 | 最近错误: %v", provider, snapshotOK, snapshotFail, err)
	}
}

// savePlayStats 落盘（先剪掉超过 7 天的，避免文件无限增长）。
func savePlayStats() {
	if statsFile == "" {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -statsKeepDays).Format("2006-01-02")
	statsMu.Lock()
	for p, days := range statsData {
		for d := range days {
			if d < cutoff {
				delete(days, d)
			}
		}
		if len(days) == 0 {
			delete(statsData, p)
		}
	}
	b, err := json.MarshalIndent(statsData, "", " ")
	statsMu.Unlock()
	if err != nil {
		return
	}
	tmp := statsFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0644); err == nil {
		_ = os.Rename(tmp, statsFile)
	}
}

// playStatsSnapshot 返回可展示的快照（按网盘 + 日期排序）。
func playStatsSnapshot() []map[string]interface{} {
	statsMu.Lock()
	defer statsMu.Unlock()
	out := make([]map[string]interface{}, 0)
	for p, days := range statsData {
		keys := make([]string, 0, len(days))
		for d := range days {
			keys = append(keys, d)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		for _, d := range keys {
			st := days[d]
			out = append(out, map[string]interface{}{
				"provider": p, "date": d,
				"ok": st.OK, "fail": st.Fail, "last_error": st.LastError,
			})
		}
	}
	return out
}

// apiPlayStats GET /api/stats —— 给页面/排查用，只读。
func apiPlayStats() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{"ok": true, "stats": playStatsSnapshot()})
	}
}

// rememberProbeItem 记住最近一次成功解析的 UC 条目（探针取样用）。
func rememberProbeItem(it *Item) {
	if it == nil || !strings.EqualFold(it.Provider, "uc") {
		return
	}
	cp := *it
	probeMu.Lock()
	probeItem = &cp
	probeMu.Unlock()
}

// runUCProbe 实测一次 UC 速度（拉 2MB），返回一句话结果（供日志/接口复用）。
func runUCProbe() (string, error) {
	probeMu.Lock()
	it := probeItem
	probeMu.Unlock()
	if it == nil {
		return "", fmt.Errorf("还没播过 UC 条目，无法取样（先播一次即可）")
	}
	p := providerByName("uc")
	if p == nil {
		return "", fmt.Errorf("uc provider 未注册")
	}
	t0 := time.Now()
	rp, err := p.ResolvePlay(it)
	if err != nil {
		probeMu.Lock()
		probeItem = nil
		probeMu.Unlock()
		return "", fmt.Errorf("解析失败：%w", err)
	}
	ms := time.Since(t0).Milliseconds()

	req, err := http.NewRequest(http.MethodGet, rp.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Range", "bytes=0-2097151")
	req.Header.Set("User-Agent", "bigpan-probe/1.0")
	c := &http.Client{Timeout: 30 * time.Second}
	t1 := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("取流失败：%w", err)
	}
	defer resp.Body.Close()
	n := 0
	buf := make([]byte, 64<<10)
	for n < 2<<20 {
		k, e := resp.Body.Read(buf)
		n += k
		if e != nil {
			break
		}
	}
	dt := time.Since(t1).Seconds()
	if dt <= 0 {
		return "", fmt.Errorf("测速时间异常")
	}
	mbps := float64(n) / 1024 / 1024 / dt
	msg := fmt.Sprintf("解析 %dms | %.2f MB/s（%.0f Mbps，样本 %.1fMB）", ms, mbps, mbps*8, float64(n)/1024/1024)
	if mbps < 1.0 {
		msg += " ⚠️ 低于 1MB/s，可能被限速或接口降级"
	}
	return msg, nil
}

// startSpeedProbe 每 3 小时实测一次 UC 速度。
func startSpeedProbe() {
	goSafeLoop("UC速度探针", 3*time.Hour, func() {
		if msg, err := runUCProbe(); err != nil {
			log.Printf("[探针] UC %v", err)
		} else {
			log.Printf("[探针] UC %s", msg)
		}
	})
}

// apiProbeNow POST /api/stats/probe —— 手动触发一次探针（排查用）。
func apiProbeNow() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}
		msg, err := runUCProbe()
		if err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true, "result": msg})
	}
}
