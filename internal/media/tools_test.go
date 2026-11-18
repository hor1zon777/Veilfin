package media

import (
	"bytes"
	"context"
	"image"
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
		key, err := tl.KeyFrame(ctx, p, 7.5, 160)
		if err != nil || jpegWidth(t, key) != 160 {
			t.Fatalf("关键帧截图失败: %v", err)
		}
		var files [][]byte
		var maxDone, total int
		info, err := tl.Trickplay(ctx, p, r.Duration, r.Width, r.Height, 4, func(b []byte) error { files = append(files, b); return nil },
			func(d, n int) { maxDone, total = max(maxDone, d), n })
		if err != nil {
			t.Fatal(err)
		}
		if maxDone != 2 || total != 2 {
			t.Fatalf("进度报告不正确: %d/%d", maxDone, total)
		}
		// 12 秒的视频每 10 秒一张：两张缩略图，雪碧图只有一行两列
		if len(files) != 1 || info.Count != 2 || info.Height != 136 {
			t.Fatalf("预览图不正确: %+v %d", info, len(files))
		}
		sprite, err := jpeg.Decode(bytes.NewReader(files[0]))
		if err != nil {
			t.Fatal(err)
		}
		if b := sprite.Bounds(); b.Dx() != info.Width*2 || b.Dy() != info.Height {
			t.Fatalf("雪碧图尺寸 %v", b)
		}
		// 第二张取自 10 秒处，画面应和第一张不同（确实跳转了，不是重复第一帧）
		if sameCell(sprite, info.Width, info.Height) {
			t.Fatal("两张缩略图完全相同，跳转截图没有生效")
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
		n := 0
		if _, err := tl.Trickplay(ctx, p, r.Duration, r.Width, r.Height, 2, func([]byte) error { n++; return nil }, nil); err != nil || n != 1 {
			t.Fatalf("HEVC 预览图失败: %v", err)
		}
	})

	t.Run("截不到画面", func(t *testing.T) {
		_, err := tl.Trickplay(ctx, filepath.Join("testdata", "external.srt"), 30, 320, 180, 2, func([]byte) error { return nil }, nil)
		if err == nil {
			t.Fatal("不是视频的文件应该生成失败")
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

	t.Run("多条内嵌字幕一次提取", func(t *testing.T) {
		p := filepath.Join("testdata", "multisubs.mp4")
		r, err := tl.Probe(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		var idx []int
		for _, s := range r.Subs {
			if s.Text {
				idx = append(idx, s.Index)
			}
		}
		if len(idx) != 3 {
			t.Fatalf("字幕流不正确: %+v", r.Subs)
		}
		got, err := tl.ExtractSubtitles(ctx, p, idx)
		if err != nil {
			t.Fatal(err)
		}
		want := map[int]string{idx[0]: "第二句字幕", idx[1]: "ASS 外挂字幕", idx[2]: "第二条字幕流"}
		for i, w := range want {
			if v := string(got[i]); !strings.HasPrefix(v, "WEBVTT") || !strings.Contains(v, w) {
				t.Fatalf("字幕流 %d 不正确:\n%s", i, v)
			}
		}
		// 其中一条无法提取时，退回逐条提取，其余的照常返回
		got, err = tl.ExtractSubtitles(ctx, p, []int{idx[0], 99, idx[2]})
		if err != nil || len(got) != 2 || got[idx[0]] == nil || got[idx[2]] == nil {
			t.Fatalf("逐条提取的退路不正确: %v %d", err, len(got))
		}
	})

	t.Run("转码", func(t *testing.T) {
		encs := tl.H264Encoders(ctx)
		if len(encs) == 0 {
			t.Skip("这个 ffmpeg 没有可用的 H.264 编码器")
		}
		p := filepath.Join("testdata", "h264.mp4")
		for _, enc := range encs {
			// 12 秒的视频从 4 秒开始转：每 4 秒一个关键帧、一个片段，共两个片段
			cmd := tl.TranscodeCmd(ctx, TranscodeOptions{Input: p, Start: 4, Encoder: enc, Width: 320, Height: 180,
				Short: 180, Bitrate: 300_000, SegDur: 4})
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s 转码失败: %v %s", enc, err, stderr.String())
			}
			var boxes []string
			for i := 0; i+8 <= len(out); {
				size := int(uint32(out[i])<<24 | uint32(out[i+1])<<16 | uint32(out[i+2])<<8 | uint32(out[i+3]))
				if size < 8 || i+size > len(out) {
					t.Fatalf("%s 输出的 MP4 盒子不完整", enc)
				}
				boxes = append(boxes, string(out[i+4:i+8]))
				i += size
			}
			moof := 0
			for _, b := range boxes {
				if b == "moof" {
					moof++
				}
			}
			if len(boxes) < 4 || boxes[0] != "ftyp" || boxes[1] != "moov" || moof != 2 {
				t.Fatalf("%s 输出的分片 MP4 结构不正确: %v", enc, boxes)
			}
			f := filepath.Join(t.TempDir(), "out.mp4")
			os.WriteFile(f, out, 0o644)
			r, err := tl.Probe(ctx, f)
			if err != nil || r.VCodec != "h264" || r.ACodec != "aac" || r.Height != 180 || math.Abs(r.Duration-8) > 0.5 {
				t.Fatalf("%s 转码结果不正确: %+v %v", enc, r, err)
			}
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

// sameCell 比较雪碧图第一行前两格的像素是否完全相同。
func sameCell(img image.Image, w, h int) bool {
	for y := 0; y < h; y += 4 {
		for x := 0; x < w; x += 4 {
			r1, g1, b1, _ := img.At(x, y).RGBA()
			r2, g2, b2, _ := img.At(x+w, y).RGBA()
			if absDiff(r1, r2)+absDiff(g1, g2)+absDiff(b1, b2) > 0x3000 {
				return false
			}
		}
	}
	return true
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}
