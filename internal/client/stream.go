package client

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"veilfin/internal/api"
	"veilfin/internal/mediacrypt"
)

// 播放时转码（HLS）：服务端下发 {"$s": 会话ID, "k": 密钥, "p": 播放列表}，
// 这里把它换成 /t/{会话ID}/index.m3u8；浏览器请求分片时从服务端的 /s/{会话ID}/{名称} 拉取、解密后返回。

const maxStreams = 64 // 每个浏览器记住的转码会话上限

type streamInfo struct {
	id       string
	key      []byte
	playlist string
	added    time.Time
}

func (c *Client) registerStream(b *browser, r *api.StreamRef) (string, error) {
	key, err := base64.RawURLEncoding.DecodeString(r.Key)
	if err != nil || len(key) != mediacrypt.KeySize {
		return "", errors.New("转码流密钥格式错误")
	}
	b.blobMu.Lock()
	if b.streams == nil {
		b.streams = map[string]*streamInfo{}
	}
	if len(b.streams) >= maxStreams { // 丢掉最早的
		var oldest *streamInfo
		for _, s := range b.streams {
			if oldest == nil || s.added.Before(oldest.added) {
				oldest = s
			}
		}
		delete(b.streams, oldest.id)
	}
	b.streams[r.ID] = &streamInfo{id: r.ID, key: key, playlist: r.Playlist, added: time.Now()}
	b.blobMu.Unlock()
	return "/t/" + r.ID + "/index.m3u8", nil
}

func (b *browser) stream(id string) *streamInfo {
	b.blobMu.Lock()
	defer b.blobMu.Unlock()
	return b.streams[id]
}

// validSegName 判断分片名是否合法："init" 或非负整数。
func validSegName(name string) bool {
	if name == "init" {
		return true
	}
	n, err := strconv.Atoi(name)
	return err == nil && n >= 0 && strconv.Itoa(n) == name
}

func (c *Client) handleStream(w http.ResponseWriter, r *http.Request) {
	b := c.lookupBrowser(r)
	if b == nil {
		http.Error(w, "未登录", http.StatusUnauthorized)
		return
	}
	st := b.stream(r.PathValue("sid"))
	if st == nil {
		http.Error(w, "未知转码会话，请刷新页面", http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	name := r.PathValue("name")
	if name == "index.m3u8" {
		h.Set("Content-Type", "application/vnd.apple.mpegurl")
		io.WriteString(w, st.playlist)
		return
	}
	if !validSegName(name) {
		http.Error(w, "分片名非法", http.StatusNotFound)
		return
	}
	plain, code, err := c.fetchSegment(r.Context(), b, st, name)
	if err != nil {
		if r.Context().Err() == nil {
			http.Error(w, err.Error(), code)
		}
		return
	}
	h.Set("Content-Type", "video/mp4")
	h.Set("Content-Length", strconv.Itoa(len(plain)))
	w.Write(plain)
}

// fetchSegment 拉取并解密一个分片（整段校验，任何篡改都会被发现）。
func (c *Client) fetchSegment(ctx context.Context, b *browser, st *streamInfo, name string) ([]byte, int, error) {
	force := false
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := b.tr.blobToken(ctx, force)
		if err != nil {
			return nil, http.StatusBadGateway, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/s/"+st.id+"/"+name, nil)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		req.Header.Set(api.TokenHeader, tok)
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, http.StatusBadGateway, err
		}
		switch resp.StatusCode {
		case http.StatusOK:
		case http.StatusForbidden:
			resp.Body.Close()
			force = true // 令牌失效或解锁状态变化，刷新后重试
			continue
		default:
			resp.Body.Close()
			code := resp.StatusCode
			if code < 400 {
				code = http.StatusBadGateway
			}
			return nil, code, fmt.Errorf("上游返回 HTTP %d", resp.StatusCode)
		}
		ct, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
		resp.Body.Close()
		if err != nil {
			return nil, http.StatusBadGateway, err
		}
		plain, err := openSegment(st.key, st.id, name, ct)
		if err != nil {
			return nil, http.StatusBadGateway, err
		}
		return plain, http.StatusOK, nil
	}
	return nil, http.StatusForbidden, errForbidden
}

// openSegment 解密一个分片。明文大小由密文大小推出（每块固定开销 28 字节）。
func openSegment(key []byte, sid, name string, ct []byte) ([]byte, error) {
	size, ok := PlainSizeOf(int64(len(ct)))
	if !ok {
		return nil, errors.New("分片长度不正确（数据被篡改或损坏）")
	}
	ci, err := mediacrypt.NewCipher(key, nil, sid+"/"+name, size)
	if err != nil {
		return nil, err
	}
	plain, err := mediacrypt.OpenAll(ci, ct)
	if err != nil {
		return nil, errors.New("分片校验失败（数据被篡改或损坏）")
	}
	return plain, nil
}

// PlainSizeOf 由密文大小推出明文大小。
func PlainSizeOf(cs int64) (int64, bool) {
	if cs <= 0 {
		return 0, cs == 0
	}
	n := (cs + mediacrypt.CipherChunkSize - 1) / mediacrypt.CipherChunkSize
	size := cs - n*mediacrypt.Overhead
	if size <= (n-1)*mediacrypt.ChunkSize {
		return 0, false
	}
	return size, true
}
