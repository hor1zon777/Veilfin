//go:build ffmpeg_embed

package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEmbeddedTools 解压内置的精简 ffmpeg，并用它跑完整的功能检查。
func TestEmbeddedTools(t *testing.T) {
	if !HasEmbedded() || EmbeddedVersion() == "" {
		t.Fatal("内置构建中应该带有 ffmpeg 和版本号")
	}
	root := t.TempDir()
	old := filepath.Join(root, "ffmpeg-0.0-old")
	os.MkdirAll(old, 0o755)

	dir, err := ExtractEmbedded(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(dir), "ffmpeg-"+EmbeddedVersion()+"-") {
		t.Fatalf("解压目录名不正确: %s", dir)
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("旧版本目录应该被清理")
	}
	for _, f := range []string{"LICENSE.txt", "BUILD.txt", "VERSION"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("缺少 %s", f)
		}
	}
	tl, err := Find(dir)
	if err != nil || filepath.Dir(tl.FFmpeg) != dir || filepath.Dir(tl.FFprobe) != dir {
		t.Fatalf("应该使用解压出的程序: %+v %v", tl, err)
	}
	out, err := run(t.Context(), 30*time.Second, nil, tl.FFmpeg, "-hide_banner", "-buildconf")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "--enable-gpl") || strings.Contains(string(out), "--enable-nonfree") {
		t.Fatal("内置 ffmpeg 不应包含 GPL / nonfree 组件")
	}
	t.Logf("内置 ffmpeg %s，编译参数：\n%s", EmbeddedVersion(), out)
	exerciseTools(t, tl)

	// 再次启动：直接复用；文件损坏时重新解压
	if d2, err := ExtractEmbedded(root); err != nil || d2 != dir {
		t.Fatalf("复用失败: %v", err)
	}
	if err := os.WriteFile(tl.FFprobe, []byte("broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if d3, err := ExtractEmbedded(root); err != nil || d3 != dir || !verifyExtracted(dir) {
		t.Fatalf("损坏后没有重新解压: %v", err)
	}
}
