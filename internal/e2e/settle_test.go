package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"veilfin/internal/library"
)

// 正在复制的视频（修改时间很近）先不处理，稳定后自动补扫；时钟明显不一致的照常处理。
func TestSkipFilesBeingWritten(t *testing.T) {
	if !hasFFmpeg() {
		t.Skip("没有 ffmpeg")
	}
	e := newEnv(t)
	library.FileSettleTime = 1500 * time.Millisecond // newEnv 之后设置，Cleanup 会恢复
	admin := e.setupAdmin()
	dir := filepath.Join(e.mediaDir, "v")
	makeVideo(t, filepath.Join(dir, "刚复制.mp4"), 5, 440)
	// NAS 时钟快了一小时：修改时间在未来，不能当成“正在写入”一直不处理
	skew := filepath.Join(dir, "时钟偏差.mp4")
	makeVideo(t, skew, 5, 550)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(skew, future, future); err != nil {
		t.Fatal(err)
	}

	var lib struct {
		ID int64 `json:"id"`
	}
	admin.must("admin.libs.create", map[string]any{"name": "v", "kind": "home", "scraper": "none", "paths": []string{dir}}, &lib)
	e.waitIdle()
	var l listResult
	admin.must("items.list", map[string]any{"libraryId": lib.ID}, &l)
	if l.Total != 1 || l.Items[0].Title != "时钟偏差" {
		t.Fatalf("刚修改的文件应先跳过、时钟偏差的照常入库: %+v", l.Items)
	}

	// 稳定后自动补扫（不需要手动扫描），并且探测成功
	deadline := time.Now().Add(20 * time.Second)
	for l.Total < 2 && time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		admin.must("items.list", map[string]any{"libraryId": lib.ID}, &l)
	}
	if l.Total != 2 {
		t.Fatalf("文件稳定后没有自动补扫入库: %+v", l.Items)
	}
	e.waitIdle()
	c := findByTitle(t, l.Items, "刚复制")
	var d detail
	admin.must("items.get", map[string]any{"id": c.ID}, &d)
	if d.Duration < 4 {
		t.Fatalf("补扫入库的视频没有探测成功: %+v", d)
	}
}
