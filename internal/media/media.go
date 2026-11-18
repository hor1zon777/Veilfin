// Package media 封装对 ffmpeg / ffprobe 的调用。
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

type Tools struct {
	FFmpeg  string
	FFprobe string
}

// Find 在 dir（可选）和 PATH 中查找 ffmpeg / ffprobe。
func Find(dir string) (*Tools, error) {
	look := func(name string) (string, error) {
		if dir != "" {
			for _, n := range []string{name, name + ".exe"} {
				p := filepath.Join(dir, n)
				if st, err := os.Stat(p); err == nil && !st.IsDir() {
					return p, nil
				}
			}
		}
		return exec.LookPath(name)
	}
	ff, err := look("ffmpeg")
	if err != nil {
		return nil, errors.New("找不到 ffmpeg，请安装或用 -ffmpeg 指定所在目录")
	}
	fp, err := look("ffprobe")
	if err != nil {
		return nil, errors.New("找不到 ffprobe，请安装或用 -ffmpeg 指定所在目录")
	}
	return &Tools{FFmpeg: ff, FFprobe: fp}, nil
}

func run(ctx context.Context, timeout time.Duration, stdin []byte, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("%s 失败: %v %s", filepath.Base(name), err, msg)
	}
	return out.Bytes(), nil
}

// ProbeResult 是 ffprobe 的结果。
type ProbeResult struct {
	Duration  float64
	Width     int
	Height    int
	VCodec    string
	ACodec    string
	Container string
	Bitrate   int64
	Subs      []SubStream
}

// SubStream 是内嵌字幕流。
type SubStream struct {
	Index   int
	Codec   string
	Lang    string
	Title   string
	Default bool
	Text    bool // 文本字幕才能转成 WebVTT；图形字幕（PGS 等）跳过
}

var textSubCodecs = map[string]bool{"subrip": true, "ass": true, "ssa": true, "mov_text": true, "webvtt": true, "text": true}

func (t *Tools) Probe(ctx context.Context, path string) (*ProbeResult, error) {
	out, err := run(ctx, 60*time.Second, nil, t.FFprobe, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Format struct {
			FormatName string `json:"format_name"`
			Duration   string `json:"duration"`
			BitRate    string `json:"bit_rate"`
		} `json:"format"`
		Streams []struct {
			Index       int               `json:"index"`
			CodecType   string            `json:"codec_type"`
			CodecName   string            `json:"codec_name"`
			Width       int               `json:"width"`
			Height      int               `json:"height"`
			Tags        map[string]string `json:"tags"`
			Disposition map[string]int    `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	r := &ProbeResult{Container: raw.Format.FormatName}
	r.Duration, _ = strconv.ParseFloat(raw.Format.Duration, 64)
	r.Bitrate, _ = strconv.ParseInt(raw.Format.BitRate, 10, 64)
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			if r.VCodec == "" && s.Disposition["attached_pic"] == 0 {
				r.VCodec, r.Width, r.Height = s.CodecName, s.Width, s.Height
			}
		case "audio":
			if r.ACodec == "" {
				r.ACodec = s.CodecName
			}
		case "subtitle":
			r.Subs = append(r.Subs, SubStream{
				Index: s.Index, Codec: s.CodecName, Lang: s.Tags["language"], Title: s.Tags["title"],
				Default: s.Disposition["default"] == 1, Text: textSubCodecs[s.CodecName],
			})
		}
	}
	if math.IsNaN(r.Duration) || r.Duration < 0 {
		r.Duration = 0
	}
	return r, nil
}

// jpegFormat 把画面转成 JPEG 标准的全范围 YUV。HEVC / VP9 片源通常标记为有限范围（tv），
// 新版 ffmpeg 的 MJPEG 编码器会直接拒绝；用 -strict unofficial 绕过则会写出颜色发灰的非标准 JPEG。
const jpegFormat = "format=yuvj420p"

// Frame 精确截取 at 秒处的一帧，输出 JPEG（宽度 width，0 表示原尺寸）。
// 需要从前一个关键帧解码到 at，用于手动挑选画面。
func (t *Tools) Frame(ctx context.Context, path string, at float64, width int) ([]byte, error) {
	return t.frame(ctx, path, at, width, false)
}

// KeyFrame 截取 at 秒之前最近的关键帧，输出 JPEG（宽度 width，0 表示原尺寸）。
// 只读取并解码这一帧，不解码到精确时间点，用于自动截封面。
func (t *Tools) KeyFrame(ctx context.Context, path string, at float64, width int) ([]byte, error) {
	return t.frame(ctx, path, at, width, true)
}

func (t *Tools) frame(ctx context.Context, path string, at float64, width int, key bool) ([]byte, error) {
	args := []string{"-v", "error"}
	if key {
		args = append(args, "-noaccurate_seek")
	}
	args = append(args, "-ss", strconv.FormatFloat(math.Max(at, 0), 'f', 3, 64))
	if key {
		args = append(args, "-skip_frame", "nokey")
	}
	args = append(args, "-i", path, "-an", "-sn", "-dn", "-frames:v", "1")
	vf := "scale=out_range=pc," + jpegFormat
	if width > 0 {
		vf = fmt.Sprintf("scale='min(%d,iw)':-2:out_range=pc,", width) + jpegFormat
	}
	args = append(args, "-vf", vf, "-q:v", "3", "-f", "image2", "-c:v", "mjpeg", "pipe:1")
	out, err := run(ctx, 60*time.Second, nil, t.FFmpeg, args...)
	if err == nil && len(out) == 0 {
		err = errors.New("截图为空（时间点可能超出视频长度）")
	}
	return out, err
}

// TrickplayInfo 描述进度条预览图的布局。
type TrickplayInfo struct {
	Interval float64 `json:"interval"` // 每张缩略图间隔秒数
	Cols     int     `json:"cols"`
	Rows     int     `json:"rows"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Count    int     `json:"count"` // 缩略图总数
}

// TrickplayQuality 是雪碧图的 JPEG 质量。
const TrickplayQuality = 80

// TrickplayLayout 计算进度条预览图的布局：默认每 10 秒一张，最多 2000 张。
func TrickplayLayout(duration float64, vw, vh int) TrickplayInfo {
	info := TrickplayInfo{Interval: 10, Cols: 10, Rows: 10, Width: 240, Height: 135}
	if n := duration / info.Interval; n > 2000 {
		info.Interval = math.Ceil(duration / 2000)
	}
	if vw > 0 && vh > 0 {
		info.Height = max(int(math.Round(float64(info.Width)*float64(vh)/float64(vw)/2))*2, 2)
	}
	info.Count = max(int(math.Ceil(duration/info.Interval)), 1)
	return info
}

// TrickFrame 跳到 at 秒之前最近的关键帧，只解码这一帧并缩放到 w×h。
// 输入端 -ss 借助容器索引直接定位，只读取这一小段数据，不会把整个文件读一遍。
func (t *Tools) TrickFrame(ctx context.Context, path string, at float64, w, h int) (image.Image, error) {
	vf := fmt.Sprintf("scale=%d:%d:flags=area:out_range=pc,%s", w, h, jpegFormat)
	out, err := run(ctx, time.Minute, nil, t.FFmpeg, "-v", "error",
		"-noaccurate_seek", "-ss", strconv.FormatFloat(math.Max(at, 0), 'f', 3, 64),
		"-skip_frame", "nokey", "-threads", "1", "-i", path,
		"-an", "-sn", "-dn", "-frames:v", "1", "-vf", vf, "-q:v", "2", "-f", "image2", "-c:v", "mjpeg", "pipe:1")
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("没有截到画面")
	}
	return jpeg.Decode(bytes.NewReader(out))
}

// Trickplay 生成进度条预览雪碧图。每个时间点单独跳转截取一帧（见 TrickFrame），parallel 个 ffmpeg 并行；
// 每拼好一张雪碧图就以 JPEG 调用 save（按顺序）；progress（可为 nil）在每截完一个时间点后报告进度。
// 返回最终的布局信息。
func (t *Tools) Trickplay(ctx context.Context, path string, duration float64, vw, vh, parallel int, save func(sprite []byte) error, progress func(done, total int)) (*TrickplayInfo, error) {
	if duration <= 0 {
		return nil, errors.New("时长未知，无法生成预览图")
	}
	info := TrickplayLayout(duration, vw, vh)
	parallel = max(parallel, 1)
	per := info.Cols * info.Rows
	sheets := (info.Count + per - 1) / per
	var last image.Image // 上一张成功截到的画面，用来填补个别失败的时间点
	for sh := 0; sh < sheets; sh++ {
		start := sh * per
		n := min(per, info.Count-start)
		frames, firstErr := t.trickFrames(ctx, path, info, start, n, parallel, sh == 0, progress)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lastOK := -1
		for i, f := range frames {
			if f != nil {
				lastOK = i
			}
		}
		if lastOK < 0 {
			if sh == 0 {
				return nil, fmt.Errorf("无法从视频中截取画面: %v", firstErr)
			}
			info.Count = start // 后面的部分（例如文件末尾损坏）截不到画面：到此为止
			break
		}
		if sh == sheets-1 {
			n = lastOK + 1 // 末尾截不到的时间点直接去掉
			info.Count = start + n
		}
		frames = frames[:n]
		// 填补失败的时间点：用前一张；开头就失败的用第一张成功的
		for i := range frames {
			if frames[i] == nil {
				frames[i] = last
			} else {
				last = frames[i]
			}
		}
		for i := range frames {
			if frames[i] == nil {
				frames[i] = frames[lastOK]
			}
		}
		rows := (n + info.Cols - 1) / info.Cols // 最后一张雪碧图只保留用到的行
		cols := min(n, info.Cols)
		dst := image.NewRGBA(image.Rect(0, 0, cols*info.Width, rows*info.Height))
		for i, f := range frames {
			x, y := (i%info.Cols)*info.Width, (i/info.Cols)*info.Height
			draw.Draw(dst, image.Rect(x, y, x+info.Width, y+info.Height), f, f.Bounds().Min, draw.Src)
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: TrickplayQuality}); err != nil {
			return nil, err
		}
		if err := save(buf.Bytes()); err != nil {
			return nil, err
		}
	}
	if info.Count <= 0 {
		return nil, errors.New("没有生成预览图")
	}
	return &info, nil
}

// trickFrames 并行截取第 start 张起的 n 张缩略图，失败的位置为 nil。
// probe 为 true 时（第一张雪碧图），前几张全部失败就提前放弃（多半是解码器不支持或文件损坏）。
func (t *Tools) trickFrames(ctx context.Context, path string, info TrickplayInfo, start, n, parallel int, probe bool, progress func(done, total int)) ([]image.Image, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	frames := make([]image.Image, n)
	var (
		mu       sync.Mutex
		firstErr error
		fails    int
		oks      int
		next     int
	)
	var wg sync.WaitGroup
	for range min(parallel, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if i >= n || ctx.Err() != nil {
					return
				}
				img, err := t.TrickFrame(ctx, path, float64(start+i)*info.Interval, info.Width, info.Height)
				mu.Lock()
				if err == nil && (img.Bounds().Dx() != info.Width || img.Bounds().Dy() != info.Height) {
					err = errors.New("画面尺寸不符")
				}
				if err != nil {
					fails++
					if firstErr == nil {
						firstErr = err
					}
					if probe && oks == 0 && fails >= 8 {
						cancel()
					}
				} else {
					oks++
					frames[i] = img
				}
				if progress != nil {
					progress(start+oks+fails, info.Count)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return frames, firstErr
}

// ExtractSubtitle 把内嵌文本字幕流转换成 WebVTT。
func (t *Tools) ExtractSubtitle(ctx context.Context, path string, index int) ([]byte, error) {
	return run(ctx, 5*time.Minute, nil, t.FFmpeg, "-v", "error", "-i", path, "-map", "0:"+strconv.Itoa(index), "-f", "webvtt", "pipe:1")
}

// ExtractSubtitles 一次读取文件，同时把多条内嵌文本字幕流转换成 WebVTT，返回 流序号 → 内容。
// 字幕数据分散在整个文件里，每提取一条都要把文件读一遍；合在一个 ffmpeg 里只需要读一遍。
// 合并提取失败时（例如某条字幕流损坏）退回逐条提取，失败的那条不出现在结果里。
func (t *Tools) ExtractSubtitles(ctx context.Context, path string, indexes []int) (map[int][]byte, error) {
	out := map[int][]byte{}
	if len(indexes) == 0 {
		return out, nil
	}
	if len(indexes) > 1 {
		dir, err := os.MkdirTemp("", "vf-subs-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		args := []string{"-v", "error", "-i", path}
		for _, i := range indexes {
			args = append(args, "-map", "0:"+strconv.Itoa(i), "-f", "webvtt", "-y", filepath.Join(dir, strconv.Itoa(i)+".vtt"))
		}
		if _, err := run(ctx, 30*time.Minute, nil, t.FFmpeg, args...); err == nil {
			for _, i := range indexes {
				if b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(i)+".vtt")); err == nil {
					out[i] = b
				}
			}
			return out, nil
		} else if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	var firstErr error
	for _, i := range indexes {
		b, err := t.ExtractSubtitle(ctx, path, i)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out[i] = b
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// ConvertSubtitle 把外挂字幕（srt/ass/ssa/vtt）转换成 UTF-8 的 WebVTT，自动处理 GBK / UTF-16 编码。
func (t *Tools) ConvertSubtitle(ctx context.Context, file string) ([]byte, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	text, err := ToUTF8(raw)
	if err != nil {
		return nil, err
	}
	format := map[string]string{".srt": "srt", ".ass": "ass", ".ssa": "ass", ".vtt": "webvtt"}[strings.ToLower(filepath.Ext(file))]
	if format == "" {
		return nil, errors.New("不支持的字幕格式")
	}
	return run(ctx, 2*time.Minute, text, t.FFmpeg, "-v", "error", "-f", format, "-i", "pipe:0", "-f", "webvtt", "pipe:1")
}

// ToUTF8 识别常见编码（UTF-8、UTF-16 BOM、GB18030/GBK）并转成 UTF-8。
func ToUTF8(b []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return b[3:], nil
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}), bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		out, _, err := transform.Bytes(unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder(), b)
		return out, err
	case utf8.Valid(b):
		return b, nil
	default:
		out, _, err := transform.Bytes(simplifiedchinese.GB18030.NewDecoder(), b)
		return out, err
	}
}
