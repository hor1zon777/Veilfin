package e2e

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func hasLibx264() bool {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	return err == nil && bytes.Contains(out, []byte(" libx264 "))
}

// firstVideoPTS 用 ffprobe 读出 init + 分片拼成的文件里第一个视频帧的时间。
func firstVideoPTS(t *testing.T, data []byte) float64 {
	t.Helper()
	f := filepath.Join(t.TempDir(), "seg.mp4")
	os.WriteFile(f, data, 0o644)
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts_time",
		"-read_intervals", "%+#1", "-of", "csv=p=0", f).Output()
	if err != nil {
		t.Fatalf("ffprobe 失败: %v", err)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(strings.Split(string(out), "\n")[0]), 64)
	if err != nil {
		t.Fatalf("解析时间失败: %q", out)
	}
	return v
}

// 播放时转码：画质选项、HLS 播放列表、分片时间戳为绝对时间、拖动后重新转码、关闭会话、CDN 看不到明文。
func TestTranscode(t *testing.T) {
	if !hasFFmpeg() || !hasLibx264() {
		t.Skip("没有带 libx264 的 ffmpeg")
	}
	e := newEnv(t)
	admin := e.setupAdmin()
	dir := filepath.Join(e.mediaDir, "tc")
	os.MkdirAll(dir, 0o755)
	src := filepath.Join(dir, "转码测试.mp4")
	cmd := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=640x360:rate=25:duration=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=30",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-c:a", "aac", "-movflags", "+faststart", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败: %v %s", err, out)
	}
	var lib struct {
		ID int64 `json:"id"`
	}
	admin.must("admin.libs.create", map[string]any{"name": "转码", "kind": "home", "scraper": "none", "paths": []string{dir}}, &lib)
	e.waitIdle()
	var l listResult
	admin.must("items.list", map[string]any{"libraryId": lib.ID, "limit": 10}, &l)
	item := findByTitle(t, l.Items, "转码测试")

	var open struct {
		Transcode *struct {
			Profiles []struct {
				ID string `json:"id"`
			} `json:"profiles"`
		} `json:"transcode"`
	}
	admin.must("play.open", map[string]any{"id": item.ID}, &open)
	if open.Transcode == nil || len(open.Transcode.Profiles) != 1 || open.Transcode.Profiles[0].ID != "360p" {
		t.Fatalf("640x360 的视频应该只能转 360P: %+v", open.Transcode)
	}
	admin.expectCode("play.transcode", map[string]any{"id": item.ID, "quality": "720p"}, "bad_request")

	var tc struct {
		Stream string `json:"stream"`
	}
	admin.must("play.transcode", map[string]any{"id": item.ID, "quality": "360p", "start": 10, "psid": "p1"}, &tc)
	if !strings.HasPrefix(tc.Stream, "/t/") || !strings.HasSuffix(tc.Stream, "/index.m3u8") {
		t.Fatalf("转码流地址不正确: %q", tc.Stream)
	}
	base := strings.TrimSuffix(tc.Stream, "index.m3u8")
	code, pl, _ := admin.get(tc.Stream, "")
	if code != 200 || !strings.HasPrefix(string(pl), "#EXTM3U") || strings.Count(string(pl), "#EXTINF") != 8 ||
		!strings.Contains(string(pl), "#EXT-X-ENDLIST") {
		t.Fatalf("播放列表不正确: %d\n%s", code, pl)
	}
	get := func(name string) []byte {
		t.Helper()
		code, b, _ := admin.get(base+name, "")
		if code != 200 {
			t.Fatalf("获取分片 %s 失败: HTTP %d %s", name, code, b)
		}
		return b
	}
	init := get("init")
	if !bytes.Contains(init[:64], []byte("ftyp")) {
		t.Fatal("初始化分片不是 MP4")
	}
	check := func(n int) []byte {
		t.Helper()
		seg := get(strconv.Itoa(n))
		pts := firstVideoPTS(t, append(append([]byte(nil), init...), seg...))
		if math.Abs(pts-float64(n)*4) > 0.05 {
			t.Fatalf("分片 %d 的起始时间是 %.3f，期望 %d", n, pts, n*4)
		}
		return seg
	}
	check(2) // 从 10 秒开始播放：第一个分片是 8～12 秒
	check(3)
	check(6) // 拖动到后面
	check(0) // 拖回开头：重新从 0 转码

	// 全部分片按顺序拼起来是一个完整、时间连续的视频
	all := append([]byte(nil), init...)
	for i := 0; i < 8; i++ {
		all = append(all, get(strconv.Itoa(i))...)
	}
	f := filepath.Join(t.TempDir(), "all.mp4")
	os.WriteFile(f, all, 0o644)
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,width,height:packet=stream_index,pts_time",
		"-of", "json", f).Output()
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Streams []struct {
			Codec  string `json:"codec_name"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"streams"`
		Packets []struct {
			Stream int    `json:"stream_index"`
			PTS    string `json:"pts_time"`
		} `json:"packets"`
	}
	json.Unmarshal(out, &probe)
	if len(probe.Streams) != 2 || probe.Streams[0].Codec != "h264" || probe.Streams[0].Height != 360 || probe.Streams[1].Codec != "aac" {
		t.Fatalf("转码结果的流不正确: %+v", probe.Streams)
	}
	frames, last := 0, -1.0
	for _, p := range probe.Packets {
		if p.Stream != 0 {
			continue
		}
		v, _ := strconv.ParseFloat(p.PTS, 64)
		if v <= last || v-last > 0.1 && last >= 0 {
			t.Fatalf("视频时间不连续: %.3f → %.3f", last, v)
		}
		last = v
		frames++
	}
	if frames < 740 || frames > 760 {
		t.Fatalf("视频帧数 %d，期望约 750", frames)
	}

	// 分片在 CDN 上只是密文
	if e.cdn.contains([]byte("#EXTM3U")) || e.cdn.contains([]byte("ftypiso")) {
		t.Fatal("CDN 看到了转码分片的明文")
	}

	// 其他浏览器（没有拿到这个会话）访问不到
	other := admin.sameClient()
	if code, _, _ := other.get(base+"0", ""); code != 401 && code != 404 {
		t.Fatalf("其他浏览器不应访问到转码分片: HTTP %d", code)
	}

	// 关闭会话后分片不能再获取
	sid := strings.Split(tc.Stream, "/")[2]
	admin.must("play.transcodeStop", map[string]any{"sid": sid}, nil)
	time.Sleep(100 * time.Millisecond)
	if code, _, _ := admin.get(base+"1", ""); code != 404 {
		t.Fatalf("关闭后仍能获取分片: HTTP %d", code)
	}

	// 同一播放页切换画质：旧会话自动关闭
	admin.must("play.transcode", map[string]any{"id": item.ID, "quality": "360p", "psid": "p2"}, &tc)
	first := tc.Stream
	admin.must("play.transcode", map[string]any{"id": item.ID, "quality": "360p", "psid": "p2"}, &tc)
	if code, _, _ := admin.get(strings.TrimSuffix(first, "index.m3u8")+"0", ""); code != 404 {
		t.Fatalf("同一播放页的旧会话没有关闭: HTTP %d", code)
	}
	if code, _, _ := admin.get(strings.TrimSuffix(tc.Stream, "index.m3u8")+"0", ""); code != 200 {
		t.Fatalf("新会话获取分片失败: HTTP %d", code)
	}

	// 按用户控制转码权限：关闭后不能转码且进行中的会话立即结束，开启后恢复
	var uRes struct {
		ID int64 `json:"id"`
	}
	admin.must("admin.users.create", map[string]any{
		"username": "no-tc", "password": "no-tc-password", "allowTranscode": false, "libraries": []int64{lib.ID},
	}, &uRes)
	bob := e.newClient()
	var me struct {
		AllowTranscode bool `json:"allowTranscode"`
	}
	bob.must("auth.login", map[string]any{"username": "no-tc", "password": "no-tc-password"}, &me)
	if me.AllowTranscode {
		t.Fatal("新建禁止转码的用户 allowTranscode 应为 false")
	}
	bob.must("play.open", map[string]any{"id": item.ID}, &open)
	if open.Transcode != nil {
		t.Fatal("未开启转码权限的用户不应看到画质选项")
	}
	bob.expectCode("play.transcode", map[string]any{"id": item.ID, "quality": "360p"}, "forbidden")
	// 给该用户开启转码权限：可以正常转码播放
	admin.must("admin.users.update", map[string]any{
		"id": uRes.ID, "allowTranscode": true, "libraries": []int64{lib.ID},
	}, nil)
	bob.must("play.open", map[string]any{"id": item.ID}, &open)
	if open.Transcode == nil || len(open.Transcode.Profiles) != 1 {
		t.Fatalf("开启转码权限后应返回画质选项: %+v", open.Transcode)
	}
	var bobTC struct {
		Stream string `json:"stream"`
	}
	bob.must("play.transcode", map[string]any{"id": item.ID, "quality": "360p", "psid": "bob1"}, &bobTC)
	bobBase := strings.TrimSuffix(bobTC.Stream, "index.m3u8")
	if code, _, _ := bob.get(bobBase+"0", ""); code != 200 {
		t.Fatalf("开启权限后获取转码分片失败: HTTP %d", code)
	}
	// 再次关闭该用户的转码权限：已创建的会话立即失效，后续请求被拒绝
	admin.must("admin.users.update", map[string]any{
		"id": uRes.ID, "allowTranscode": false, "libraries": []int64{lib.ID},
	}, nil)
	if code, _, _ := bob.get(bobBase+"1", ""); code != 403 && code != 404 {
		t.Fatalf("关闭转码权限后旧会话不应继续提供分片: HTTP %d", code)
	}
	bob.expectCode("play.transcode", map[string]any{"id": item.ID, "quality": "360p"}, "forbidden")

	// 关闭全局转码后不再提供画质选项
	var st map[string]any
	var got struct {
		Settings map[string]any `json:"settings"`
	}
	admin.must("admin.settings.get", nil, &got)
	st = got.Settings
	st["transcodeEnabled"] = false
	admin.must("admin.settings.set", st, nil)
	admin.must("play.open", map[string]any{"id": item.ID}, &open)
	if open.Transcode != nil {
		t.Fatal("关闭转码后仍然提供画质选项")
	}
	admin.expectCode("play.transcode", map[string]any{"id": item.ID, "quality": "360p"}, "bad_request")
}
