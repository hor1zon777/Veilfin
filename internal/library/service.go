// Package library 负责媒体库扫描和元数据处理流水线：
//
//	扫描目录 → ffprobe → 本地元数据（NFO / JSON / 图片）→ 在线刮削 → 截图 → 字幕 → 进度条预览图
package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"veilfin/internal/db"
	"veilfin/internal/imaging"
	"veilfin/internal/media"
	"veilfin/internal/mediacrypt"
	"veilfin/internal/scraper"
	"veilfin/internal/tasks"
)

// VideoExts 是支持的视频格式（浏览器可以直接播放的容器）。
var VideoExts = map[string]string{
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".mov":  "video/mp4",
	".webm": "video/webm",
}

var subExts = map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".vtt": true}

// 扫描时跳过的目录
var skipDirs = map[string]bool{"@eaDir": true, "#recycle": true, "$RECYCLE.BIN": true, "System Volume Information": true, "lost+found": true}

// Settings 是可以在管理后台修改的系统设置。
type Settings struct {
	ServerName        string `json:"serverName"`
	TMDBKey           string `json:"tmdbKey"`
	TMDBLang          string `json:"tmdbLang"`
	MetaTubeURL       string `json:"metatubeUrl"`
	MetaTubeToken     string `json:"metatubeToken"`
	ScraperProxy      string `json:"scraperProxy"`
	ScanIntervalHours int    `json:"scanIntervalHours"`
	WatchEnabled      bool   `json:"watchEnabled"`
	// KeepPageTitle 为真时，播放视频不会把浏览器标签页标题改成视频名称
	KeepPageTitle bool `json:"keepPageTitle"`
	// AutoCheckUpdate 为真时服务端定时访问 GitHub 检查新版本（只检查，不自动安装）
	AutoCheckUpdate bool `json:"autoCheckUpdate"`
	// MetadataWorkers 是同时处理条目（刮削、下载封面、截图、字幕）的线程数
	MetadataWorkers int `json:"metadataWorkers"`
	// TrickplayWorkers 是生成进度条预览图时同时运行的 ffmpeg 数（一次处理一个视频，每个 ffmpeg 截取一个时间点）
	TrickplayWorkers int `json:"trickplayWorkers"`
	// TranscodeEnabled 为真时允许播放时选择较低画质（服务端用 ffmpeg 实时转码）
	TranscodeEnabled bool `json:"transcodeEnabled"`
	// TranscodeEncoder 是转码使用的 H.264 编码器，为空表示自动选择
	TranscodeEncoder string `json:"transcodeEncoder"`
	// TranscodeMax 是同时转码的播放数上限
	TranscodeMax int `json:"transcodeMax"`
}

// 元数据处理、预览图生成线程数的默认值和上限
const (
	DefaultMetadataWorkers  = 4
	MaxMetadataWorkers      = 32
	DefaultTrickplayWorkers = 4
	MaxTrickplayWorkers     = 16
	DefaultTranscodeMax     = 2
	MaxTranscodeMax         = 16
)

func DefaultSettings() Settings {
	return Settings{ServerName: "Veilfin", TMDBLang: "zh-CN", ScanIntervalHours: 6, WatchEnabled: true, AutoCheckUpdate: true, MetadataWorkers: DefaultMetadataWorkers, TrickplayWorkers: DefaultTrickplayWorkers,
		TranscodeEnabled: true, TranscodeMax: DefaultTranscodeMax}
}

type Service struct {
	DB       *db.DB
	FF       *media.Tools // 可能为 nil：没有 ffmpeg 时只能播放，不做截图和字幕
	AssetDir string
	Main     *tasks.Queue
	Heavy    *tasks.Queue // 预览图等耗时任务单独排队，不阻塞元数据处理；一次只处理一个视频，视频内部并行截图

	// NewProvider 允许测试替换刮削数据源。
	NewProvider func(kind string, s Settings) (scraper.Provider, error)

	baseCtx  context.Context // 服务的生命周期（目录监听等长期任务挂在它上面，而不是某个请求上）
	mu       sync.Mutex
	settings *Settings
	watcher  *watcher
	progress progressSet

	scanMu   sync.Mutex
	scanning map[int64]*rescan // 正在扫描的媒体库；同一个库同时只跑一次扫描

	settleMu     sync.Mutex
	settleTimers map[int64]*time.Timer // 有文件正在写入的媒体库：等文件稳定后补扫
}

// FileSettleTime：修改时间在这段时间以内的视频视为可能正在复制 / 下载，扫描时先跳过，稳定后再处理。
// 很多工具把 MP4 的索引（moov）写在文件末尾，没写完就探测会报“moov atom not found”。
// 测试可以调小（为 0 时不等待）。
var FileSettleTime = 2 * time.Minute

// writing 判断文件是否可能还在写入：修改时间在 FileSettleTime 以内。
// 修改时间明显在未来的（NAS 与服务器时钟不一致）不算，否则这些文件会一直不被处理。
func writing(mtime, now time.Time) bool {
	age := now.Sub(mtime)
	return FileSettleTime > 0 && age < FileSettleTime && age > -FileSettleTime
}

// scheduleSettleScan 在 at 时刻（文件稳定后）补扫一次媒体库；已经安排过的取较晚的时间。
func (s *Service) scheduleSettleScan(libID int64, at time.Time) {
	s.settleMu.Lock()
	defer s.settleMu.Unlock()
	if s.settleTimers == nil {
		s.settleTimers = map[int64]*time.Timer{}
	}
	if t := s.settleTimers[libID]; t != nil {
		t.Stop()
	}
	s.settleTimers[libID] = time.AfterFunc(time.Until(at), func() {
		s.settleMu.Lock()
		delete(s.settleTimers, libID)
		s.settleMu.Unlock()
		if s.baseCtx.Err() == nil {
			s.EnqueueScan(libID, false)
		}
	})
}

// rescan 记录扫描进行期间又有人要求扫描：当前这次结束后再补扫一次。
type rescan struct {
	again   bool
	refresh bool
}

func New(ctx context.Context, d *db.DB, ff *media.Tools, assetDir string) *Service {
	s := &Service{
		DB:       d,
		FF:       ff,
		AssetDir: assetDir,
		Main:     tasks.New(ctx, "元数据", DefaultMetadataWorkers),
		Heavy:    tasks.New(ctx, "预览图", 1),
		baseCtx:  ctx,
		scanning: map[int64]*rescan{},
	}
	s.NewProvider = s.defaultProvider
	st := s.Settings(ctx)
	s.Main.SetWorkers(st.MetadataWorkers)
	return s
}

// Stopped 等待后台任务全部退出（需先结束创建服务时传入的 ctx），之后才能安全地关闭数据库。
func (s *Service) Stopped() {
	s.Main.Stopped()
	s.Heavy.Stopped()
}

// ---- 设置 ----

func (s *Service) Settings(ctx context.Context) Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings != nil {
		return *s.settings
	}
	st := DefaultSettings()
	if v, err := s.DB.GetSetting(ctx, "config"); err == nil && v != "" {
		json.Unmarshal([]byte(v), &st)
	}
	st.normalizeWorkers()
	s.settings = &st
	return st
}

func (s *Service) SaveSettings(ctx context.Context, st Settings) error {
	if st.ScanIntervalHours < 0 {
		st.ScanIntervalHours = 0
	}
	st.normalizeWorkers()
	b, _ := json.Marshal(st)
	if err := s.DB.SetSetting(ctx, "config", string(b)); err != nil {
		return err
	}
	s.mu.Lock()
	s.settings = &st
	s.mu.Unlock()
	s.Main.SetWorkers(st.MetadataWorkers)
	return nil
}

// normalizeWorkers 把线程数限制在合法范围内，0（未设置）或负数取默认值。
func (st *Settings) normalizeWorkers() {
	st.MetadataWorkers = clampWorkers(st.MetadataWorkers, DefaultMetadataWorkers, MaxMetadataWorkers)
	st.TrickplayWorkers = clampWorkers(st.TrickplayWorkers, DefaultTrickplayWorkers, MaxTrickplayWorkers)
	st.TranscodeMax = clampWorkers(st.TranscodeMax, DefaultTranscodeMax, MaxTranscodeMax)
}

func clampWorkers(n, def, max int) int {
	switch {
	case n <= 0:
		return def
	case n > max:
		return max
	}
	return n
}

func (s *Service) defaultProvider(kind string, st Settings) (scraper.Provider, error) {
	h, err := scraper.NewHTTP(st.ScraperProxy)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "metatube":
		return &scraper.MetaTube{Base: st.MetaTubeURL, Token: st.MetaTubeToken, H: h}, nil
	case "tmdb":
		return &scraper.TMDB{Key: st.TMDBKey, Language: st.TMDBLang, H: h}, nil
	}
	return nil, fmt.Errorf("未知的刮削源: %s", kind)
}

// ---- 扫描 ----

// EnqueueScan 把媒体库扫描加入队列。refresh 为 true 时会重新读取所有条目的元数据（不覆盖手动锁定的字段）。
func (s *Service) EnqueueScan(libID int64, refresh bool) {
	if s.deferScan(libID, refresh) {
		return
	}
	key := fmt.Sprintf("scan:%d", libID)
	label := fmt.Sprintf("扫描媒体库 #%d", libID)
	if refresh {
		key += ":refresh"
		label += "（刷新元数据）"
	}
	s.progress.queued(libID, refresh)
	s.Main.Add(key, label, func(ctx context.Context) error { return s.ScanLibrary(ctx, libID, refresh) })
}

// ScanLibrary 扫描媒体库目录，同步新增 / 变化 / 删除的文件，并把需要处理的条目加入队列。
func (s *Service) ScanLibrary(ctx context.Context, libID int64, refresh bool) error {
	// 同一个库的扫描不能并行（任务队列有多个工作协程，“扫描”和“扫描并刷新”的 key 也不同）：
	// 两次扫描各拿一份旧的条目快照，会重复删除、重复插入
	s.scanMu.Lock()
	if r := s.scanning[libID]; r != nil {
		r.again, r.refresh = true, r.refresh || refresh
		s.scanMu.Unlock()
		return nil
	}
	s.scanning[libID] = &rescan{}
	s.scanMu.Unlock()
	defer func() {
		s.scanMu.Lock()
		r := s.scanning[libID]
		delete(s.scanning, libID)
		s.scanMu.Unlock()
		if r.again && ctx.Err() == nil {
			s.EnqueueScan(libID, r.refresh)
		}
	}()
	s.progress.startScan(libID, refresh)
	if err := s.scanLibrary(ctx, libID, refresh); err != nil {
		s.progress.failScan(libID, err)
		return err
	}
	return nil
}

// deferScan 在这个库正在扫描时记下“结束后再扫一次”并返回 true。
func (s *Service) deferScan(libID int64, refresh bool) bool {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	r := s.scanning[libID]
	if r == nil {
		return false
	}
	r.again, r.refresh = true, r.refresh || refresh
	return true
}

func (s *Service) scanLibrary(ctx context.Context, libID int64, refresh bool) error {
	lib, err := s.DB.LibraryByID(ctx, libID)
	if err != nil {
		return err
	}
	existing, err := s.DB.LibraryFiles(ctx, lib.ID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var unavailable []string // 不可访问的根目录：其下的条目不删除（防止硬盘未挂载时清空媒体库）
	multi := len(lib.Paths) > 1
	now := time.Now()
	var settling []string // 正在写入、本次跳过的文件
	var newestWrite time.Time

	for _, root := range lib.Paths {
		root = filepath.Clean(root)
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			log.Printf("媒体库「%s」的目录不可访问，跳过: %s", lib.Name, root)
			unavailable = append(unavailable, root)
			continue
		}
		found := 0
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// 读不了的子目录（网络盘掉线、权限变化）：其下的条目一律保留，不当作已删除
				log.Printf("扫描时无法读取 %s: %v（其下条目保留）", path, err)
				unavailable = append(unavailable, path)
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if path != root && (strings.HasPrefix(d.Name(), ".") || skipDirs[d.Name()]) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") {
				return nil
			}
			mime, ok := VideoExts[strings.ToLower(filepath.Ext(d.Name()))]
			if !ok {
				return nil
			}
			info, err := d.Info()
			if err == nil && d.Type()&fs.ModeSymlink != 0 {
				info, err = os.Stat(path) // 符号链接取目标文件的大小和时间
			}
			if err != nil || info.Size() == 0 || !info.Mode().IsRegular() {
				// 暂时读不到或正在复制（大小为 0）的文件：已有条目保留，新文件等下次扫描
				if _, ok := existing[path]; ok {
					seen[path] = true
				}
				return nil
			}
			mtime := info.ModTime().Unix()
			ex, known := existing[path]
			changed := !known || ex.Size != info.Size() || ex.Mtime != mtime
			if changed && writing(info.ModTime(), now) {
				// 可能还在复制 / 下载：新文件先不入库，已有条目先不重新探测，等稳定后补扫
				if known {
					seen[path] = true
				}
				settling = append(settling, filepath.Base(path))
				if info.ModTime().After(newestWrite) {
					newestWrite = info.ModTime()
				}
				return nil
			}
			found++
			seen[path] = true
			s.progress.update(lib.ID, func(t *scanTrack) { t.p.Found++ })
			if known {
				if changed {
					// 内容变了：换新的对象 ID，让 CDN 上的旧缓存自然失效
					if err := s.DB.ReplaceVideoFile(ctx, ex.ID, info.Size(), mtime, mediacrypt.NewID(), mime); err != nil {
						log.Printf("更新 %s 失败: %v", path, err)
					}
				}
				return nil
			}
			it := &db.Item{
				LibraryID: lib.ID, Path: path, Folder: folderOf(root, path, multi), Size: info.Size(), Mtime: mtime,
				VideoBlob: mediacrypt.NewID(), Mime: mime, Title: initialTitle(lib.Kind, path),
			}
			it.SortTitle = db.SortKey(it.Title)
			if _, err := s.DB.InsertItem(ctx, it); err != nil {
				log.Printf("添加 %s 失败: %v", path, err)
			} else {
				s.progress.update(lib.ID, func(t *scanTrack) { t.p.Added++ })
			}
			return nil
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if found == 0 && hasUnder(existing, root) {
			log.Printf("媒体库「%s」的目录 %s 为空但之前有视频，可能未挂载，本次不删除条目", lib.Name, root)
			unavailable = append(unavailable, root)
		}
	}

	if len(settling) > 0 {
		names := strings.Join(settling[:min(len(settling), 3)], "、")
		if len(settling) > 3 {
			names += fmt.Sprintf(" 等 %d 个", len(settling))
		}
		log.Printf("媒体库「%s」中 %s 刚修改过，可能正在复制，稳定后再处理", lib.Name, names)
		s.scheduleSettleScan(lib.ID, newestWrite.Add(FileSettleTime+5*time.Second))
	}

	var roots []string
	for _, r := range lib.Paths {
		roots = append(roots, filepath.Clean(r))
	}
	emptyDirs := map[string]bool{}
	for path, ex := range existing {
		if seen[path] || underAny(path, unavailable) {
			continue
		}
		if dir, ok := mountGone(path, roots, emptyDirs); ok {
			// 子目录是空的：多半是 NAS 子目录掉线 / 没挂载上，不能当成文件被删了（删条目会连带清掉观看进度）
			log.Printf("媒体库「%s」的目录 %s 是空的，可能未挂载，保留其下的条目 %s", lib.Name, dir, path)
			continue
		}
		files, err := s.DB.DeleteItem(ctx, ex.ID)
		if err != nil {
			log.Printf("删除条目失败: %v", err)
			continue
		}
		s.RemoveAssets(files)
		s.progress.update(lib.ID, func(t *scanTrack) { t.p.Removed++ })
	}
	s.DB.TouchLibraryScan(ctx, lib.ID)

	need := db.StateProbed | db.StateLocalMeta | db.StateThumb | db.StateSubs
	if lib.Scraper != "" && lib.Scraper != "none" {
		need |= db.StateScraped
	}
	ids, err := s.DB.ItemsNeedingWork(ctx, lib.ID, need)
	if err != nil {
		return err
	}
	if refresh {
		all, _ := s.DB.ItemsNeedingWork(ctx, lib.ID, 1<<30) // 1<<30 这一位从不设置，等于取全部
		ids = all
	}
	pending := map[int64]bool{}
	for _, id := range ids {
		pending[id] = true
	}
	// 需要处理的条目会在处理完（拿到时长）后自己排队生成预览图，这里只补上其余缺预览图的
	var tricks []int64
	if lib.Trickplay && s.FF != nil {
		tids, _ := s.DB.ItemsNeedingWork(ctx, lib.ID, db.StateTrickplay)
		for _, id := range tids {
			if !pending[id] {
				tricks = append(tricks, id)
			}
		}
	}
	s.progress.trackItems(lib.ID, ids, tricks)
	for _, id := range ids {
		s.EnqueueItem(id, refresh)
	}
	for _, id := range tricks {
		s.EnqueueTrickplay(id)
	}
	return nil
}

func folderOf(root, path string, multi bool) string {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || rel == "." {
		rel = ""
	}
	rel = filepath.ToSlash(rel)
	if multi {
		if rel == "" {
			return filepath.Base(root)
		}
		return filepath.Base(root) + "/" + rel
	}
	return rel
}

func initialTitle(kind, path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	switch kind {
	case "movie":
		t, _ := scraper.ParseTitle(path)
		return t
	case "jav":
		if c := scraper.ParseCode(path); c.Code != "" {
			return c.Code
		}
	}
	return base
}

func underAny(path string, roots []string) bool {
	for _, r := range roots {
		if path == r || strings.HasPrefix(path, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// mountGone 判断找不到的文件是不是因为目录没挂载上：从文件所在目录往上找到第一个还存在的目录
// （不超过媒体库根目录），这个目录完全是空的（连隐藏文件都没有）就认为是没挂载上的挂载点。
// 读不了的目录也按没挂载处理。确实删掉了某个文件夹里唯一的视频时，把空文件夹一起删掉条目才会移除。
func mountGone(path string, roots []string, cache map[string]bool) (string, bool) {
	root := ""
	for _, r := range roots {
		if underAny(path, []string{r}) && len(r) > len(root) {
			root = r
		}
	}
	if root == "" {
		return "", false
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if gone, ok := cache[dir]; ok {
			return dir, gone
		}
		entries, err := os.ReadDir(dir)
		switch {
		case err == nil:
			cache[dir] = len(entries) == 0
			return dir, cache[dir]
		case !errors.Is(err, fs.ErrNotExist):
			cache[dir] = true
			return dir, true
		}
		// 目录不存在：往上一级找；到了根目录（根目录不存在的情况前面已经处理过）就不再往上
		if dir == root || len(dir) <= len(root) {
			return dir, false
		}
	}
}

func hasUnder(existing map[string]db.FileState, root string) bool {
	for p := range existing {
		if strings.HasPrefix(p, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// ---- 单个条目的处理流水线 ----

func (s *Service) EnqueueItem(id int64, refresh bool) {
	s.Main.Add(fmt.Sprintf("item:%d", id), fmt.Sprintf("处理条目 #%d", id), func(ctx context.Context) error {
		err := s.ProcessItem(ctx, id, refresh)
		s.progress.itemDone(id, err)
		return err
	})
}

func (s *Service) EnqueueTrickplay(id int64) {
	s.progress.trickQueued(id)
	s.Heavy.Add(trickKey(id), fmt.Sprintf("生成预览图 #%d", id), s.trickTask(id))
}

func trickKey(id int64) string { return fmt.Sprintf("trick:%d", id) }

// PrioritizeTrickplay 有人打开播放页时调用：条目还没有预览图的话，把它的任务插到队首。
func (s *Service) PrioritizeTrickplay(ctx context.Context, it *db.Item) {
	if s.FF == nil || it.Trickplay != "" || it.Duration <= 0 || !s.trickplayOn(ctx, it.LibraryID) {
		return
	}
	s.progress.trickQueued(it.ID)
	s.Heavy.AddFront(trickKey(it.ID), fmt.Sprintf("生成预览图 #%d", it.ID), s.trickTask(it.ID))
}

// trickplayOn 报告媒体库是否开启了进度条预览图。
func (s *Service) trickplayOn(ctx context.Context, libID int64) bool {
	lib, err := s.DB.LibraryByID(ctx, libID)
	return err == nil && lib.Trickplay
}

func (s *Service) trickTask(id int64) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		defer s.progress.trickDone(id)
		return s.GenerateTrickplay(ctx, id)
	}
}

// ProcessItem 依次执行探测、本地元数据、刮削、截图、字幕。refresh 时重新执行所有步骤。
func (s *Service) ProcessItem(ctx context.Context, id int64, refresh bool) error {
	it, err := s.DB.ItemByID(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	lib, err := s.DB.LibraryByID(ctx, it.LibraryID)
	if err != nil {
		return err
	}
	state := it.ScanState
	need := func(bit int) bool { return refresh || state&bit == 0 }

	var probe *media.ProbeResult
	if s.FF != nil && (need(db.StateProbed) || need(db.StateSubs)) {
		probe, err = s.FF.Probe(ctx, it.Path)
		if err != nil {
			return fmt.Errorf("探测 %s: %w", filepath.Base(it.Path), err)
		}
		if err := s.DB.SetMediaInfo(ctx, id, db.MediaInfo{Duration: probe.Duration, Width: probe.Width, Height: probe.Height,
			VCodec: probe.VCodec, ACodec: probe.ACodec, Container: probe.Container, Bitrate: probe.Bitrate}); err != nil {
			return err
		}
		it.Duration, it.Width, it.Height = probe.Duration, probe.Width, probe.Height
	}

	hasLocal := false
	if need(db.StateLocalMeta) {
		lm := readLocalMeta(it.Path, countVideosInDir(filepath.Dir(it.Path)) == 1)
		if lm.Source != "" {
			hasLocal = containsStr(lm.Fields, "title")
			if err := s.DB.ApplyMeta(ctx, id, &lm.Meta, lm.Fields, lm.Source, false, false); err != nil {
				return err
			}
		}
		if lib.Kind == "jav" && !containsStr(lm.Fields, "code") {
			if c := scraper.ParseCode(it.Path); c.Code != "" {
				s.DB.ApplyMeta(ctx, id, &db.Meta{Code: c.Code}, []string{"code"}, "", false, false)
			}
		}
		for kind, p := range map[string]string{"poster": lm.Poster, "backdrop": lm.Back, "thumb": lm.Thumb} {
			if p == "" {
				continue
			}
			if data, err := os.ReadFile(p); err == nil {
				if err := s.SetImageData(ctx, id, kind, data, false); err != nil {
					log.Printf("读取本地图片 %s 失败: %v", p, err)
				}
			}
		}
		s.DB.AddScanState(ctx, id, db.StateLocalMeta)
	} else {
		hasLocal = it.MetaSource == "nfo" || it.MetaSource == "json"
	}

	if lib.Scraper != "" && lib.Scraper != "none" && need(db.StateScraped) && !hasLocal {
		if err := s.Scrape(ctx, id, "", false); err != nil && !errors.Is(err, scraper.ErrNoMatch) {
			return fmt.Errorf("刮削 %s: %w", filepath.Base(it.Path), err)
		}
	}

	if s.FF != nil && need(db.StateThumb) {
		cur, _ := s.DB.ItemByID(ctx, id)
		if cur != nil && cur.Thumb == "" {
			at := 5.0
			if it.Duration > 0 {
				at = it.Duration * 0.1
			}
			// 只取最近的关键帧：读取和解码的都只有这一帧
			if data, err := s.FF.KeyFrame(ctx, it.Path, at, imaging.Limits["thumb"][0]); err == nil {
				s.SetImageData(ctx, id, "thumb", data, false)
			} else {
				log.Printf("截图 %s 失败: %v", filepath.Base(it.Path), err)
			}
		}
		s.DB.AddScanState(ctx, id, db.StateThumb)
	}

	if s.FF != nil && need(db.StateSubs) {
		if err := s.processSubtitles(ctx, it, probe); err != nil {
			log.Printf("处理字幕 %s 失败: %v", filepath.Base(it.Path), err)
		}
	}
	if s.FF != nil && (refresh || state&db.StateTrickplay == 0) && lib.Trickplay {
		s.EnqueueTrickplay(id)
	}
	return nil
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func countVideosInDir(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			if _, ok := VideoExts[strings.ToLower(filepath.Ext(e.Name()))]; ok {
				n++
			}
		}
	}
	return n
}

// Scrape 用媒体库配置的数据源刮削条目。code 不为空时用它代替自动识别的番号 / 标题。
// replaceImages 为 true 时覆盖已有图片。
func (s *Service) Scrape(ctx context.Context, id int64, override string, replaceImages bool) error {
	it, err := s.DB.ItemByID(ctx, id)
	if err != nil {
		return err
	}
	lib, err := s.DB.LibraryByID(ctx, it.LibraryID)
	if err != nil {
		return err
	}
	if lib.Scraper == "" || lib.Scraper == "none" {
		return errors.New("该媒体库没有启用刮削")
	}
	p, err := s.NewProvider(lib.Scraper, s.Settings(ctx))
	if err != nil {
		return err
	}
	var q scraper.Query
	switch lib.Scraper {
	case "metatube":
		q.Code = override
		if q.Code == "" {
			q.Code = it.Code
		}
		if q.Code == "" {
			q.Code = scraper.ParseCode(it.Path).Code
		}
	default:
		q.Title, q.Year = scraper.ParseTitle(it.Path)
		if override != "" {
			q.Title, q.Year = override, 0
		}
	}
	res, err := p.Lookup(ctx, q)
	if errors.Is(err, scraper.ErrNoMatch) {
		s.DB.AddScanState(ctx, id, db.StateScraped)
		return err
	}
	if err != nil {
		return err
	}
	m := &db.Meta{Title: res.Title, OriginalTitle: res.OriginalTitle, Overview: res.Overview, Year: res.Year,
		ReleaseDate: res.ReleaseDate, Code: res.Code, Studio: res.Studio, Director: res.Director, Series: res.Series,
		Rating: res.Rating, Genres: res.Genres, Actors: res.Actors}
	var fields []string
	add := func(f string, ok bool) {
		if ok {
			fields = append(fields, f)
		}
	}
	add("title", m.Title != "")
	add("originalTitle", m.OriginalTitle != "")
	add("overview", m.Overview != "")
	add("year", m.Year > 0)
	add("releaseDate", m.ReleaseDate != "")
	add("code", m.Code != "")
	add("studio", m.Studio != "")
	add("director", m.Director != "")
	add("series", m.Series != "")
	add("rating", m.Rating > 0)
	add("genres", len(m.Genres) > 0)
	add("actors", len(m.Actors) > 0)
	if err := s.DB.ApplyMeta(ctx, id, m, fields, res.Source, false, false); err != nil {
		return err
	}
	for kind, u := range map[string]string{"poster": res.PosterURL, "backdrop": res.BackdropURL} {
		if u == "" {
			continue
		}
		data, err := p.Fetch(ctx, u)
		if err != nil {
			log.Printf("下载图片失败 %s: %v", u, err)
			continue
		}
		if kind == "poster" && res.CropPoster {
			if data, err = imaging.CropPoster(data); err != nil {
				continue
			}
		}
		if err := s.SetImageData(ctx, id, kind, data, replaceImages); err != nil {
			log.Printf("保存图片失败: %v", err)
		}
	}
	return s.DB.AddScanState(ctx, id, db.StateScraped)
}

// ---- 图片和资源文件 ----

// SetImageData 规范化图片并设为条目的 poster / backdrop / thumb。replace 为 false 时已有图片不覆盖。
func (s *Service) SetImageData(ctx context.Context, id int64, kind string, data []byte, replace bool) error {
	lim, ok := imaging.Limits[kind]
	if !ok {
		return errors.New("图片类型无效")
	}
	if !replace {
		it, err := s.DB.ItemByID(ctx, id)
		if err != nil {
			return err
		}
		cur := map[string]string{"poster": it.Poster, "backdrop": it.Backdrop, "thumb": it.Thumb}[kind]
		if cur != "" {
			return nil
		}
	}
	img, err := imaging.Normalize(data, lim[0], lim[1])
	if err != nil {
		return fmt.Errorf("图片无法识别: %w", err)
	}
	b, err := s.saveAsset(img, "image/jpeg", ".jpg")
	if err != nil {
		return err
	}
	old, err := s.DB.SetImage(ctx, id, kind, b)
	if err != nil {
		os.Remove(b.Path)
		return err
	}
	s.RemoveAssets([]string{old})
	return nil
}

// SetImageFromFrame 用视频某一时刻的画面作为图片。poster 会从画面中间裁出竖版。
func (s *Service) SetImageFromFrame(ctx context.Context, id int64, kind string, at float64) error {
	if s.FF == nil {
		return errors.New("没有 ffmpeg，无法截图")
	}
	it, err := s.DB.ItemByID(ctx, id)
	if err != nil {
		return err
	}
	data, err := s.FF.Frame(ctx, it.Path, at, 0)
	if err != nil {
		return err
	}
	if kind == "poster" {
		if data, err = imaging.CropCenter(data, 2.0/3.0); err != nil {
			return err
		}
	}
	return s.SetImageData(ctx, id, kind, data, true)
}

func (s *Service) saveAsset(data []byte, mime, ext string) (*db.Blob, error) {
	id := mediacrypt.NewID()
	dir := filepath.Join(s.AssetDir, id[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, id+ext)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return nil, err
	}
	return &db.Blob{ID: id, Path: p, Size: int64(len(data)), Mime: mime}, nil
}

// RemoveAssets 删除缓存文件。只删除资源目录下的文件，绝不会碰原始视频。
func (s *Service) RemoveAssets(paths []string) {
	root, err := filepath.Abs(s.AssetDir)
	if err != nil {
		return
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil || !strings.HasPrefix(abs, root+string(filepath.Separator)) {
			continue
		}
		os.Remove(abs)
	}
}

// ---- 字幕 ----

func (s *Service) processSubtitles(ctx context.Context, it *db.Item, probe *media.ProbeResult) error {
	dir := filepath.Dir(it.Path)
	base := strings.TrimSuffix(filepath.Base(it.Path), filepath.Ext(it.Path))
	var subs []*db.Subtitle
	var blobs []*db.Blob

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if e.IsDir() || !subExts[ext] || !strings.HasPrefix(name, base) {
			continue
		}
		mid := strings.Trim(strings.TrimSuffix(strings.TrimPrefix(name, base), filepath.Ext(name)), ".-_ ")
		if mid != "" && !strings.HasPrefix(strings.TrimPrefix(name, base), ".") {
			continue // 文件名只是前缀相同（如 abc.mp4 与 abcd.srt），不是这个视频的字幕
		}
		vtt, err := s.FF.ConvertSubtitle(ctx, filepath.Join(dir, name))
		if err != nil || len(vtt) < 10 {
			log.Printf("转换字幕 %s 失败: %v", name, err)
			continue
		}
		b, err := s.saveAsset(vtt, "text/vtt; charset=utf-8", ".vtt")
		if err != nil {
			return err
		}
		lang, label := langLabel(mid)
		subs = append(subs, &db.Subtitle{Lang: lang, Label: label + "（外挂）", Source: "external"})
		blobs = append(blobs, b)
	}
	if probe != nil {
		// 所有内嵌文本字幕一次提取：整个文件只读一遍
		var idx []int
		for _, st := range probe.Subs {
			if st.Text {
				idx = append(idx, st.Index)
			}
		}
		vtts, err := s.FF.ExtractSubtitles(ctx, it.Path, idx)
		if err != nil {
			log.Printf("提取内嵌字幕 %s 失败: %v", filepath.Base(it.Path), err)
		}
		for _, st := range probe.Subs {
			vtt := vtts[st.Index]
			if !st.Text || len(vtt) < 10 {
				continue
			}
			b, err := s.saveAsset(vtt, "text/vtt; charset=utf-8", ".vtt")
			if err != nil {
				return err
			}
			lang, label := langLabel(st.Lang)
			if st.Title != "" {
				label = st.Title
			}
			subs = append(subs, &db.Subtitle{Lang: lang, Label: label + "（内嵌）", Source: "embedded"})
			blobs = append(blobs, b)
		}
	}
	old, err := s.DB.ReplaceSubtitles(ctx, it.ID, subs, blobs)
	if err != nil {
		for _, b := range blobs {
			os.Remove(b.Path)
		}
		return err
	}
	s.RemoveAssets(old)
	return nil
}

var langNames = map[string][2]string{
	"zh": {"zh", "中文"}, "chi": {"zh", "中文"}, "zho": {"zh", "中文"}, "chs": {"zh", "简体中文"}, "sc": {"zh", "简体中文"},
	"zh-cn": {"zh", "简体中文"}, "zh-hans": {"zh", "简体中文"}, "cht": {"zh", "繁体中文"}, "tc": {"zh", "繁体中文"},
	"zh-tw": {"zh", "繁体中文"}, "zh-hant": {"zh", "繁体中文"}, "en": {"en", "英语"}, "eng": {"en", "英语"},
	"ja": {"ja", "日语"}, "jpn": {"ja", "日语"}, "jp": {"ja", "日语"}, "ko": {"ko", "韩语"}, "kor": {"ko", "韩语"},
}

func langLabel(code string) (string, string) {
	c := strings.ToLower(strings.TrimSpace(code))
	if v, ok := langNames[c]; ok {
		return v[0], v[1]
	}
	if c == "" || c == "und" {
		return "", "字幕"
	}
	return c, code
}

// ---- 进度条预览图 ----

// TrickplayData 是保存在条目上的预览图信息。
type TrickplayData struct {
	media.TrickplayInfo
	Sprites []string `json:"sprites"` // 对象 ID
}

func (s *Service) GenerateTrickplay(ctx context.Context, id int64) error {
	if s.FF == nil {
		return nil
	}
	it, err := s.DB.ItemByID(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if it.Duration <= 0 {
		return nil // 还没探测完，下次扫描再生成
	}
	if !s.trickplayOn(ctx, it.LibraryID) {
		return nil // 排队之后媒体库关闭了预览图，或手动刷新了未开启预览图的媒体库中的条目
	}
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	// 雪碧图在内存中拼好后直接加密保存，不经过临时文件
	var blobs []*db.Blob
	cleanup := func() {
		for _, b := range blobs {
			os.Remove(b.Path)
		}
	}
	workers := s.Settings(ctx).TrickplayWorkers
	key := trickKey(id)
	s.Heavy.SetLabel(key, fmt.Sprintf("生成预览图 #%d · %d 线程", id, workers))
	info, err := s.FF.Trickplay(ctx, it.Path, it.Duration, it.Width, it.Height, workers, func(sprite []byte) error {
		b, err := s.saveAsset(sprite, "image/jpeg", ".jpg")
		if err != nil {
			return err
		}
		blobs = append(blobs, b)
		return nil
	}, func(done, total int) {
		// 后台概览页显示线程数和进度：一个视频内部由多个 ffmpeg 并行截图
		s.Heavy.SetLabel(key, fmt.Sprintf("生成预览图 #%d · %d 线程 · %d/%d", id, workers, done, total))
	})
	if err != nil {
		cleanup()
		return err
	}
	td := TrickplayData{TrickplayInfo: *info}
	for _, b := range blobs {
		td.Sprites = append(td.Sprites, b.ID)
	}
	j, _ := json.Marshal(td)
	old, err := s.DB.SetTrickplay(ctx, id, string(j), blobs)
	if err != nil {
		cleanup()
		return err
	}
	s.RemoveAssets(old)
	return nil
}

// ---- 定时扫描 ----

// RunScheduler 启动时扫描一次所有媒体库，之后按设置的间隔定时扫描，并维护目录监听。
func (s *Service) RunScheduler(ctx context.Context) {
	libs, _ := s.DB.ListLibraries(ctx)
	for _, l := range libs {
		s.EnqueueScan(l.ID, false)
	}
	s.RestartWatcher()
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stopWatcher()
			return
		case <-t.C:
			st := s.Settings(ctx)
			if st.ScanIntervalHours <= 0 {
				continue
			}
			libs, _ := s.DB.ListLibraries(ctx)
			for _, l := range libs {
				if time.Since(time.Unix(l.LastScanAt, 0)) >= time.Duration(st.ScanIntervalHours)*time.Hour {
					s.EnqueueScan(l.ID, false)
				}
			}
		}
	}
}
