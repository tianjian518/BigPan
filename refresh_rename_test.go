package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// 假网盘：RefreshShare 返回预设的文件清单，用来驱动 refreshShareOnce。
type fakeProvider struct {
	name  string
	files []ShareFile
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) MatchLink(string) bool {
	return strings.HasPrefix("x", "y") // 恒 false，不影响其他测试
}
func (f *fakeProvider) ListShareFiles(string) (ShareInfo, error) {
	return ShareInfo{}, fmt.Errorf("测试桩不实现")
}
func (f *fakeProvider) RefreshShare(shareID, passcode string) (string, []ShareFile, error) {
	return "测试分享", f.files, nil
}
func (f *fakeProvider) Wants302() bool { return true }
func (f *fakeProvider) ResolvePlay(*Item) (PlayTarget, error) {
	return PlayTarget{}, fmt.Errorf("stub")
}

func newFakeStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "items.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sf(id, name, path string) ShareFile {
	return ShareFile{ID: id, Name: name, Path: path, Size: 100}
}

// 场景一：分享主把《流浪地球》改名《流浪地球1080P》——应原地改名，不新增。
func TestRefreshRenameFollowsToken(t *testing.T) {
	s := newFakeStore(t)
	p := &fakeProvider{name: "faket", files: []ShareFile{sf("tok-1", "流浪地球.mkv", "流浪地球.mkv")}}
	registerProvider(p)

	// 首次导入
	if _, added, _, err := refreshShareOnce(s, "faket", "shr-test", "", ""); err != nil || added != 1 {
		t.Fatalf("首次导入: added=%d err=%v", added, err)
	}
	first := s.List()[0]

	// 分享主改名（token 不变，名字变了）
	p.files = []ShareFile{sf("tok-1", "流浪地球1080P.mkv", "流浪地球1080P.mkv")}
	show, added, total, err := refreshShareOnce(s, "faket", "shr-test", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Fatalf("改名不应新增条目，实际新增 %d", added)
	}
	items := s.List()
	if len(items) != 1 {
		t.Fatalf("应仍只有 1 条，实际 %d", len(items))
	}
	it := items[0]
	if it.ID != first.ID {
		t.Fatal("应是同一条目原地更新（ID 不变）")
	}
	if it.Title != "流浪地球1080P" || !strings.HasSuffix(it.FileName(), ".mkv") {
		t.Fatalf("标题没跟上：%q", it.Title)
	}
	// 平铺单文件没有子目录：Dir 仍是 盘/剧名，变化体现在 Title（=文件名）上
	if it.Dir != "faket/测试分享" {
		t.Fatalf("Dir 不应变化：%q", it.Dir)
	}
	if show != "测试分享" || total != 1 {
		t.Fatalf("show=%q total=%d", show, total)
	}
}

// 场景二：分享主新增一部 + 改名一部——新增和改名同时发生，各司其职。
func TestRefreshRenamePlusAdd(t *testing.T) {
	s := newFakeStore(t)
	p := &fakeProvider{name: "faket2", files: []ShareFile{
		sf("tok-a", "让子弹飞.mkv", "让子弹飞.mkv"),
		sf("tok-b", "鬼子来了.mkv", "鬼子来了.mkv"),
	}}
	registerProvider(p)
	refreshShareOnce(s, "faket2", "shr-x", "", "")

	p.files = []ShareFile{
		sf("tok-a", "让子弹飞4K.mkv", "让子弹飞4K.mkv"), // 改名
		sf("tok-b", "鬼子来了.mkv", "鬼子来了.mkv"),     // 没动
		sf("tok-c", "活着.mkv", "活着.mkv"),         // 新增
	}
	_, added, _, err := refreshShareOnce(s, "faket2", "shr-x", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("只应新增 1 条（活着），实际 %d", added)
	}
	if n := len(s.List()); n != 3 {
		t.Fatalf("总共应 3 条，实际 %d", n)
	}
	names := map[string]bool{}
	for _, it := range s.List() {
		names[it.Title] = true
	}
	if !names["让子弹飞4K"] || !names["鬼子来了"] || !names["活着"] {
		t.Fatalf("条目不对：%v", names)
	}
	if names["让子弹飞"] {
		t.Fatal("旧名字《让子弹飞》不应残留")
	}
}

// 场景三：文件被挪进子文件夹（位置变了、名字没变）——原地改路径。
func TestRefreshMoveFollowsToken(t *testing.T) {
	s := newFakeStore(t)
	// 两个文件都在根，避免「单条目顶层夹被当分享容器剥掉」的规则干扰
	p := &fakeProvider{name: "faket3", files: []ShareFile{
		sf("tok-m", "阿凡达.mkv", "阿凡达.mkv"),
		sf("tok-n", "保卫战.mkv", "保卫战.mkv"),
	}}
	registerProvider(p)
	refreshShareOnce(s, "faket3", "shr-m", "", "")

	// 阿凡达被挪进「新片场」夹，保卫战没动 → 公共根不再存在，剥离不生效
	p.files = []ShareFile{
		sf("tok-m", "阿凡达.mkv", "新片场/阿凡达.mkv"),
		sf("tok-n", "保卫战.mkv", "保卫战.mkv"),
	}
	if _, added, _, err := refreshShareOnce(s, "faket3", "shr-m", "", ""); err != nil || added != 0 {
		t.Fatalf("挪位不应新增：added=%d err=%v", added, err)
	}
	if n := len(s.List()); n != 2 {
		t.Fatalf("应仍 2 条，实际 %d", n)
	}
	var avator *Item
	for i := range s.List() {
		if s.List()[i].FileID == "tok-m" {
			avator = &s.List()[i]
		}
	}
	if avator == nil || !strings.Contains(avator.Dir, "新片场") {
		t.Fatalf("路径没跟上挪位：%+v", avator)
	}
}
