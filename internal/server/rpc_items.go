package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"veilfin/internal/api"
	"veilfin/internal/db"
	"veilfin/internal/library"
)

func init() {
	register("home.get", user, rpcHome)
	register("lib.list", user, rpcLibList)
	register("items.list", user, rpcItemsList)
	register("items.ids", user, rpcItemIDs)
	register("items.get", user, rpcItemGet)
	register("items.filters", user, rpcFilters)
	register("items.setPlayed", user, rpcSetPlayed)
	register("play.open", user, rpcPlayOpen)
	register("play.report", user, rpcPlayReport)
	register("history.list", user, rpcHistory)
	register("history.clear", user, func(s *Server, c *call) (any, error) {
		return struct{}{}, s.DB.ClearHistory(c.ctx, c.u.ID)
	})
}

// visibility 计算当前会话能看到哪些媒体库。受限库在用户设置了 PIN 且会话未解锁时处于“锁定”状态。
type visibility struct {
	libs     map[int64]*db.Library // 有权限的媒体库
	order    []*db.Library
	visible  []int64
	locked   map[int64]bool
	unlocked bool
}

func (s *Server) visibility(c *call) (*visibility, error) {
	a, err := s.access(c.ctx, c.u.ID)
	if err != nil {
		return nil, err
	}
	all, err := s.DB.ListLibraries(c.ctx)
	if err != nil {
		return nil, err
	}
	v := &visibility{libs: map[int64]*db.Library{}, locked: map[int64]bool{}}
	v.unlocked = c.u.PinHash == "" || c.ss.unlocked(s.now())
	for _, l := range all {
		if !a.allowed[l.ID] && !c.u.IsAdmin {
			continue
		}
		v.libs[l.ID] = l
		v.order = append(v.order, l)
		if l.Restricted && !v.unlocked {
			v.locked[l.ID] = true
			continue
		}
		v.visible = append(v.visible, l.ID)
	}
	return v, nil
}

// checkItem 校验当前用户能否访问条目。
func (s *Server) checkItem(c *call, v *visibility, id int64) (*db.ItemRow, error) {
	row, err := s.DB.ItemRowByID(c.ctx, c.u.ID, id)
	if err != nil {
		return nil, err
	}
	if _, ok := v.libs[row.LibraryID]; !ok {
		return nil, errf(api.CodeNotFound, "记录不存在")
	}
	if v.locked[row.LibraryID] {
		return nil, errf(api.CodeLocked, "受限内容，请先输入 PIN 解锁")
	}
	return row, nil
}

// card 是列表中的条目卡片。
type card struct {
	ID         int64        `json:"id"`
	LibraryID  int64        `json:"libraryId"`
	Title      string       `json:"title"`
	Year       int          `json:"year"`
	Code       string       `json:"code,omitempty"`
	Duration   float64      `json:"duration"`
	Width      int          `json:"width"`
	Height     int          `json:"height"`
	AddedAt    int64        `json:"addedAt"`
	Poster     *api.BlobRef `json:"poster"`
	Thumb      *api.BlobRef `json:"thumb"`
	Restricted bool         `json:"restricted"`
	Position   float64      `json:"position"`
	Played     bool         `json:"played"`
	PlayCount  int          `json:"playCount"`
	LastPlayed int64        `json:"lastPlayed"`
}

func (s *Server) cards(c *call, v *visibility, rows []*db.ItemRow) ([]*card, error) {
	ids := make([]string, 0, len(rows)*2)
	for _, r := range rows {
		ids = append(ids, r.Poster, r.Thumb)
	}
	refs, err := s.refs(c.ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*card, 0, len(rows))
	for _, r := range rows {
		lib := v.libs[r.LibraryID]
		out = append(out, &card{
			ID: r.ID, LibraryID: r.LibraryID, Title: r.Title, Year: r.Year, Code: r.Code, Duration: r.Duration,
			Width: r.Width, Height: r.Height, AddedAt: r.AddedAt, Poster: refs[r.Poster], Thumb: refs[r.Thumb],
			Restricted: lib != nil && lib.Restricted, Position: r.Position, Played: r.Played, PlayCount: r.PlayCount,
			LastPlayed: r.LastPlayed,
		})
	}
	return out, nil
}

type libInfo struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Restricted bool   `json:"restricted"`
	Locked     bool   `json:"locked"`
	Count      int    `json:"count"`
}

func (s *Server) libInfos(c *call, v *visibility) ([]libInfo, error) {
	counts, err := s.DB.LibraryCounts(c.ctx)
	if err != nil {
		return nil, err
	}
	out := make([]libInfo, 0, len(v.order))
	for _, l := range v.order {
		out = append(out, libInfo{ID: l.ID, Name: l.Name, Kind: l.Kind, Restricted: l.Restricted, Locked: v.locked[l.ID], Count: counts[l.ID]})
	}
	return out, nil
}

func rpcLibList(s *Server, c *call) (any, error) {
	v, err := s.visibility(c)
	if err != nil {
		return nil, err
	}
	return s.libInfos(c, v)
}

func rpcHome(s *Server, c *call) (any, error) {
	v, err := s.visibility(c)
	if err != nil {
		return nil, err
	}
	libs, err := s.libInfos(c, v)
	if err != nil {
		return nil, err
	}
	cont, err := s.DB.ContinueWatching(c.ctx, c.u.ID, v.visible, 20)
	if err != nil {
		return nil, err
	}
	contCards, err := s.cards(c, v, cont)
	if err != nil {
		return nil, err
	}
	type latestRow struct {
		Library libInfo `json:"library"`
		Items   []*card `json:"items"`
	}
	var latest []latestRow
	for _, li := range libs {
		if li.Locked {
			continue
		}
		rows, err := s.DB.Latest(c.ctx, c.u.ID, li.ID, 16)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			continue
		}
		cs, err := s.cards(c, v, rows)
		if err != nil {
			return nil, err
		}
		latest = append(latest, latestRow{Library: li, Items: cs})
	}
	return map[string]any{"libraries": libs, "continue": contCards, "latest": latest, "me": s.me(c)}, nil
}

// listParams 是 items.list / items.ids 共用的筛选参数。
type listParams struct {
	LibraryID int64  `json:"libraryId"`
	Browse    bool   `json:"browse"` // 按文件夹浏览
	Folder    string `json:"folder"`
	Q         string `json:"q"`
	Kind      string `json:"kind"` // 旧参数：kind + tag 表示单个标签条件
	Tag       string `json:"tag"`
	Genre     string `json:"genre"`
	Label     string `json:"label"` // “标签”类（kind=tag）
	Actor     string `json:"actor"`
	Studio    string `json:"studio"`
	Series    string `json:"series"`
	Year      string `json:"year"` // "2023" 或 "1990-1999"
	Res       string `json:"res"`
	Dur       string `json:"dur"`
	Status    string `json:"status"`
	Played    *bool  `json:"played"`
	Sort      string `json:"sort"`
	Desc      bool   `json:"desc"`
	Seed      uint64 `json:"seed"`
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
}

// parseYears 解析 "2023" 或 "1990-1999"。
func parseYears(s string) (from, to int) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	from, _ = strconv.Atoi(strings.TrimSpace(a))
	to = from
	if ok {
		to, _ = strconv.Atoi(strings.TrimSpace(b))
	}
	return from, to
}

// itemQuery 把参数转换成数据库查询，并校验媒体库权限。
func (s *Server) itemQuery(c *call, v *visibility, p *listParams) (db.ItemQuery, error) {
	q := db.ItemQuery{UserID: c.u.ID, Q: p.Q, Studio: p.Studio, Series: p.Series, Res: p.Res, Dur: p.Dur,
		Status: p.Status, Played: p.Played, Sort: p.Sort, Desc: p.Desc, Seed: p.Seed, Offset: p.Offset, Limit: p.Limit}
	q.YearFrom, q.YearTo = parseYears(p.Year)
	for _, t := range []db.TagFilter{{Kind: p.Kind, Name: p.Tag}, {Kind: "genre", Name: p.Genre}, {Kind: "tag", Name: p.Label}, {Kind: "actor", Name: p.Actor}} {
		if t.Kind != "" && t.Name != "" {
			q.Tags = append(q.Tags, t)
		}
	}
	if p.LibraryID != 0 {
		if _, ok := v.libs[p.LibraryID]; !ok {
			return q, errf(api.CodeNotFound, "媒体库不存在")
		}
		if v.locked[p.LibraryID] {
			return q, errf(api.CodeLocked, "受限内容，请先输入 PIN 解锁")
		}
		q.Libraries = []int64{p.LibraryID}
	} else {
		q.Libraries = v.visible
	}
	return q, nil
}

func rpcItemsList(s *Server, c *call) (any, error) {
	var p listParams
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	v, err := s.visibility(c)
	if err != nil {
		return nil, err
	}
	q, err := s.itemQuery(c, v, &p)
	if err != nil {
		return nil, err
	}
	var folders []db.Folder
	if p.Browse && p.LibraryID != 0 {
		folder := strings.Trim(p.Folder, "/")
		q.Folder = &folder
		if p.Offset == 0 {
			if folders, err = s.DB.SubFolders(c.ctx, p.LibraryID, folder); err != nil {
				return nil, err
			}
		}
	}
	rows, total, err := s.DB.QueryItems(c.ctx, q)
	if err != nil {
		return nil, err
	}
	cs, err := s.cards(c, v, rows)
	if err != nil {
		return nil, err
	}
	return map[string]any{"items": cs, "total": total, "folders": folders}, nil
}

// rpcItemIDs 返回符合筛选条件的条目 ID 列表，用于生成播放队列（随机播放）。
// 按文件夹浏览时包含子文件夹里的视频。
func rpcItemIDs(s *Server, c *call) (any, error) {
	var p listParams
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	v, err := s.visibility(c)
	if err != nil {
		return nil, err
	}
	q, err := s.itemQuery(c, v, &p)
	if err != nil {
		return nil, err
	}
	if p.Browse && p.LibraryID != 0 {
		folder := strings.Trim(p.Folder, "/")
		q.FolderTree = &folder
	}
	limit := p.Limit
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	ids, err := s.DB.QueryItemIDs(c.ctx, q, limit)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []int64{}
	}
	return map[string]any{"ids": ids}, nil
}

type detail struct {
	*card
	OriginalTitle string              `json:"originalTitle"`
	Overview      string              `json:"overview"`
	ReleaseDate   string              `json:"releaseDate"`
	Studio        string              `json:"studio"`
	Director      string              `json:"director"`
	Series        string              `json:"series"`
	Rating        float64             `json:"rating"`
	SortTitle     string              `json:"sortTitle"`
	Tags          map[string][]string `json:"tags"`
	Backdrop      *api.BlobRef        `json:"backdrop"`
	Subtitles     []*db.Subtitle      `json:"subtitles"`
	VCodec        string              `json:"vcodec"`
	ACodec        string              `json:"acodec"`
	Container     string              `json:"container"`
	Mime          string              `json:"mime"`
	Bitrate       int64               `json:"bitrate"`
	Size          int64               `json:"size"`
	Folder        string              `json:"folder"`
	MetaSource    string              `json:"metaSource"`
	Locked        []string            `json:"locked"`
	Library       libInfo             `json:"library"`
	Path          string              `json:"path,omitempty"` // 仅管理员可见
	HasTrickplay  bool                `json:"hasTrickplay"`
}

func (s *Server) detail(c *call, v *visibility, row *db.ItemRow) (*detail, error) {
	cs, err := s.cards(c, v, []*db.ItemRow{row})
	if err != nil {
		return nil, err
	}
	tags, err := s.DB.ItemTags(c.ctx, row.ID)
	if err != nil {
		return nil, err
	}
	subs, err := s.DB.Subtitles(c.ctx, row.ID)
	if err != nil {
		return nil, err
	}
	locked, _ := s.DB.ItemLocked(c.ctx, row.ID)
	refs, err := s.refs(c.ctx, []string{row.Backdrop})
	if err != nil {
		return nil, err
	}
	lib := v.libs[row.LibraryID]
	d := &detail{
		card: cs[0], OriginalTitle: row.OriginalTitle, Overview: row.Overview, ReleaseDate: row.ReleaseDate,
		Studio: row.Studio, Director: row.Director, Series: row.Series, Rating: row.Rating, SortTitle: row.SortTitle,
		Tags: tags, Backdrop: refs[row.Backdrop], Subtitles: subs, VCodec: row.VCodec, ACodec: row.ACodec,
		Container: row.Container, Mime: row.Mime, Bitrate: row.Bitrate, Size: row.Size, Folder: row.Folder, MetaSource: row.MetaSource,
		Locked: locked, HasTrickplay: row.Trickplay != "",
		Library: libInfo{ID: lib.ID, Name: lib.Name, Kind: lib.Kind, Restricted: lib.Restricted},
	}
	if c.u.IsAdmin {
		d.Path = row.Path
	}
	return d, nil
}

func rpcItemGet(s *Server, c *call) (any, error) {
	var p struct {
		ID int64 `json:"id"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	v, err := s.visibility(c)
	if err != nil {
		return nil, err
	}
	row, err := s.checkItem(c, v, p.ID)
	if err != nil {
		return nil, err
	}
	return s.detail(c, v, row)
}

func rpcFilters(s *Server, c *call) (any, error) {
	var p struct {
		LibraryID int64 `json:"libraryId"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	v, err := s.visibility(c)
	if err != nil {
		return nil, err
	}
	libs := v.visible
	if p.LibraryID != 0 {
		if _, ok := v.libs[p.LibraryID]; !ok || v.locked[p.LibraryID] {
			return nil, errf(api.CodeNotFound, "媒体库不存在")
		}
		libs = []int64{p.LibraryID}
	}
	out := map[string]any{}
	for _, k := range []string{"genre", "tag", "actor"} {
		names, err := s.DB.TopTags(c.ctx, libs, k, 200)
		if err != nil {
			return nil, err
		}
		out[k] = nonNil(names)
	}
	for _, col := range []string{"studio", "series"} {
		names, err := s.DB.TopColumn(c.ctx, libs, col, 200)
		if err != nil {
			return nil, err
		}
		out[col] = nonNil(names)
	}
	years, err := s.DB.Years(c.ctx, libs)
	if err != nil {
		return nil, err
	}
	out["years"] = nonNil(years)
	return out, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func rpcSetPlayed(s *Server, c *call) (any, error) {
	var p struct {
		ID     int64 `json:"id"`
		Played bool  `json:"played"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	v, err := s.visibility(c)
	if err != nil {
		return nil, err
	}
	if _, err := s.checkItem(c, v, p.ID); err != nil {
		return nil, err
	}
	return struct{}{}, s.DB.SetPlayed(c.ctx, c.u.ID, p.ID, p.Played)
}

func rpcPlayOpen(s *Server, c *call) (any, error) {
	var p struct {
		ID    int64   `json:"id"`
		Queue []int64 `json:"queue"` // 播放队列中接下来的条目：第一个可访问的作为“下一个”
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	v, err := s.visibility(c)
	if err != nil {
		return nil, err
	}
	row, err := s.checkItem(c, v, p.ID)
	if err != nil {
		return nil, err
	}
	// 正在看的视频还没有预览图：插到预览图队列最前面
	s.Lib.PrioritizeTrickplay(c.ctx, row.Item)
	d, err := s.detail(c, v, row)
	if err != nil {
		return nil, err
	}
	ids := []string{row.VideoBlob}
	for _, sub := range d.Subtitles {
		ids = append(ids, sub.Blob)
	}
	var tp *library.TrickplayData
	if row.Trickplay != "" {
		tp = &library.TrickplayData{}
		if json.Unmarshal([]byte(row.Trickplay), tp) == nil {
			ids = append(ids, tp.Sprites...)
		} else {
			tp = nil
		}
	}
	refs, err := s.refs(c.ctx, ids)
	if err != nil {
		return nil, err
	}
	video := refs[row.VideoBlob]
	if video == nil {
		return nil, errf(api.CodeNotFound, "视频文件不存在")
	}
	type subOut struct {
		Label string       `json:"label"`
		Lang  string       `json:"lang"`
		Src   *api.BlobRef `json:"src"`
	}
	subs := []subOut{}
	for _, sub := range d.Subtitles {
		if r := refs[sub.Blob]; r != nil {
			subs = append(subs, subOut{Label: sub.Label, Lang: sub.Lang, Src: r})
		}
	}
	var trick any
	if tp != nil {
		sprites := make([]*api.BlobRef, 0, len(tp.Sprites))
		for _, id := range tp.Sprites {
			sprites = append(sprites, refs[id])
		}
		trick = map[string]any{"interval": tp.Interval, "cols": tp.Cols, "rows": tp.Rows, "width": tp.Width,
			"height": tp.Height, "count": tp.Count, "sprites": sprites}
	}
	tc := s.transcodeInfo(c.ctx, c.u, row.Width, row.Height, row.Duration)
	var next any
	if p.Queue != nil {
		// 播放队列（例如随机播放）：跳过已删除或无权访问的条目
		for _, id := range p.Queue[:min(len(p.Queue), 50)] {
			if r, err := s.checkItem(c, v, id); err == nil && id != row.ID {
				next = map[string]any{"id": r.ID, "title": r.Title}
				break
			}
		}
		return map[string]any{"item": d, "video": video, "subtitles": subs, "trickplay": trick, "next": next, "transcode": tc}, nil
	}
	// 如果当前目录还有其他视频，给出“下一个”，方便连续观看
	folder := row.Folder
	sib, _, err := s.DB.QueryItems(c.ctx, db.ItemQuery{UserID: c.u.ID, Libraries: []int64{row.LibraryID}, Folder: &folder, Sort: "title", Limit: 500})
	if err == nil {
		for i, r := range sib {
			if r.ID == row.ID && i+1 < len(sib) {
				next = map[string]any{"id": sib[i+1].ID, "title": sib[i+1].Title}
				break
			}
		}
	}
	return map[string]any{"item": d, "video": video, "subtitles": subs, "trickplay": trick, "next": next, "transcode": tc}, nil
}

func rpcPlayReport(s *Server, c *call) (any, error) {
	var p struct {
		ID       int64   `json:"id"`
		PSID     string  `json:"psid"`
		Position float64 `json:"position"`
		Duration float64 `json:"duration"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	if p.PSID == "" || len(p.PSID) > 64 || p.Position < 0 {
		return nil, errf(api.CodeBadRequest, "参数错误")
	}
	v, err := s.visibility(c)
	if err != nil {
		return nil, err
	}
	row, err := s.checkItem(c, v, p.ID)
	if err != nil {
		return nil, err
	}
	dur := p.Duration
	if row.Duration > 0 {
		dur = row.Duration // 以服务端探测到的时长为准
	}
	return s.DB.ReportProgress(c.ctx, c.u.ID, p.ID, p.PSID, p.Position, dur, s.now().Unix())
}

func rpcHistory(s *Server, c *call) (any, error) {
	var p struct {
		Offset int `json:"offset"`
		Limit  int `json:"limit"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	v, err := s.visibility(c)
	if err != nil {
		return nil, err
	}
	hs, total, err := s.DB.History(c.ctx, c.u.ID, v.visible, p.Offset, p.Limit)
	if err != nil {
		return nil, err
	}
	rows := make([]*db.ItemRow, len(hs))
	for i, h := range hs {
		rows[i] = h.Row
	}
	cs, err := s.cards(c, v, rows)
	if err != nil {
		return nil, err
	}
	type entry struct {
		*card
		StartedAt int64   `json:"startedAt"`
		Watched   float64 `json:"watched"`
	}
	out := make([]entry, len(hs))
	for i, h := range hs {
		out[i] = entry{card: cs[i], StartedAt: h.StartedAt, Watched: h.Watched}
	}
	return map[string]any{"items": out, "total": total, "now": time.Now().Unix()}, nil
}
