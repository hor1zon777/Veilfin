// Package transcode 实现播放时的实时转码。
//
// 每个播放会话对应一个 Session：ffmpeg 从指定位置开始把视频转成 H.264 + AAC 的分片 MP4，
// 程序按关键帧把输出切成固定时长（SegDur 秒）的 HLS 分片，改写时间戳为源视频中的绝对时间后
// 逐段加密写到临时目录。客户端按 HLS 播放列表依次请求分片：
//
//   - 请求的分片已经生成：直接返回；
//   - 正好是 ffmpeg 接下来要生成的：等它生成；
//   - 离得远（拖动进度条）：结束当前的 ffmpeg，从请求的分片重新开始转码。
//
// 生成的分片领先播放位置太多时暂停读取 ffmpeg 的输出，ffmpeg 写不进管道自然停下，不占 CPU；
// 播放位置追上来后继续。一段时间没有请求的会话先结束 ffmpeg，再久一些整个删掉。
package transcode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"veilfin/internal/media"
	"veilfin/internal/mediacrypt"
)

const (
	SegDur     = 4.0 // 分片时长（秒）
	maxAhead   = 30  // 最多领先最近请求的分片数（约 2 分钟），超过就暂停 ffmpeg
	keepBehind = 45  // 保留最近请求之前的分片数（约 3 分钟），方便回退
	keepAfter  = 60  // 之前的转码留下、在最近请求之后太远的分片也删掉
	nearGap    = 2   // 请求的分片在 ffmpeg 当前位置之后这么多段以内时等它，否则重新开始转码
	waitLimit  = 90 * time.Second
	stopIdle   = 60 * time.Second // 这么久没有请求，结束 ffmpeg（暂停播放时）
	dropIdle   = 20 * time.Minute // 这么久没有请求，删除整个会话
)

var (
	ErrNotFound = errors.New("转码会话不存在或已过期")
	ErrBusy     = errors.New("转码任务已满")
)

// Profile 是一档转码画质。
type Profile struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Short   int    `json:"height"`  // 输出视频短边
	Bitrate int64  `json:"bitrate"` // 视频码率（bps）
}

var Profiles = []Profile{
	{"1080p", "1080P", 1080, 6_000_000},
	{"720p", "720P", 720, 3_000_000},
	{"480p", "480P", 480, 1_200_000},
	{"360p", "360P", 360, 600_000},
}

// ProfilesFor 返回适合源视频的画质：短边比源视频小的各档，加上不超过源视频的最高一档
// （同分辨率转码，用于降低码率或浏览器不支持原始编码的视频，不会放大）。
func ProfilesFor(w, h int) []Profile {
	short := min(w, h)
	if short <= 0 {
		return nil
	}
	var out []Profile
	for i := len(Profiles) - 1; i >= 0; i-- { // Profiles 从高到低排列，倒着找第一档不低于源视频的
		if Profiles[i].Short >= short {
			out = append(out, Profiles[i])
			break
		}
	}
	for _, p := range Profiles {
		if p.Short < short {
			out = append(out, p)
		}
	}
	return out
}

// FindProfile 按 ID 查找画质。
func FindProfile(id string) (Profile, bool) {
	for _, p := range Profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// Options 是创建会话的参数。
type Options struct {
	UID       int64
	Owner     string // 播放页实例 ID：同一播放页再次创建会话（切换画质）时关闭旧的
	LibraryID int64
	Path      string
	Duration  float64
	Width     int
	Height    int
	Bitrate   int64 // 源视频总码率（bps），0 表示未知
	Profile   Profile
	Encoder   string
	Start     float64
}

// Manager 管理所有转码会话。
type Manager struct {
	dir  string
	ff   *media.Tools
	keys *mediacrypt.Keys

	mu       sync.Mutex
	sessions map[string]*Session
	now      func() time.Time
}

// NewManager 创建管理器。dir 是临时目录，启动时清空。
func NewManager(dir string, ff *media.Tools, keys *mediacrypt.Keys) *Manager {
	os.RemoveAll(dir)
	return &Manager{dir: dir, ff: ff, keys: keys, sessions: map[string]*Session{}, now: time.Now}
}

// Session 是一个转码会话。
type Session struct {
	ID        string
	UID       int64
	LibraryID int64
	m         *Manager
	o         Options
	nseg      int
	dir       string
	enc       []byte
	nonce     []byte
	bitrate   int64

	mu         sync.Mutex
	init       []byte // 加密后的初始化分片
	have       map[int]bool
	run        *run
	lastReq    int
	lastAccess time.Time
	changed    chan struct{} // 状态变化时关闭并换新，用来唤醒等待者
	closed     bool
}

type run struct {
	start  int
	next   int // 下一个要完成的分片
	cancel context.CancelFunc
	done   bool
	eof    bool // ffmpeg 正常结束（转到了视频结尾），而不是被中途结束
	err    error
}

// Active 返回最近仍在使用的会话数（正在转码或刚刚有请求）。
func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	now := m.now()
	for _, s := range m.sessions {
		if s.active(now) {
			n++
		}
	}
	return n
}

func (s *Session) active(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Sub(s.lastAccess) < stopIdle
}

// Create 创建会话并立即从 o.Start 所在的分片开始转码。limit 是同时转码的会话上限。
func (m *Manager) Create(o Options, limit int) (*Session, error) {
	if o.Duration <= 0 {
		return nil, errors.New("视频时长未知，无法转码")
	}
	// 同一播放页的旧会话（切换画质）先关掉，不占名额
	m.mu.Lock()
	var old []*Session
	for _, s := range m.sessions {
		if s.UID == o.UID && s.o.Owner == o.Owner && o.Owner != "" {
			old = append(old, s)
		}
	}
	m.mu.Unlock()
	for _, s := range old {
		m.remove(s)
	}
	if limit > 0 && m.Active() >= limit {
		return nil, ErrBusy
	}
	idb := make([]byte, 16)
	rand.Read(idb)
	id := base64.RawURLEncoding.EncodeToString(idb)
	enc, nonce := m.keys.BlobKeys("transcode/" + id)
	br := o.Profile.Bitrate
	if o.Bitrate > 0 {
		// 源视频码率本来就不高：不要转得比原来还大（留出音频的部分）
		br = min(br, max(o.Bitrate-128_000, 300_000))
	}
	s := &Session{
		ID: id, UID: o.UID, LibraryID: o.LibraryID, m: m, o: o,
		nseg: int(math.Ceil(o.Duration/SegDur - 1e-6)), dir: filepath.Join(m.dir, id),
		enc: enc, nonce: nonce, bitrate: br,
		have: map[int]bool{}, lastAccess: m.now(), changed: make(chan struct{}),
	}
	if s.nseg < 1 {
		s.nseg = 1
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	start := min(max(int(o.Start/SegDur), 0), s.nseg-1)
	s.mu.Lock()
	s.lastReq = start
	s.startRun(start)
	s.mu.Unlock()
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	return s, nil
}

// Get 返回会话。
func (m *Manager) Get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// Close 关闭某个用户的会话。
func (m *Manager) Close(id string, uid int64) {
	if s := m.Get(id); s != nil && s.UID == uid {
		m.remove(s)
	}
}

// CloseUser 关闭某个用户的全部会话（退出登录、被禁用、锁定受限内容时）。
func (m *Manager) CloseUser(uid int64) {
	m.mu.Lock()
	var list []*Session
	for _, s := range m.sessions {
		if s.UID == uid {
			list = append(list, s)
		}
	}
	m.mu.Unlock()
	for _, s := range list {
		m.remove(s)
	}
}

func (m *Manager) remove(s *Session) {
	m.mu.Lock()
	if m.sessions[s.ID] == s {
		delete(m.sessions, s.ID)
	}
	m.mu.Unlock()
	s.mu.Lock()
	s.closed = true
	if s.run != nil && !s.run.done {
		s.run.cancel()
	}
	s.notify()
	s.mu.Unlock()
	// ffmpeg 退出后才能删掉目录（Windows 上占用中的文件删不掉），稍后再删一次
	os.RemoveAll(s.dir)
	go func() {
		time.Sleep(3 * time.Second)
		os.RemoveAll(s.dir)
	}()
}

// GC 结束空闲会话的 ffmpeg、删除长期不用的会话。定期调用。
func (m *Manager) GC() {
	now := m.now()
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	for _, s := range list {
		s.mu.Lock()
		idle := now.Sub(s.lastAccess)
		if idle > stopIdle && s.run != nil && !s.run.done {
			s.run.cancel()
		}
		s.mu.Unlock()
		if idle > dropIdle {
			m.remove(s)
		}
	}
}

// Shutdown 结束所有会话。
func (m *Manager) Shutdown() {
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	for _, s := range list {
		m.remove(s)
	}
}

// Key 返回客户端解密分片用的密钥。
func (s *Session) Key() string { return base64.RawURLEncoding.EncodeToString(s.enc) }

// Playlist 返回 HLS 播放列表（VOD，列出全部分片，可以任意拖动）。
func (s *Session) Playlist() string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(SegDur)+1)
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-MAP:URI=\"init\"\n")
	for i := 0; i < s.nseg; i++ {
		d := SegDur
		if i == s.nseg-1 {
			d = s.o.Duration - float64(i)*SegDur
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%d\n", d, i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// ObjectID 是分片在加密时使用的对象 ID（写进 AAD，分片之间不能互相替换）。
func ObjectID(sid, name string) string { return sid + "/" + name }

func (s *Session) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Segment 返回加密后的分片（name 为 "init" 或分片序号），必要时等待转码完成。
func (s *Session) Segment(ctx context.Context, name string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, waitLimit)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAccess = s.m.now()
	if name == "init" {
		for s.init == nil {
			if s.closed {
				return nil, ErrNotFound
			}
			if r := s.run; r == nil || r.done {
				if r != nil && r.err != nil {
					return nil, r.err
				}
				if r != nil && r.eof {
					return nil, ErrNotFound
				}
				s.startRun(s.lastReq)
			}
			if err := s.wait(ctx); err != nil {
				return nil, err
			}
		}
		return s.init, nil
	}
	n, err := strconv.Atoi(name)
	if err != nil || n < 0 || n >= s.nseg || strconv.Itoa(n) != name {
		return nil, ErrNotFound
	}
	if n != s.lastReq {
		s.lastReq = n
		s.notify() // 唤醒因领先太多而暂停的读取
	}
	for {
		if s.closed {
			return nil, ErrNotFound
		}
		if s.have[n] {
			b, err := os.ReadFile(s.segPath(n))
			if err != nil {
				delete(s.have, n)
				continue
			}
			s.prune(n)
			s.prefetch(n)
			return b, nil
		}
		r := s.run
		switch {
		case r != nil && !r.done && r.next <= n && n <= r.next+nearGap:
			// ffmpeg 很快就会生成它
		case r != nil && r.done && r.err != nil && r.start == n:
			return nil, r.err // 就是从这里开始的转码失败了，不再反复重试
		case r != nil && r.done && r.eof && r.next <= n:
			return nil, ErrNotFound // 已经转到结尾，没有这个分片（视频实际比记录的时长短）
		default:
			s.startRun(n)
		}
		if err := s.wait(ctx); err != nil {
			return nil, err
		}
	}
}

func (s *Session) wait(ctx context.Context) error {
	ch := s.changed
	s.mu.Unlock()
	defer s.mu.Lock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return errors.New("等待转码超时")
	}
}

// prefetch 播放到已有分片时，如果后面不远处有缺口而 ffmpeg 不会很快补上，提前从缺口开始转码。
func (s *Session) prefetch(n int) {
	for m := n + 1; m <= n+6 && m < s.nseg; m++ {
		if s.have[m] {
			continue
		}
		r := s.run
		if r != nil && !r.done && r.next <= m && m <= r.next+nearGap {
			return
		}
		if r != nil && r.done && (r.err != nil && r.start == m || r.eof && r.next <= m) {
			return
		}
		s.startRun(m)
		return
	}
}

// prune 删除离最近请求太远的分片，限制占用的磁盘空间。
func (s *Session) prune(n int) {
	for i := range s.have {
		if i < n-keepBehind || i > n+keepAfter+maxAhead {
			os.Remove(s.segPath(i))
			delete(s.have, i)
		}
	}
}

func (s *Session) segPath(n int) string { return filepath.Join(s.dir, strconv.Itoa(n)+".seg") }

// startRun 结束当前的 ffmpeg，从第 n 个分片重新开始转码。调用方持有 s.mu。
func (s *Session) startRun(n int) {
	if s.run != nil && !s.run.done {
		s.run.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{start: n, next: n, cancel: cancel}
	s.run = r
	s.notify()
	go s.transcode(ctx, r)
}

// transcode 运行一次 ffmpeg，把输出切成分片。
func (s *Session) transcode(ctx context.Context, r *run) {
	start := float64(r.start) * SegDur
	cmd := s.m.ff.TranscodeCmd(ctx, media.TranscodeOptions{
		Input: s.o.Path, Start: start, Encoder: s.o.Encoder, Width: s.o.Width, Height: s.o.Height,
		Short: s.o.Profile.Short, Bitrate: s.bitrate, SegDur: SegDur,
	})
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		s.finishRun(r, fmt.Errorf("启动 ffmpeg 失败: %w", err), false)
		return
	}
	err = s.split(ctx, r, bufio.NewReaderSize(out, 256<<10), start)
	if err != nil {
		r.cancel() // 解析出错：结束 ffmpeg
	}
	werr := cmd.Wait()
	if ctx.Err() != nil {
		s.finishRun(r, nil, false) // 被主动结束（拖动、空闲、关闭）
		return
	}
	if err == nil && werr != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		err = fmt.Errorf("ffmpeg 转码失败: %v %s", werr, msg)
	}
	if err != nil {
		log.Printf("转码失败（%s，从 %.0f 秒开始）：%v", filepath.Base(s.o.Path), start, err)
	}
	s.finishRun(r, err, err == nil)
}

func (s *Session) finishRun(r *run, err error, eof bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.done = true
	r.err = err
	r.eof = eof
	s.notify()
}

// split 从 ffmpeg 的输出中读取盒子：ftyp + moov 是初始化分片，之后每个 moof + mdat 是一个片段。
// 片段按改写后的起始时间归入分片（正常情况下一个片段就是一个分片；编码器多插了关键帧时，
// 几个片段合成一个分片）。下一个分片的片段到来时，前一个分片就完整了。
func (s *Session) split(ctx context.Context, r *run, br *bufio.Reader, offset float64) error {
	var initBuf []byte
	var tracks map[uint32]trackInfo
	cur := -1
	var segBuf []byte
	for {
		b, err := readBox(br)
		if err == io.EOF {
			if cur >= 0 && ctx.Err() == nil {
				return s.finishSegment(ctx, r, cur, segBuf, true)
			}
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		switch b.typ {
		case "ftyp":
			initBuf = append(initBuf, b.data...)
		case "moov":
			initBuf = append(initBuf, b.data...)
			tracks = parseMoov(b.data)
			if err := s.setInit(initBuf); err != nil {
				return err
			}
		case "moof":
			if tracks == nil {
				return errors.New("ffmpeg 输出缺少 moov")
			}
			t, ok := patchMoof(b.data, tracks, offset)
			if !ok {
				return errors.New("ffmpeg 输出的片段缺少时间戳")
			}
			idx := int(math.Floor(t/SegDur + 0.05))
			idx = min(max(idx, r.start), s.nseg-1)
			if cur >= 0 && idx > cur {
				if err := s.finishSegment(ctx, r, cur, segBuf, false); err != nil {
					return err
				}
				segBuf = nil
			}
			if idx > cur {
				cur = idx
			}
			segBuf = append(segBuf, b.data...)
		case "mdat":
			if cur >= 0 {
				segBuf = append(segBuf, b.data...)
			}
		}
	}
}

func (s *Session) setInit(plain []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.init != nil {
		return nil // 每次重新转码生成的初始化分片都一样（编码参数相同），只用第一份
	}
	ct, err := s.seal("init", plain)
	if err != nil {
		return err
	}
	s.init = ct
	s.notify()
	return nil
}

func (s *Session) seal(name string, plain []byte) ([]byte, error) {
	c, err := mediacrypt.NewCipher(s.enc, s.nonce, ObjectID(s.ID, name), int64(len(plain)))
	if err != nil {
		return nil, err
	}
	return mediacrypt.SealAll(c, plain)
}

// finishSegment 加密并保存一个完整的分片，然后在领先太多时暂停（不再读取 ffmpeg 的输出）。
func (s *Session) finishSegment(ctx context.Context, r *run, n int, plain []byte, last bool) error {
	ct, err := s.seal(strconv.Itoa(n), plain)
	if err != nil {
		return err
	}
	tmp := s.segPath(n) + ".tmp"
	if err := os.WriteFile(tmp, ct, 0o600); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		os.Remove(tmp)
		return context.Canceled
	}
	os.Remove(s.segPath(n)) // Windows 上 Rename 不能覆盖正在被读取的文件
	if err := os.Rename(tmp, s.segPath(n)); err != nil {
		os.Remove(tmp)
		return err
	}
	s.have[n] = true
	if s.run == r {
		r.next = n + 1
	}
	s.notify()
	if last {
		return nil
	}
	for n+1 > s.lastReq+maxAhead && !s.closed && ctx.Err() == nil {
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		s.mu.Lock()
	}
	return nil
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
