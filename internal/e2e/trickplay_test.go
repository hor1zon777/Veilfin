package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 进度条预览图按媒体库开关；多条内嵌字幕一次提取。
func TestLibraryTrickplayAndEmbeddedSubs(t *testing.T) {
	if !hasFFmpeg() {
		t.Skip("没有 ffmpeg")
	}
	e := newEnv(t)
	admin := e.setupAdmin()
	on := filepath.Join(e.mediaDir, "on")
	off := filepath.Join(e.mediaDir, "off")
	makeVideo(t, filepath.Join(on, "开启.mp4"), 30, 440)
	makeVideo(t, filepath.Join(off, "关闭.mp4"), 30, 550)
	subs, err := os.ReadFile(filepath.Join("..", "media", "testdata", "multisubs.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(on, "多字幕.mp4"), subs, 0o644)

	var onLib, offLib struct {
		ID int64 `json:"id"`
	}
	admin.must("admin.libs.create", map[string]any{"name": "开启", "kind": "movie", "scraper": "none", "trickplay": true, "paths": []string{on}}, &onLib)
	admin.must("admin.libs.create", map[string]any{"name": "关闭", "kind": "home", "scraper": "none", "trickplay": false, "paths": []string{off}}, &offLib)
	e.waitIdle()

	item := func(lib int64, title string) detail {
		t.Helper()
		var l listResult
		admin.must("items.list", map[string]any{"libraryId": lib, "limit": 10}, &l)
		c := findByTitle(t, l.Items, title)
		var d detail
		admin.must("items.get", map[string]any{"id": c.ID}, &d)
		return d
	}
	if !item(onLib.ID, "开启").HasTrickplay {
		t.Fatal("开启预览图的媒体库没有生成预览图")
	}
	offItem := item(offLib.ID, "关闭")
	if offItem.HasTrickplay {
		t.Fatal("关闭预览图的媒体库不应生成预览图")
	}

	// 关闭的媒体库：手动刷新、打开播放页都不会生成
	admin.must("items.refresh", map[string]any{"id": offItem.ID, "trickplay": true}, nil)
	admin.must("play.open", map[string]any{"id": offItem.ID}, nil)
	e.waitIdle()
	if item(offLib.ID, "关闭").HasTrickplay {
		t.Fatal("关闭预览图的媒体库在刷新 / 播放后生成了预览图")
	}

	// 媒体库列表返回开关状态；改为开启后补上预览图
	var libs []map[string]any
	admin.must("admin.libs.list", nil, &libs)
	var offCfg map[string]any
	for _, l := range libs {
		if int64(l["id"].(float64)) == offLib.ID {
			offCfg = l
		}
	}
	if offCfg == nil || offCfg["trickplay"] != false {
		t.Fatalf("媒体库列表中的预览图开关不正确: %v", offCfg)
	}
	offCfg["trickplay"] = true
	admin.must("admin.libs.update", offCfg, nil)
	e.waitIdle()
	if !item(offLib.ID, "关闭").HasTrickplay {
		t.Fatal("媒体库开启预览图后没有补上")
	}

	// 三条内嵌文本字幕都提取出来
	d := item(onLib.ID, "多字幕")
	n := 0
	for _, s := range d.Subtitles {
		if strings.HasSuffix(s.Label, "（内嵌）") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("内嵌字幕数量 %d，期望 3: %+v", n, d.Subtitles)
	}
}
