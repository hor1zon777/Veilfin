package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type scanProgress struct {
	LibraryID  int64  `json:"libraryId"`
	Phase      string `json:"phase"`
	Found      int    `json:"found"`
	Added      int    `json:"added"`
	Removed    int    `json:"removed"`
	Total      int    `json:"total"`
	Done       int    `json:"done"`
	Failed     int    `json:"failed"`
	TrickTotal int    `json:"trickTotal"`
	TrickDone  int    `json:"trickDone"`
	Err        string `json:"err"`
}

// waitScan 轮询直到媒体库的扫描进度结束，返回最终进度和途中见过的阶段。
func waitScan(t *testing.T, u *user, libID int64) (scanProgress, map[string]bool) {
	t.Helper()
	phases := map[string]bool{}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		var list []scanProgress
		u.must("admin.libs.progress", nil, &list)
		for _, p := range list {
			if p.LibraryID != libID {
				continue
			}
			phases[p.Phase] = true
			if p.Done > p.Total || p.TrickDone > p.TrickTotal {
				t.Fatalf("进度超过总数: %+v", p)
			}
			if p.Phase == "done" || p.Phase == "failed" {
				return p, phases
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("等待扫描完成超时")
	return scanProgress{}, nil
}

// 媒体库扫描进度：扫描文件 → 处理条目 → 生成预览图 → 完成，以及删除文件后的重新扫描。
func TestScanProgress(t *testing.T) {
	if !hasFFmpeg() {
		t.Skip("需要 ffmpeg")
	}
	e := newEnv(t)
	admin := e.setupAdmin()
	dir := filepath.Join(e.mediaDir, "progress")
	for i, name := range []string{"a.mp4", "b.mp4", "sub/c.mp4"} {
		makeVideo(t, filepath.Join(dir, name), 12, 300+i*100)
	}
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("不是视频"), 0o644)

	var lib struct {
		ID int64 `json:"id"`
	}
	admin.must("admin.libs.create", map[string]any{"name": "进度", "kind": "home", "scraper": "none", "trickplay": true, "paths": []string{dir}}, &lib)
	p, phases := waitScan(t, admin, lib.ID)
	if p.Phase != "done" || p.Found != 3 || p.Added != 3 || p.Total != 3 || p.Done != 3 || p.Failed != 0 {
		t.Fatalf("首次扫描进度不正确: %+v", p)
	}
	if p.TrickTotal != 3 || p.TrickDone != 3 {
		t.Fatalf("预览图进度不正确: %+v", p)
	}
	if !phases["processing"] {
		t.Fatalf("应该经历处理阶段: %v", phases)
	}
	e.waitIdle()

	// 没有变化的重新扫描：不需要处理任何条目，立即完成
	admin.must("admin.libs.scan", map[string]any{"id": lib.ID}, nil)
	p, _ = waitScan(t, admin, lib.ID)
	// 刚触发时可能还读到上一次的“完成”，等新的一次开始
	for i := 0; i < 50 && p.Added != 0; i++ {
		time.Sleep(20 * time.Millisecond)
		p, _ = waitScan(t, admin, lib.ID)
	}
	if p.Phase != "done" || p.Found != 3 || p.Added != 0 || p.Total != 0 || p.Removed != 0 {
		t.Fatalf("无变化的扫描进度不正确: %+v", p)
	}

	// 删除一个文件后扫描
	os.Remove(filepath.Join(dir, "b.mp4"))
	admin.must("admin.libs.scan", map[string]any{"id": lib.ID}, nil)
	for i := 0; i < 100; i++ {
		if p, _ = waitScan(t, admin, lib.ID); p.Removed == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p.Phase != "done" || p.Found != 2 || p.Removed != 1 {
		t.Fatalf("删除文件后的扫描进度不正确: %+v", p)
	}

	// 刷新元数据：所有条目都重新处理
	admin.must("admin.libs.scan", map[string]any{"id": lib.ID, "refresh": true}, nil)
	for i := 0; i < 200; i++ {
		if p, _ = waitScan(t, admin, lib.ID); p.Total == 2 && p.Phase == "done" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p.Total != 2 || p.Done != 2 {
		t.Fatalf("刷新元数据的进度不正确: %+v", p)
	}
}
