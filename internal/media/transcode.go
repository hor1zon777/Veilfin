package media

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 播放时转码用到的 H.264 编码器，按优先级排列。内置的精简 ffmpeg（LGPL）带的是 OpenH264（BSD），
// 画质不如 x264，所以排在最后：系统 ffmpeg 有 libx264 或可用的硬件编码器时优先用它们。
var h264Encoders = []string{"libx264", "h264_nvenc", "h264_qsv", "h264_amf", "h264_videotoolbox", "libopenh264"}

// EncoderLabel 返回编码器的中文说明。
func EncoderLabel(name string) string {
	switch name {
	case "libx264":
		return "libx264（CPU 软件编码）"
	case "h264_nvenc":
		return "NVIDIA NVENC"
	case "h264_qsv":
		return "Intel Quick Sync"
	case "h264_amf":
		return "AMD AMF"
	case "h264_videotoolbox":
		return "Apple VideoToolbox"
	case "libopenh264":
		return "OpenH264（CPU 软件编码）"
	}
	return name
}

type encoderProbe struct {
	once sync.Once
	list []string
}

var encProbes sync.Map // ffmpeg 路径 → *encoderProbe

// H264Encoders 返回这个 ffmpeg 实际能用的 H.264 编码器（结果按 ffmpeg 路径缓存）。
// 硬件编码器即使编进了 ffmpeg，没有对应显卡 / 驱动也用不了，所以每个都真的编码一小段试一下。
// 没有 AAC 编码器或 MP4 复用器时视为不能转码。
func (t *Tools) H264Encoders(ctx context.Context) []string {
	v, _ := encProbes.LoadOrStore(t.FFmpeg, &encoderProbe{})
	p := v.(*encoderProbe)
	p.once.Do(func() { p.list = t.probeEncoders(ctx) })
	return p.list
}

func (t *Tools) probeEncoders(ctx context.Context) []string {
	out, err := run(ctx, 20*time.Second, nil, t.FFmpeg, "-hide_banner", "-encoders")
	if err != nil {
		return nil
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && len(f[0]) == 6 {
			have[f[1]] = true
		}
	}
	if !have["aac"] {
		return nil
	}
	mux, err := run(ctx, 20*time.Second, nil, t.FFmpeg, "-hide_banner", "-muxers")
	if err != nil || !strings.Contains(string(mux), " mp4 ") {
		return nil
	}
	lavfi := false
	if demux, err := run(ctx, 20*time.Second, nil, t.FFmpeg, "-hide_banner", "-formats"); err == nil {
		lavfi = strings.Contains(string(demux), " lavfi ")
	}
	var list []string
	for _, e := range h264Encoders {
		if !have[e] {
			continue
		}
		if lavfi {
			// 用测试画面编码 5 帧
			_, err := run(ctx, 20*time.Second, nil, t.FFmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi",
				"-i", "color=c=black:s=256x144:r=25:d=0.2", "-pix_fmt", "yuv420p", "-c:v", e, "-f", "null", "-")
			if err != nil {
				continue
			}
		}
		list = append(list, e)
	}
	return list
}

// TranscodeOptions 是一次转码的参数。
type TranscodeOptions struct {
	Input   string
	Start   float64 // 从源视频的这个时间开始（秒），精确跳转
	Encoder string
	Width   int   // 源视频宽高，用来决定按哪条边缩放
	Height  int   //
	Short   int   // 输出视频短边的像素数（不放大）
	Bitrate int64 // 视频码率（bps）
	SegDur  float64
}

// TranscodeCmd 构造转码命令：输出到 stdout 的分片 MP4（fMP4），每 SegDur 秒强制一个关键帧、切一个分片。
// 不用 B 帧：B 帧会让第一帧的显示时间比解码时间晚两帧，音画差几十毫秒。
// 分片的时间戳由调用方改写成源视频中的绝对时间（MP4 复用器总是从 0 开始）。
func (t *Tools) TranscodeCmd(ctx context.Context, o TranscodeOptions) *exec.Cmd {
	scale := "scale=-2:'min(" + strconv.Itoa(o.Short) + ",ih)'"
	if o.Height > o.Width { // 竖屏视频按宽度缩放
		scale = "scale='min(" + strconv.Itoa(o.Short) + ",iw)':-2"
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error"}
	if o.Start > 0 {
		args = append(args, "-ss", strconv.FormatFloat(o.Start, 'f', 3, 64))
	}
	br := o.Bitrate
	args = append(args, "-i", o.Input,
		"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1",
		"-vf", scale+",format=yuv420p",
		"-c:v", o.Encoder, "-b:v", strconv.FormatInt(br, 10), "-maxrate", strconv.FormatInt(br*3/2, 10),
		"-bufsize", strconv.FormatInt(br*2, 10), "-bf", "0", "-g", "9999",
		"-force_key_frames", "expr:gte(t,n_forced*"+strconv.FormatFloat(o.SegDur, 'f', -1, 64)+")")
	switch o.Encoder {
	case "libx264":
		args = append(args, "-preset", "veryfast", "-profile:v", "high", "-sc_threshold", "0")
	case "h264_nvenc":
		args = append(args, "-preset", "p4", "-profile:v", "high", "-no-scenecut", "1", "-forced-idr", "1")
	case "h264_qsv":
		args = append(args, "-preset", "veryfast", "-profile:v", "high", "-look_ahead", "0", "-forced_idr", "1")
	case "h264_amf":
		args = append(args, "-usage", "transcoding", "-quality", "speed", "-profile:v", "high", "-forced_idr", "1")
	case "h264_videotoolbox":
		args = append(args, "-realtime", "1", "-profile:v", "high")
	case "libopenh264":
		// 默认的码率控制是“质量优先”，不按 -b:v 走
		args = append(args, "-profile:v", "high", "-rc_mode", "bitrate", "-allow_skip_frames", "0")
	}
	args = append(args, "-c:a", "aac", "-b:a", "128k", "-ac", "2",
		"-f", "mp4", "-movflags", "frag_keyframe+empty_moov+default_base_moof", "-use_editlist", "0", "pipe:1")
	cmd := exec.CommandContext(ctx, t.FFmpeg, args...)
	hideWindow(cmd)
	return cmd
}
