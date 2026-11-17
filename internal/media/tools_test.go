package media

import (
	"bytes"
	"context"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exerciseTools 用 testdata 里的样片检查 Veilfin 用到的全部 ffmpeg 功能：
// 读取信息、截图、进度条预览图、内嵌字幕提取、外挂字幕转换。
// 系统 ffmpeg 和内置的精简 ffmpeg 都跑这一套，保证精简编译没有漏掉需要的组件。
func exerciseTools(t *testing.T, tl *Tools) {
	ctx := context.Background()
	jpegWidth := func(t *testing.T, b []byte) int {
		t.Helper()
		img, err := jpeg.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("不是有效的 JPEG: %v", err)
		}
		return img.Bounds().Dx()
	}

	t.Run("H.264+AAC+mov_text", func(t *testing.T) {
		p := filepath.Join("testdata", "h264.mp4")
		r, err := tl.Probe(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(r.Duration-12) > 0.5 || r.Width != 320 || r.Height != 180 || r.VCodec != "h264" || r.ACodec != "aac" {
			t.Fatalf("信息不正确: %+v", r)
		}
		if len(r.Subs) != 1 || r.Subs[0].Codec != "mov_text" || !r.Subs[0].Text || r.Subs[0].Lang != "chi" {
			t.Fatalf("字幕流不正确: %+v", r.Subs)
		}
		img, err := tl.Frame(ctx, p, 3, 160)
		if err != nil {
			t.Fatal(err)
		}
		if w := jpegWidth(t, img); w != 160 {
			t.Fatalf("截图宽度 %d", w)
		}
		full, err := tl.Frame(ctx, p, 1, 0)
		if err != nil || jpegWidth(t, full) != 320 {
			t.Fatalf("原尺寸截图失败: %v", err)
		}
		info, files, err := tl.Trickplay(ctx, p, r.Duration, r.Width, r.Height, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 || info.Count < 1 || info.Height != 136 {
			t.Fatalf("预览图不正确: %+v %v", info, files)
		}
		sprite, _ := os.ReadFile(files[0])
		if w := jpegWidth(t, sprite); w != info.Width*info.Cols {
			t.Fatalf("雪碧图宽度 %d", w)
		}
		vtt, err := tl.ExtractSubtitle(ctx, p, r.Subs[0].Index)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(vtt), "WEBVTT") || !strings.Contains(string(vtt), "内嵌字幕测试") || !strings.Contains(string(vtt), "第二句字幕") {
			t.Fatalf("字幕提取不正确:\n%s", vtt)
		}
	})

	t.Run("HEVC", func(t *testing.T) {
		p := filepath.Join("testdata", "hevc.mp4")
		r, err := tl.Probe(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if r.VCodec != "hevc" || r.Width != 320 || r.Height != 180 || r.Duration < 3.5 {
			t.Fatalf("信息不正确: %+v", r)
		}
		img, err := tl.Frame(ctx, p, 2, 0)
		if err != nil || jpegWidth(t, img) != 320 {
			t.Fatalf("HEVC 截图失败: %v", err)
		}
		if _, files, err := tl.Trickplay(ctx, p, r.Duration, r.Width, r.Height, t.TempDir()); err != nil || len(files) != 1 {
			t.Fatalf("HEVC 预览图失败: %v", err)
		}
	})

	t.Run("VP9+Opus+WebVTT", func(t *testing.T) {
		p := filepath.Join("testdata", "vp9.webm")
		r, err := tl.Probe(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if r.VCodec != "vp9" || r.ACodec != "opus" || r.Width != 320 || len(r.Subs) != 1 || r.Subs[0].Codec != "webvtt" {
			t.Fatalf("信息不正确: %+v %+v", r, r.Subs)
		}
		img, err := tl.Frame(ctx, p, 1.5, 0)
		if err != nil || jpegWidth(t, img) != 320 {
			t.Fatalf("VP9 截图失败: %v", err)
		}
		vtt, err := tl.ExtractSubtitle(ctx, p, r.Subs[0].Index)
		if err != nil || !strings.Contains(string(vtt), "WebM 字幕测试") {
			t.Fatalf("WebM 字幕提取失败: %v\n%s", err, vtt)
		}
	})

	t.Run("外挂字幕", func(t *testing.T) {
		for file, want := range map[string]string{"external.srt": "第二句字幕", "external.ass": "ASS 外挂字幕"} {
			vtt, err := tl.ConvertSubtitle(ctx, filepath.Join("testdata", file))
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			if !strings.HasPrefix(string(vtt), "WEBVTT") || !strings.Contains(string(vtt), want) {
				t.Fatalf("%s 转换不正确:\n%s", file, vtt)
			}
		}
	})
}

// TestSystemTools 用 PATH 中的 ffmpeg 跑一遍（没有安装时跳过）。
func TestSystemTools(t *testing.T) {
	tl, err := Find("")
	if err != nil {
		t.Skip("PATH 中没有 ffmpeg / ffprobe")
	}
	exerciseTools(t, tl)
}

func TestNoEmbeddedByDefault(t *testing.T) {
	if HasEmbedded() {
		t.Skip("这是内置 ffmpeg 的构建")
	}
	if EmbeddedVersion() != "" {
		t.Fatal("没有内置 ffmpeg 时版本号应为空")
	}
	if _, err := ExtractEmbedded(t.TempDir()); err == nil {
		t.Fatal("没有内置 ffmpeg 时解压应失败")
	}
}
