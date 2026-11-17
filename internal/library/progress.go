package library

// 媒体库扫描进度：扫描文件 → 处理条目（探测、元数据、刮削、截图、字幕）→ 生成预览图，按媒体库统计，供管理后台显示。

import (
	"sync"
	"time"
)

// ScanProgress 是某个媒体库最近一次扫描的进度。
type ScanProgress struct {
	LibraryID  int64  `json:"libraryId"`
	Phase      string `json:"phase"` // queued 排队 | scanning 扫描文件 | processing 处理条目 / 生成预览图 | done 完成 | failed 失败
	Refresh    bool   `json:"refresh"`
	Found      int    `json:"found"`   // 扫描到的视频文件数
	Added      int    `json:"added"`   // 新增的条目
	Removed    int    `json:"removed"` // 删除的条目（文件已不存在）
	Total      int    `json:"total"`   // 需要处理的条目数
	Done       int    `json:"done"`    // 已处理的条目数
	Failed     int    `json:"failed"`  // 处理失败的条目数
	TrickTotal int    `json:"trickTotal"`
	TrickDone  int    `json:"trickDone"`
	Err        string `json:"err,omitempty"`
	Started    int64  `json:"started"`
	Ended      int64  `json:"ended,omitempty"`
}

type scanTrack struct {
	p      ScanProgress
	items  map[int64]bool // 尚未处理完的条目
	tricks map[int64]bool // 尚未生成完预览图的条目
}

type progressSet struct {
	mu      sync.Mutex
	tracks  map[int64]*scanTrack // 媒体库 ID → 进度
	itemLib map[int64]int64      // 正在跟踪的条目 → 所属媒体库
}

// progressKeep 是扫描结束后进度还保留多久（供页面显示“完成”）。
const progressKeep = 10 * time.Minute

func (ps *progressSet) init() {
	if ps.tracks == nil {
		ps.tracks = map[int64]*scanTrack{}
		ps.itemLib = map[int64]int64{}
	}
}

func (t *scanTrack) active() bool { return t.p.Phase != "done" && t.p.Phase != "failed" }

// queued 在扫描加入队列时调用；正在进行的扫描不重置。
func (ps *progressSet) queued(libID int64, refresh bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.init()
	if t := ps.tracks[libID]; t != nil && t.active() {
		return
	}
	ps.reset(libID, "queued", refresh)
}

func (ps *progressSet) reset(libID int64, phase string, refresh bool) *scanTrack {
	if old := ps.tracks[libID]; old != nil {
		for id := range old.items {
			delete(ps.itemLib, id)
		}
		for id := range old.tricks {
			delete(ps.itemLib, id)
		}
	}
	t := &scanTrack{
		p:     ScanProgress{LibraryID: libID, Phase: phase, Refresh: refresh, Started: time.Now().Unix()},
		items: map[int64]bool{}, tricks: map[int64]bool{},
	}
	ps.tracks[libID] = t
	return t
}

// update 在锁内修改某个媒体库的进度。
func (ps *progressSet) update(libID int64, fn func(t *scanTrack)) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.init()
	t := ps.tracks[libID]
	if t == nil {
		t = ps.reset(libID, "scanning", false)
	}
	fn(t)
}

func (ps *progressSet) startScan(libID int64, refresh bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.init()
	t := ps.tracks[libID]
	if t == nil || t.p.Phase != "queued" {
		t = ps.reset(libID, "scanning", refresh)
	}
	t.p.Phase = "scanning"
	t.p.Started = time.Now().Unix()
}

func (ps *progressSet) failScan(libID int64, err error) {
	ps.update(libID, func(t *scanTrack) {
		t.p.Phase, t.p.Err, t.p.Ended = "failed", err.Error(), time.Now().Unix()
	})
}

// trackItems 在扫描结束、条目加入处理队列前调用。
func (ps *progressSet) trackItems(libID int64, items, tricks []int64) {
	ps.update(libID, func(t *scanTrack) {
		for _, id := range items {
			t.items[id] = true
			ps.itemLib[id] = libID
		}
		for _, id := range tricks {
			t.tricks[id] = true
			ps.itemLib[id] = libID
		}
		t.p.Total, t.p.TrickTotal = len(t.items), len(t.tricks)
		t.p.Phase = "processing"
		ps.finishIfIdle(t)
	})
}

// trickQueued 在条目处理完、排队生成预览图时调用：属于正在跟踪的扫描就计入。
func (ps *progressSet) trickQueued(id int64) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	libID, ok := ps.itemLib[id]
	if !ok {
		return
	}
	if t := ps.tracks[libID]; t != nil && t.active() && !t.tricks[id] {
		t.tricks[id] = true
		t.p.TrickTotal++
	}
}

// itemDone / trickDone 在任务结束时调用（无论成败）。
func (ps *progressSet) itemDone(id int64, err error) { ps.finish(id, false, err) }
func (ps *progressSet) trickDone(id int64)           { ps.finish(id, true, nil) }

func (ps *progressSet) finish(id int64, trick bool, err error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	libID, ok := ps.itemLib[id]
	if !ok {
		return
	}
	t := ps.tracks[libID]
	if t == nil {
		delete(ps.itemLib, id)
		return
	}
	if trick {
		if t.tricks[id] {
			delete(t.tricks, id)
			t.p.TrickDone++
		}
	} else if t.items[id] {
		delete(t.items, id)
		t.p.Done++
		if err != nil {
			t.p.Failed++
		}
	}
	if !t.items[id] && !t.tricks[id] {
		delete(ps.itemLib, id)
	}
	ps.finishIfIdle(t)
}

func (ps *progressSet) finishIfIdle(t *scanTrack) {
	if t.p.Phase == "processing" && len(t.items) == 0 && len(t.tricks) == 0 {
		t.p.Phase, t.p.Ended = "done", time.Now().Unix()
	}
}

// snapshot 返回正在进行或最近结束的扫描进度。
func (ps *progressSet) snapshot() []ScanProgress {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := []ScanProgress{}
	now := time.Now().Unix()
	for id, t := range ps.tracks {
		if !t.active() && now-t.p.Ended > int64(progressKeep/time.Second) {
			delete(ps.tracks, id)
			continue
		}
		out = append(out, t.p)
	}
	return out
}

// ScanProgress 返回各媒体库正在进行或最近结束（10 分钟内）的扫描进度。
func (s *Service) ScanProgress() []ScanProgress { return s.progress.snapshot() }
