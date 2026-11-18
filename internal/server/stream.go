package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"veilfin/internal/api"
	"veilfin/internal/media"
	"veilfin/internal/mediacrypt"
	"veilfin/internal/transcode"
)

// 播放时转码：play.open 告诉前端可选的画质，play.transcode 创建转码会话，
// 分片通过 GET /s/{会话ID}/{init|序号} 加密下发（不缓存，CDN 只看到不透明的密文）。

func init() {
	register("play.transcode", user, rpcPlayTranscode)
	register("play.transcodeStop", user, rpcPlayTranscodeStop)
}

// encoders 返回可用的 H.264 编码器（第一次调用时探测，之后用缓存）。没有 ffmpeg 时为空。
func (s *Server) encoders(ctx context.Context) []string {
	if s.Lib.FF == nil {
		return nil
	}
	return s.Lib.FF.H264Encoders(context.WithoutCancel(ctx))
}

// transcodeEncoder 返回转码使用的编码器；不能转码（关闭、没有编码器）时返回空字符串。
func (s *Server) transcodeEncoder(ctx context.Context) string {
	st := s.Lib.Settings(ctx)
	if !st.TranscodeEnabled || s.tc == nil {
		return ""
	}
	list := s.encoders(ctx)
	if len(list) == 0 {
		return ""
	}
	if slices.Contains(list, st.TranscodeEncoder) {
		return st.TranscodeEncoder
	}
	return list[0]
}

// encoderInfo 是管理后台显示的编码器列表。
func (s *Server) encoderInfo(ctx context.Context) []map[string]string {
	out := []map[string]string{}
	for _, e := range s.encoders(ctx) {
		out = append(out, map[string]string{"id": e, "label": media.EncoderLabel(e)})
	}
	return out
}

func rpcPlayTranscode(s *Server, c *call) (any, error) {
	var p struct {
		ID      int64   `json:"id"`
		Quality string  `json:"quality"`
		Start   float64 `json:"start"`
		PSID    string  `json:"psid"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	enc := s.transcodeEncoder(c.ctx)
	if enc == "" {
		return nil, errf(api.CodeBadRequest, "服务端没有开启转码，或 ffmpeg 不带可用的 H.264 编码器")
	}
	prof, ok := transcode.FindProfile(p.Quality)
	if !ok {
		return nil, errf(api.CodeBadRequest, "未知的画质")
	}
	v, err := s.visibility(c)
	if err != nil {
		return nil, err
	}
	row, err := s.checkItem(c, v, p.ID)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(transcode.ProfilesFor(row.Width, row.Height), prof) || row.Duration <= 0 {
		return nil, errf(api.CodeBadRequest, "这个视频不能转成该画质")
	}
	if len(p.PSID) > 64 {
		p.PSID = p.PSID[:64]
	}
	sess, err := s.tc.Create(transcode.Options{
		UID: c.u.ID, Owner: p.PSID, LibraryID: row.LibraryID, Path: row.Path,
		Duration: row.Duration, Width: row.Width, Height: row.Height, Bitrate: row.Bitrate,
		Profile: prof, Encoder: enc, Start: p.Start,
	}, s.Lib.Settings(c.ctx).TranscodeMax)
	if errors.Is(err, transcode.ErrBusy) {
		return nil, errf(api.CodeTooMany, "服务端同时转码的播放数已达上限，请稍后再试或切换到原画")
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"stream":  api.StreamRef{ID: sess.ID, Key: sess.Key(), Playlist: sess.Playlist()},
		"quality": prof,
	}, nil
}

func rpcPlayTranscodeStop(s *Server, c *call) (any, error) {
	var p struct {
		ID string `json:"sid"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	if s.tc != nil {
		s.tc.Close(p.ID, c.u.ID)
	}
	return nil, nil
}

// transcodeInfo 是 play.open 返回给前端的转码选项；不能转码时为 nil。
func (s *Server) transcodeInfo(ctx context.Context, w, h int, dur float64) any {
	if dur <= 0 || s.transcodeEncoder(ctx) == "" {
		return nil
	}
	profiles := transcode.ProfilesFor(w, h)
	if len(profiles) == 0 {
		return nil
	}
	return map[string]any{"profiles": profiles}
}

// authorizeObject 校验访问令牌：用户有效、能访问该媒体库、受限内容已解锁。返回用户 ID。
func (s *Server) authorizeObject(r *http.Request, libraryID int64) (int64, bool) {
	uid, gen, flags, ok := s.keys.ParseToken(r.Header.Get(api.TokenHeader), s.now())
	if !ok {
		return 0, false
	}
	a, err := s.access(r.Context(), uid)
	if err != nil || a.user.Disabled || gen != a.user.TokenGen || !a.allowed[libraryID] {
		return 0, false
	}
	if a.restricted[libraryID] && a.user.PinHash != "" && flags&mediacrypt.TokenUnlocked == 0 {
		return 0, false
	}
	return uid, true
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if s.tc == nil {
		blobError(w, http.StatusNotFound)
		return
	}
	sess := s.tc.Get(r.PathValue("sid"))
	if sess == nil {
		blobError(w, http.StatusNotFound)
		return
	}
	if uid, ok := s.authorizeObject(r, sess.LibraryID); !ok || uid != sess.UID {
		blobError(w, http.StatusForbidden)
		return
	}
	b, err := sess.Segment(r.Context(), r.PathValue("name"))
	if err != nil {
		switch {
		case errors.Is(err, transcode.ErrNotFound):
			blobError(w, http.StatusNotFound)
		case errors.Is(err, transcode.ErrSuperseded):
			blobError(w, http.StatusConflict)
		case r.Context().Err() != nil:
			// 客户端已经放弃（拖动到别处）
		default:
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "transcode failed", http.StatusBadGateway)
		}
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "no-store") // 转码会话是临时的，内容不缓存
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.Write(b)
}
