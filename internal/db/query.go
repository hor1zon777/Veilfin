package db

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
)

// Progress 是某个用户对某个条目的观看状态。
type Progress struct {
	Position   float64 `json:"position"`
	Played     bool    `json:"played"`
	PlayCount  int     `json:"playCount"`
	LastPlayed int64   `json:"lastPlayed"`
}

type ItemRow struct {
	*Item
	Progress
}

var qualifiedItemCols = func() string {
	parts := strings.Split(strings.ReplaceAll(itemCols, "\n", ""), ",")
	for i, p := range parts {
		parts[i] = "items." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ",")
}()

const progressCols = "COALESCE(p.position,0),COALESCE(p.played,0),COALESCE(p.play_count,0),COALESCE(p.last_played_at,0)"

func scanItemRow(rows *sql.Rows) (*ItemRow, error) {
	var it Item
	var r ItemRow
	err := rows.Scan(&it.ID, &it.LibraryID, &it.Path, &it.Folder, &it.Size, &it.Mtime, &it.VideoBlob, &it.Mime, &it.Title,
		&it.SortTitle, &it.OriginalTitle, &it.Overview, &it.Year, &it.ReleaseDate, &it.Code, &it.Studio, &it.Director,
		&it.Series, &it.Rating, &it.Duration, &it.Width, &it.Height, &it.VCodec, &it.ACodec, &it.Container, &it.Bitrate,
		&it.Poster, &it.Backdrop, &it.Thumb, &it.Trickplay, &it.Locked, &it.MetaSource, &it.ScanState, &it.AddedAt, &it.UpdatedAt,
		&r.Position, &r.Played, &r.PlayCount, &r.LastPlayed)
	r.Item = &it
	return &r, err
}

func collectRows(rows *sql.Rows, err error) ([]*ItemRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ItemRow
	for rows.Next() {
		r, err := scanItemRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type ItemQuery struct {
	UserID     int64
	Libraries  []int64 // 允许访问的媒体库（调用方负责权限过滤）
	Folder     *string // 只看某个文件夹（不含子文件夹）
	FolderTree *string // 某个文件夹及其所有子文件夹（"" 表示整个媒体库）
	Q          string
	Tags       []TagFilter // 需要同时满足的标签条件
	Studio     string
	Series     string
	YearFrom   int
	YearTo     int
	Res        string // 4k | 1080 | 720 | sd
	Dur        string // short | medium | long | xlong
	Status     string // unplayed | played | inprogress
	Played     *bool  // 旧参数，等价于 Status=played / unplayed
	Sort       string // added | title | release | duration | rating | lastPlayed | random
	Desc       bool
	Seed       uint64 // 随机排序的种子：种子相同则顺序相同，分页时不会重复或遗漏
	Offset     int
	Limit      int
}

// TagFilter 是一个标签条件，Kind 为 genre | tag | actor。
type TagFilter struct {
	Kind string
	Name string
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// prefixArg 生成“以 s 开头”的 LIKE 参数。
func prefixArg(s string) string { return likeEscaper.Replace(s) + "%" }

// likeArg 生成“包含 s”的 LIKE 参数。
func likeArg(s string) string { return "%" + likeEscaper.Replace(s) + "%" }

// 分辨率档位，和前端 resolutionLabel 的划分一致（长边或短边满足其一即可，兼容竖屏视频）
const (
	res4K   = "(items.width>=3800 OR MIN(items.width,items.height)>=2000)"
	res1080 = "(items.width>=1900 OR MIN(items.width,items.height)>=1000)"
	res720  = "(items.width>=1200 OR MIN(items.width,items.height)>=700)"
)

var resConds = map[string]string{
	"4k":   res4K,
	"1080": res1080 + " AND NOT " + res4K,
	"720":  res720 + " AND NOT " + res1080,
	"sd":   "items.width>0 AND NOT " + res720,
}

var durConds = map[string]string{
	"short":  "items.duration>0 AND items.duration<1200",
	"medium": "items.duration>=1200 AND items.duration<3600",
	"long":   "items.duration>=3600 AND items.duration<7200",
	"xlong":  "items.duration>=7200",
}

var tagKinds = map[string]bool{"genre": true, "tag": true, "actor": true}

// itemsFrom 根据查询条件生成 FROM ... WHERE 子句和参数（第一个参数是用户 ID）。
func itemsFrom(q ItemQuery) (string, []any) {
	where := []string{"items.library_id IN (" + placeholders(len(q.Libraries)) + ")"}
	args := int64Args(q.Libraries)
	if q.Folder != nil {
		where = append(where, "items.folder = ?")
		args = append(args, *q.Folder)
	} else if q.FolderTree != nil && *q.FolderTree != "" {
		where = append(where, `(items.folder = ? OR items.folder LIKE ? ESCAPE '\')`)
		args = append(args, *q.FolderTree, prefixArg(*q.FolderTree+"/"))
	}
	if s := strings.TrimSpace(q.Q); s != "" {
		l := likeArg(s)
		where = append(where, `(items.title LIKE ? ESCAPE '\' OR items.original_title LIKE ? ESCAPE '\' OR items.code LIKE ? ESCAPE '\'
			OR EXISTS(SELECT 1 FROM item_tags t WHERE t.item_id=items.id AND t.name LIKE ? ESCAPE '\'))`)
		args = append(args, l, l, l, l)
	}
	for _, t := range q.Tags {
		if !tagKinds[t.Kind] || t.Name == "" {
			continue
		}
		where = append(where, "EXISTS(SELECT 1 FROM item_tags t WHERE t.item_id=items.id AND t.kind=? AND t.name=?)")
		args = append(args, t.Kind, t.Name)
	}
	if q.Studio != "" {
		where = append(where, "items.studio = ?")
		args = append(args, q.Studio)
	}
	if q.Series != "" {
		where = append(where, "items.series = ?")
		args = append(args, q.Series)
	}
	if q.YearFrom > 0 {
		where = append(where, "items.year >= ?")
		args = append(args, q.YearFrom)
	}
	if q.YearTo > 0 {
		where = append(where, "items.year > 0 AND items.year <= ?")
		args = append(args, q.YearTo)
	}
	if c := resConds[q.Res]; c != "" {
		where = append(where, c)
	}
	if c := durConds[q.Dur]; c != "" {
		where = append(where, c)
	}
	status := q.Status
	if status == "" && q.Played != nil {
		status = "unplayed"
		if *q.Played {
			status = "played"
		}
	}
	switch status {
	case "played":
		where = append(where, "COALESCE(p.played,0)=1")
	case "unplayed":
		where = append(where, "COALESCE(p.played,0)=0")
	case "inprogress":
		where = append(where, "COALESCE(p.position,0)>0")
	}
	from := " FROM items LEFT JOIN progress p ON p.item_id=items.id AND p.user_id=? WHERE " + strings.Join(where, " AND ")
	return from, append([]any{q.UserID}, args...)
}

func itemsOrder(q ItemQuery) string {
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	var order string
	switch q.Sort {
	case "title":
		order = "items.sort_title " + dir
	case "release":
		order = "items.release_date " + dir + ", items.year " + dir
	case "duration":
		order = "items.duration " + dir
	case "rating":
		order = "items.rating " + dir
	case "lastPlayed":
		order = "COALESCE(p.last_played_at,0) " + dir
	default:
		order = "items.added_at " + dir
	}
	return order + ", items.id " + dir
}

func scanIDs(rows *sql.Rows, err error) ([]int64, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// shuffledIDs 取出所有符合条件的条目 ID，并按种子打乱。
func (d *DB) shuffledIDs(ctx context.Context, from string, args []any, seed uint64) ([]int64, error) {
	ids, err := scanIDs(d.R.QueryContext(ctx, "SELECT items.id"+from+" ORDER BY items.id", args...))
	if err != nil {
		return nil, err
	}
	r := rand.New(rand.NewPCG(seed, 0x5eed))
	r.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	return ids, nil
}

// QueryItems 按条件分页查询条目，返回结果和总数。
func (d *DB) QueryItems(ctx context.Context, q ItemQuery) ([]*ItemRow, int, error) {
	if len(q.Libraries) == 0 {
		return nil, 0, nil
	}
	from, args := itemsFrom(q)
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 60
	}
	offset := max(q.Offset, 0)

	if q.Sort == "random" {
		ids, err := d.shuffledIDs(ctx, from, args, q.Seed)
		if err != nil {
			return nil, 0, err
		}
		total := len(ids)
		if offset >= total {
			return nil, total, nil
		}
		page := ids[offset:min(offset+limit, total)]
		rows, err := collectRows(d.R.QueryContext(ctx, "SELECT "+qualifiedItemCols+","+progressCols+
			" FROM items LEFT JOIN progress p ON p.item_id=items.id AND p.user_id=? WHERE items.id IN ("+placeholders(len(page))+")",
			append([]any{q.UserID}, int64Args(page)...)...))
		if err != nil {
			return nil, 0, err
		}
		pos := make(map[int64]int, len(page))
		for i, id := range page {
			pos[id] = i
		}
		sort.Slice(rows, func(i, j int) bool { return pos[rows[i].ID] < pos[rows[j].ID] })
		return rows, total, nil
	}

	var total int
	if err := d.R.QueryRowContext(ctx, "SELECT COUNT(*)"+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := collectRows(d.R.QueryContext(ctx, "SELECT "+qualifiedItemCols+","+progressCols+from+" ORDER BY "+itemsOrder(q)+" LIMIT ? OFFSET ?", args...))
	return rows, total, err
}

// QueryItemIDs 只返回符合条件的条目 ID（用于“随机播放”等播放队列），最多 limit 个。
func (d *DB) QueryItemIDs(ctx context.Context, q ItemQuery, limit int) ([]int64, error) {
	if len(q.Libraries) == 0 {
		return nil, nil
	}
	from, args := itemsFrom(q)
	if q.Sort == "random" {
		ids, err := d.shuffledIDs(ctx, from, args, q.Seed)
		if len(ids) > limit {
			ids = ids[:limit]
		}
		return ids, err
	}
	return scanIDs(d.R.QueryContext(ctx, "SELECT items.id"+from+" ORDER BY "+itemsOrder(q)+" LIMIT ?", append(args, limit)...))
}

// ItemRowByID 返回条目和该用户的观看进度。
func (d *DB) ItemRowByID(ctx context.Context, uid, id int64) (*ItemRow, error) {
	rows, err := collectRows(d.R.QueryContext(ctx, "SELECT "+qualifiedItemCols+","+progressCols+
		" FROM items LEFT JOIN progress p ON p.item_id=items.id AND p.user_id=? WHERE items.id=?", uid, id))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	return rows[0], nil
}

// Folder 是文件夹浏览中的一个子文件夹。
type Folder struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// SubFolders 返回 parent 下一级的子文件夹以及各自包含的视频数（含更深层）。
func (d *DB) SubFolders(ctx context.Context, lib int64, parent string) ([]Folder, error) {
	var rows *sql.Rows
	var err error
	if parent == "" {
		rows, err = d.R.QueryContext(ctx, "SELECT folder, COUNT(*) FROM items WHERE library_id=? AND folder<>'' GROUP BY folder", lib)
	} else {
		rows, err = d.R.QueryContext(ctx, `SELECT folder, COUNT(*) FROM items WHERE library_id=? AND folder LIKE ? ESCAPE '\' GROUP BY folder`,
			lib, strings.TrimSuffix(likeArg(parent+"/"), "%")[1:]+"%")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var f string
		var n int
		rows.Scan(&f, &n)
		rest := f
		if parent != "" {
			rest = strings.TrimPrefix(f, parent+"/")
		}
		child := strings.SplitN(rest, "/", 2)[0]
		if child == "" {
			continue
		}
		counts[child] += n
	}
	out := make([]Folder, 0, len(counts))
	for name, n := range counts {
		p := name
		if parent != "" {
			p = parent + "/" + name
		}
		out = append(out, Folder{Path: p, Name: name, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, rows.Err()
}

// ---- 观看进度 ----

const (
	playedRatio = 0.90 // 播放超过 90% 视为看完
	resumeRatio = 0.05 // 不到 5% 不计入“继续观看”
)

// ReportProgress 记录一次播放进度上报，返回最新状态。
func (d *DB) ReportProgress(ctx context.Context, uid, itemID int64, psid string, pos, dur float64, t int64) (*Progress, error) {
	var out Progress
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		// 播放事件：累计观看时长。用“墙钟间隔”和“进度变化”中较小的一个，拖动和暂停都不会被算进去。
		var evID int64
		var updated int64
		var lastPos float64
		err := tx.QueryRowContext(ctx, "SELECT id, updated_at, last_pos FROM play_events WHERE psid=? AND user_id=?", psid, uid).Scan(&evID, &updated, &lastPos)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, "INSERT INTO play_events(psid,user_id,item_id,started_at,updated_at,last_pos) VALUES(?,?,?,?,?,?)",
				psid, uid, itemID, t, t, pos); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			add := 0.0
			if wall := float64(t - updated); wall > 0 && wall <= 120 {
				add = math.Min(wall+1, math.Abs(pos-lastPos))
			}
			if _, err := tx.ExecContext(ctx, "UPDATE play_events SET updated_at=?, last_pos=?, watched=watched+? WHERE id=?", t, pos, add, evID); err != nil {
				return err
			}
		}

		var cur Progress
		err = tx.QueryRowContext(ctx, "SELECT position, played, play_count, last_played_at FROM progress WHERE user_id=? AND item_id=?", uid, itemID).
			Scan(&cur.Position, &cur.Played, &cur.PlayCount, &cur.LastPlayed)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		out = cur
		out.LastPlayed = t
		if dur > 0 && pos/dur >= playedRatio {
			if !cur.Played || cur.Position > 0 {
				out.PlayCount++
			}
			out.Played, out.Position = true, 0
		} else {
			out.Position = pos
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO progress(user_id,item_id,position,played,play_count,last_played_at) VALUES(?,?,?,?,?,?)
			ON CONFLICT(user_id,item_id) DO UPDATE SET position=excluded.position, played=excluded.played,
			play_count=excluded.play_count, last_played_at=excluded.last_played_at`,
			uid, itemID, out.Position, out.Played, out.PlayCount, out.LastPlayed)
		return err
	})
	return &out, err
}

// SetPlayed 手动标记已看 / 未看。
func (d *DB) SetPlayed(ctx context.Context, uid, itemID int64, played bool) error {
	if played {
		_, err := d.W.ExecContext(ctx, `INSERT INTO progress(user_id,item_id,position,played,play_count,last_played_at) VALUES(?,?,0,1,1,?)
			ON CONFLICT(user_id,item_id) DO UPDATE SET position=0, played=1, play_count=MAX(play_count,1)`, uid, itemID, now())
		return err
	}
	_, err := d.W.ExecContext(ctx, "UPDATE progress SET position=0, played=0 WHERE user_id=? AND item_id=?", uid, itemID)
	return err
}

// ContinueWatching 返回看了一部分、还没看完的条目。
func (d *DB) ContinueWatching(ctx context.Context, uid int64, libs []int64, limit int) ([]*ItemRow, error) {
	if len(libs) == 0 {
		return nil, nil
	}
	args := append([]any{uid}, int64Args(libs)...)
	args = append(args, resumeRatio, limit)
	return collectRows(d.R.QueryContext(ctx, "SELECT "+qualifiedItemCols+","+progressCols+
		` FROM progress p JOIN items ON items.id=p.item_id WHERE p.user_id=? AND items.library_id IN (`+placeholders(len(libs))+`)
		AND p.position > 0 AND (items.duration=0 OR p.position >= items.duration*?)
		ORDER BY p.last_played_at DESC LIMIT ?`, args...))
}

// Latest 返回某个媒体库最近添加的条目。
func (d *DB) Latest(ctx context.Context, uid, lib int64, limit int) ([]*ItemRow, error) {
	return collectRows(d.R.QueryContext(ctx, "SELECT "+qualifiedItemCols+","+progressCols+
		" FROM items LEFT JOIN progress p ON p.item_id=items.id AND p.user_id=? WHERE items.library_id=? ORDER BY items.added_at DESC, items.id DESC LIMIT ?",
		uid, lib, limit))
}

// HistoryEntry 是一次播放记录。
type HistoryEntry struct {
	Row       *ItemRow
	StartedAt int64
	Watched   float64
}

func (d *DB) History(ctx context.Context, uid int64, libs []int64, offset, limit int) ([]*HistoryEntry, int, error) {
	if len(libs) == 0 {
		return nil, 0, nil
	}
	libArgs := int64Args(libs)
	var total int
	cargs := append([]any{uid}, libArgs...)
	if err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM play_events e JOIN items ON items.id=e.item_id
		WHERE e.user_id=? AND items.library_id IN (`+placeholders(len(libs))+`)`, cargs...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args := append([]any{uid, uid}, libArgs...)
	args = append(args, limit, offset)
	rows, err := d.R.QueryContext(ctx, "SELECT "+qualifiedItemCols+","+progressCols+`, e.started_at, e.watched
		FROM play_events e JOIN items ON items.id=e.item_id LEFT JOIN progress p ON p.item_id=items.id AND p.user_id=?
		WHERE e.user_id=? AND items.library_id IN (`+placeholders(len(libs))+`) ORDER BY e.started_at DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*HistoryEntry
	for rows.Next() {
		var it Item
		var r ItemRow
		var h HistoryEntry
		if err := rows.Scan(&it.ID, &it.LibraryID, &it.Path, &it.Folder, &it.Size, &it.Mtime, &it.VideoBlob, &it.Mime, &it.Title,
			&it.SortTitle, &it.OriginalTitle, &it.Overview, &it.Year, &it.ReleaseDate, &it.Code, &it.Studio, &it.Director,
			&it.Series, &it.Rating, &it.Duration, &it.Width, &it.Height, &it.VCodec, &it.ACodec, &it.Container, &it.Bitrate,
			&it.Poster, &it.Backdrop, &it.Thumb, &it.Trickplay, &it.Locked, &it.MetaSource, &it.ScanState, &it.AddedAt, &it.UpdatedAt,
			&r.Position, &r.Played, &r.PlayCount, &r.LastPlayed, &h.StartedAt, &h.Watched); err != nil {
			return nil, 0, err
		}
		r.Item = &it
		h.Row = &r
		out = append(out, &h)
	}
	return out, total, rows.Err()
}

func (d *DB) ClearHistory(ctx context.Context, uid int64) error {
	_, err := d.W.ExecContext(ctx, "DELETE FROM play_events WHERE user_id=?", uid)
	return err
}

// ---- 统计 ----

type UserStat struct {
	UserID   int64   `json:"userId"`
	Username string  `json:"username"`
	Plays    int     `json:"plays"`
	Watched  float64 `json:"watched"`
	LastPlay int64   `json:"lastPlay"`
}

type DayStat struct {
	Day     string  `json:"day"`
	Watched float64 `json:"watched"`
	Plays   int     `json:"plays"`
}

type TopItem struct {
	ItemID    int64   `json:"itemId"`
	LibraryID int64   `json:"-"`
	Title     string  `json:"title"`
	Plays     int     `json:"plays"`
	Watched   float64 `json:"watched"`
}

type Stats struct {
	Users         []UserStat `json:"users"`
	Days          []DayStat  `json:"days"`
	Top           []TopItem  `json:"top"`
	ItemCount     int        `json:"itemCount"`
	TotalSize     int64      `json:"totalSize"`
	TotalDuration float64    `json:"totalDuration"`
}

func (d *DB) Stats(ctx context.Context, since int64) (*Stats, error) {
	var s Stats
	if err := d.R.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(size),0), COALESCE(SUM(duration),0) FROM items").
		Scan(&s.ItemCount, &s.TotalSize, &s.TotalDuration); err != nil {
		return nil, err
	}
	rows, err := d.R.QueryContext(ctx, `SELECT u.id, u.username, COUNT(e.id), COALESCE(SUM(e.watched),0), COALESCE(MAX(e.started_at),0)
		FROM users u LEFT JOIN play_events e ON e.user_id=u.id AND e.started_at>=? GROUP BY u.id ORDER BY 4 DESC`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u UserStat
		rows.Scan(&u.UserID, &u.Username, &u.Plays, &u.Watched, &u.LastPlay)
		s.Users = append(s.Users, u)
	}
	rows.Close()
	rows, err = d.R.QueryContext(ctx, `SELECT date(started_at,'unixepoch','localtime') d, COALESCE(SUM(watched),0), COUNT(*)
		FROM play_events WHERE started_at>=? GROUP BY d ORDER BY d`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var x DayStat
		rows.Scan(&x.Day, &x.Watched, &x.Plays)
		s.Days = append(s.Days, x)
	}
	rows.Close()
	rows, err = d.R.QueryContext(ctx, `SELECT e.item_id, i.library_id, i.title, COUNT(*), COALESCE(SUM(e.watched),0) FROM play_events e
		JOIN items i ON i.id=e.item_id WHERE e.started_at>=? GROUP BY e.item_id ORDER BY 4 DESC, 5 DESC LIMIT 10`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var x TopItem
		rows.Scan(&x.ItemID, &x.LibraryID, &x.Title, &x.Plays, &x.Watched)
		s.Top = append(s.Top, x)
	}
	rows.Close()
	return &s, nil
}
