package transcode

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"veilfin/internal/media"
	"veilfin/internal/mediacrypt"
)

// 两个请求同时等待相距很远的分片（拖动后旧请求没有被取消）：只有最新的请求能重新开始转码，
// 旧请求直接放弃，不会两边轮流结束对方的 ffmpeg、无限重启。
func TestConcurrentSeekNoRestartLoop(t *testing.T) {
	tl, err := media.Find("")
	if err != nil {
		t.Skip("没有 ffmpeg")
	}
	encs := tl.H264Encoders(context.Background())
	if len(encs) == 0 {
		t.Skip("没有可用的 H.264 编码器")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=size=320x180:rate=25:duration=120",
		"-f", "lavfi", "-i", "sine=duration=120", "-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-c:a", "aac", src).CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败: %v %s", err, out)
	}
	keys, _ := mediacrypt.NewKeys(make([]byte, 32))
	m := NewManager(filepath.Join(dir, "tc"), tl, keys)
	defer m.Shutdown()
	p, _ := FindProfile("360p")
	s, err := m.Create(Options{UID: 1, Path: src, Duration: 120, Width: 320, Height: 180, Profile: p, Encoder: encs[0]}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	var errOld error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, errOld = s.Segment(ctx, "25") // 旧请求：一直没有被取消
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := s.Segment(ctx, "5"); err != nil { // 新请求（拖动到前面）
		t.Fatalf("新请求失败: %v", err)
	}
	wg.Wait()
	if errOld != nil && !errors.Is(errOld, ErrSuperseded) {
		t.Fatalf("旧请求应该拿到分片或被放弃: %v", errOld)
	}
	s.mu.Lock()
	runs := s.runs
	s.mu.Unlock()
	if runs > 4 {
		t.Fatalf("ffmpeg 启动了 %d 次，转码在反复重启", runs)
	}
}
