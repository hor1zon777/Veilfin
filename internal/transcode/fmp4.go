package transcode

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
)

// 分片 MP4 的最小解析：只读顶层盒子，从 moov 里取各轨道的时间刻度，改写 moof 里的 tfdt。

const maxBox = 256 << 20 // 单个盒子的上限（一个 4 秒分片远小于它）

type box struct {
	typ  string
	data []byte // 完整的盒子（含头部）
}

// readBox 读取下一个顶层盒子。
func readBox(r *bufio.Reader) (*box, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	size := uint64(binary.BigEndian.Uint32(hdr[:4]))
	typ := string(hdr[4:8])
	head := hdr[:]
	if size == 1 {
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		size = binary.BigEndian.Uint64(ext[:])
		head = append(append([]byte(nil), hdr[:]...), ext[:]...)
	}
	if size == 0 || size < uint64(len(head)) || size > maxBox {
		return nil, errors.New("fMP4 盒子大小非法")
	}
	buf := make([]byte, size)
	copy(buf, head)
	if _, err := io.ReadFull(r, buf[len(head):]); err != nil {
		return nil, io.ErrUnexpectedEOF
	}
	return &box{typ: typ, data: buf}, nil
}

// children 遍历 b（去掉 off 字节的头部之后）中的子盒子。
func children(b []byte, fn func(typ string, body []byte, start int)) {
	for i := 0; i+8 <= len(b); {
		size := int(binary.BigEndian.Uint32(b[i:]))
		hl := 8
		if size == 1 {
			if i+16 > len(b) {
				return
			}
			size = int(binary.BigEndian.Uint64(b[i+8:]))
			hl = 16
		}
		if size < hl || i+size > len(b) {
			return
		}
		fn(string(b[i+4:i+8]), b[i+hl:i+size], i+hl)
		i += size
	}
}

// trackInfo 是 moov 中每个轨道的信息。
type trackInfo struct {
	timescale uint32
	video     bool
}

// parseMoov 返回 track_ID → 轨道信息。
func parseMoov(moov []byte) map[uint32]trackInfo {
	out := map[uint32]trackInfo{}
	children(moov[8:], func(typ string, trak []byte, _ int) {
		if typ != "trak" {
			return
		}
		var id uint32
		var ti trackInfo
		children(trak, func(typ string, body []byte, _ int) {
			switch typ {
			case "tkhd":
				if len(body) < 4 {
					return
				}
				if body[0] == 1 { // version 1：创建 / 修改时间是 64 位
					if len(body) >= 24 {
						id = binary.BigEndian.Uint32(body[20:])
					}
				} else if len(body) >= 16 {
					id = binary.BigEndian.Uint32(body[12:])
				}
			case "mdia":
				children(body, func(typ string, b []byte, _ int) {
					switch typ {
					case "mdhd":
						if len(b) >= 24 && b[0] == 1 {
							ti.timescale = binary.BigEndian.Uint32(b[20:])
						} else if len(b) >= 16 {
							ti.timescale = binary.BigEndian.Uint32(b[12:])
						}
					case "hdlr":
						if len(b) >= 12 && string(b[8:12]) == "vide" {
							ti.video = true
						}
					}
				})
			}
		})
		if id != 0 && ti.timescale != 0 {
			out[id] = ti
		}
	})
	return out
}

// patchMoof 把 moof 中每个轨道的 tfdt 加上 offset 秒（原地修改），返回视频轨道改写后的起始时间（秒）。
// 没有视频轨道时返回第一个轨道的时间；找不到 tfdt 时 ok 为 false。
func patchMoof(moof []byte, tracks map[uint32]trackInfo, offset float64) (start float64, ok bool) {
	found := false
	children(moof[8:], func(typ string, traf []byte, _ int) {
		if typ != "traf" {
			return
		}
		var ti trackInfo
		children(traf, func(typ string, body []byte, _ int) {
			switch typ {
			case "tfhd":
				if len(body) >= 8 {
					ti = tracks[binary.BigEndian.Uint32(body[4:])]
				}
			case "tfdt":
				if ti.timescale == 0 || len(body) < 8 {
					return
				}
				add := uint64(offset*float64(ti.timescale) + 0.5)
				var t uint64
				if body[0] == 1 && len(body) >= 12 {
					t = binary.BigEndian.Uint64(body[4:]) + add
					binary.BigEndian.PutUint64(body[4:], t)
				} else {
					t = uint64(binary.BigEndian.Uint32(body[4:])) + add
					if t > 0xFFFFFFFF { // 32 位放不下（极长的视频才会出现，MP4 复用器实际写的都是 64 位）
						return
					}
					binary.BigEndian.PutUint32(body[4:], uint32(t))
				}
				s := float64(t) / float64(ti.timescale)
				if ti.video || !found {
					start, ok = s, true
				}
				if ti.video {
					found = true
				}
			}
		})
	})
	return start, ok
}
