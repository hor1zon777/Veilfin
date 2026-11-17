// Package update 实现在线更新：从 GitHub Release 查询新版本，下载与当前平台对应的程序，
// 用发布附带的 SHA256SUMS.txt 校验，试运行确认版本号后替换正在运行的程序文件。
//
// 替换后需要重新启动进程才会生效（见 Reexec）。旧程序保留为 “<名称>.old[.exe]”，更新出问题时可以手动换回。
package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultRepo 是默认的发布仓库（owner/name）。
const DefaultRepo = "hor1zon777/Veilfin"

// DefaultAPI 是 GitHub API 地址。
const DefaultAPI = "https://api.github.com"

// SumsFile 是发布中的校验和文件名（release.yml 生成）。
const SumsFile = "SHA256SUMS.txt"

// maxBinary 是下载程序的大小上限，防止异常响应把磁盘写满。
const maxBinary = 512 << 20

type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
}

type Release struct {
	Tag        string    `json:"tag_name"`
	Name       string    `json:"name"`
	Body       string    `json:"body"`
	HTMLURL    string    `json:"html_url"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published_at"`
	Assets     []Asset   `json:"assets"`
}

// Asset 按文件名查找附件。
func (r *Release) Asset(name string) *Asset {
	for i := range r.Assets {
		if r.Assets[i].Name == name {
			return &r.Assets[i]
		}
	}
	return nil
}

// Client 访问 GitHub Release。
type Client struct {
	HTTP      *http.Client
	API       string // 默认 DefaultAPI
	Repo      string // 默认 DefaultRepo
	UserAgent string
}

// NewHTTPClient 创建下载用的 HTTP 客户端。proxy 为空时使用系统代理环境变量（HTTPS_PROXY 等）。
// 不设整体超时（下载大文件可能需要较长时间），由调用方通过 context 控制。
func NewHTTPClient(proxy string) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("代理地址无效: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr}, nil
}

func (c *Client) api() string {
	if c.API != "" {
		return strings.TrimRight(c.API, "/")
	}
	return DefaultAPI
}

func (c *Client) repo() string {
	if c.Repo != "" {
		return c.Repo
	}
	return DefaultRepo
}

func (c *Client) get(ctx context.Context, u string, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "Veilfin-Updater"
	}
	req.Header.Set("User-Agent", ua)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		msg := strings.TrimSpace(string(b))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		if resp.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(msg), "rate limit") {
			return nil, errors.New("GitHub API 访问次数超出限制，请稍后再试")
		}
		return nil, fmt.Errorf("HTTP %d：%s", resp.StatusCode, msg)
	}
	return resp, nil
}

// Latest 返回版本号最高的已发布版本；pre 为真时包含预发布版本。没有可用版本时返回 nil。
func (c *Client) Latest(ctx context.Context, pre bool) (*Release, error) {
	resp, err := c.get(ctx, c.api()+"/repos/"+c.repo()+"/releases?per_page=30", "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list []*Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&list); err != nil {
		return nil, fmt.Errorf("解析发布列表失败: %w", err)
	}
	var best *Release
	var bestV Version
	for _, r := range list {
		if r.Draft || (r.Prerelease && !pre) {
			continue
		}
		v, ok := ParseVersion(r.Tag)
		if !ok || (!pre && v.Pre != "") {
			continue
		}
		if best == nil || Compare(v, bestV) > 0 {
			best, bestV = r, v
		}
	}
	return best, nil
}

// ByTag 按标签查询发布。
func (c *Client) ByTag(ctx context.Context, tag string) (*Release, error) {
	resp, err := c.get(ctx, c.api()+"/repos/"+c.repo()+"/releases/tags/"+url.PathEscape(tag), "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("解析发布信息失败: %w", err)
	}
	return &r, nil
}

// ---- 版本号 ----

// Version 是语义化版本号（主.次.修订[-预发布]）。
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// ParseVersion 解析 "v1.2.3"、"1.2.3-rc.1"、"1.2.3 (abc1234)" 这类版本号；
// 开发版（"dev"）和按日期命名的本地构建等无法解析时返回 false。
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v Version
	core := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		core, v.Pre = s[:i], s[i+1:]
		if v.Pre == "" {
			return Version{}, false
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return Version{}, false
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, true
}

// Compare 按语义化版本规则比较，返回 -1、0、1。
func Compare(a, b Version) int {
	for _, d := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	// 没有预发布标识的版本更高
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	}
	ap, bp := strings.Split(a.Pre, "."), strings.Split(b.Pre, ".")
	for i := 0; i < len(ap) && i < len(bp); i++ {
		an, aerr := strconv.Atoi(ap[i])
		bn, berr := strconv.Atoi(bp[i])
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
		case aerr == nil: // 数字标识比字母标识低
			return -1
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(ap[i], bp[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(ap) < len(bp):
		return -1
	case len(ap) > len(bp):
		return 1
	}
	return 0
}

// ---- 平台与文件 ----

// AssetName 返回发布中对应平台的程序文件名（与 release.yml 一致）。
// full 为真表示内置 ffmpeg 的服务端版本。
func AssetName(goos, goarch string, full bool) string {
	name := "veilfin-" + goos + "-" + goarch
	if full {
		name = "veilfin-server-full-" + goos + "-" + goarch
	}
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// PickAsset 为当前平台选择附件：内置 ffmpeg 的程序优先选同样内置 ffmpeg 的版本，没有时退回普通版本。
func PickAsset(r *Release, goos, goarch string, full bool) *Asset {
	if full {
		if a := r.Asset(AssetName(goos, goarch, true)); a != nil {
			return a
		}
	}
	return r.Asset(AssetName(goos, goarch, false))
}

// Executable 返回当前程序文件的真实路径（解析符号链接）。
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return filepath.Abs(exe)
}

// InContainer 粗略判断是否运行在 Docker / Podman 容器中：容器里替换的程序在重建容器后就会丢失。
func InContainer() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// CheckWritable 检查能否在程序所在目录创建文件（替换程序需要）。
func CheckWritable(exe string) error {
	f, err := os.CreateTemp(filepath.Dir(exe), ".veilfin-wtest-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// sidePath 返回与程序同目录、在扩展名前插入 tag 的路径，例如 veilfin.exe → veilfin.new.exe。
// Windows 上保留 .exe 扩展名，新旧程序都可以直接运行。
func sidePath(exe, tag string) string {
	ext := filepath.Ext(exe)
	if !strings.EqualFold(ext, ".exe") {
		ext = ""
	}
	return strings.TrimSuffix(exe, ext) + "." + tag + ext
}

// NewPath 是下载新程序的临时路径。
func NewPath(exe string) string { return sidePath(exe, "new") }

// OldPath 是替换后旧程序的保存路径。
func OldPath(exe string) string { return sidePath(exe, "old") }

// ---- 下载与校验 ----

// fetchSum 从发布的 SHA256SUMS.txt 中找到指定文件的校验和。
func (c *Client) fetchSum(ctx context.Context, r *Release, name string) ([]byte, error) {
	sa := r.Asset(SumsFile)
	if sa == nil {
		return nil, fmt.Errorf("发布 %s 中没有 %s，无法校验，已停止更新", r.Tag, SumsFile)
	}
	resp, err := c.get(ctx, sa.URL, "application/octet-stream")
	if err != nil {
		return nil, fmt.Errorf("下载 %s 失败: %w", SumsFile, err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 || strings.TrimPrefix(f[1], "*") != name {
			continue
		}
		sum, err := hex.DecodeString(f[0])
		if err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("%s 中 %s 的校验和格式不正确", SumsFile, name)
		}
		return sum, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%s 中没有 %s 的校验和", SumsFile, name)
}

// Download 下载附件到 dst 并校验 SHA-256，成功后设置为可执行。progress 可为 nil。
// 失败时删除 dst。
func (c *Client) Download(ctx context.Context, r *Release, a *Asset, dst string, progress func(done, total int64)) (err error) {
	want, err := c.fetchSum(ctx, r, a.Name)
	if err != nil {
		return err
	}
	resp, err := c.get(ctx, a.URL, "application/octet-stream")
	if err != nil {
		return fmt.Errorf("下载 %s 失败: %w", a.Name, err)
	}
	defer resp.Body.Close()
	total := a.Size
	if total <= 0 {
		total = resp.ContentLength
	}
	os.Remove(dst)
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()
	h := sha256.New()
	pw := &progressWriter{total: total, fn: progress}
	n, err := io.Copy(io.MultiWriter(f, h, pw), io.LimitReader(resp.Body, maxBinary+1))
	if err != nil {
		return fmt.Errorf("下载 %s 失败: %w", a.Name, err)
	}
	if n > maxBinary {
		return errors.New("下载的文件过大，已停止更新")
	}
	if got := h.Sum(nil); !bytes.Equal(got, want) {
		return fmt.Errorf("%s 校验失败（SHA-256 不一致），文件可能损坏或被篡改，已停止更新", a.Name)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755)
}

type progressWriter struct {
	done, total int64
	fn          func(done, total int64)
	last        time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.fn != nil && (time.Since(p.last) > 200*time.Millisecond || p.done == p.total) {
		p.last = time.Now()
		p.fn(p.done, p.total)
	}
	return len(b), nil
}

// Verify 试运行新程序（"<程序> version"），确认它能在本机运行并且版本号与发布一致。
func Verify(ctx context.Context, path string, want Version) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("新程序无法在本机运行（%s/%s）: %w", runtime.GOOS, runtime.GOARCH, err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	got, ok := ParseVersion(line)
	if !ok || Compare(got, want) != 0 {
		return fmt.Errorf("新程序报告的版本号是 %q，与发布的 %s 不一致，已停止更新", strings.TrimSpace(line), want)
	}
	return nil
}

// Install 用 newPath 替换 exe，旧程序保存为 OldPath(exe)。失败时尽量恢复原状。
//
// Windows 不允许覆盖正在运行的程序，但允许改名，所以先把旧程序改名再放入新程序；
// 其他系统直接原子替换（正在运行的进程不受影响），旧程序通过硬链接（不支持时复制）保留一份。
func Install(exe, newPath string) error {
	old := OldPath(exe)
	if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("无法删除上次保留的旧程序 %s: %w", old, err)
	}
	if st, err := os.Stat(exe); err == nil {
		os.Chmod(newPath, st.Mode().Perm()|0o100)
	}
	if runtime.GOOS == "windows" {
		if err := os.Rename(exe, old); err != nil {
			return fmt.Errorf("无法移走正在运行的程序: %w", err)
		}
		if err := os.Rename(newPath, exe); err != nil {
			if rerr := os.Rename(old, exe); rerr != nil {
				return fmt.Errorf("替换程序失败（%v），并且没能恢复旧程序（%v），请手动把 %s 改回 %s", err, rerr, old, exe)
			}
			return fmt.Errorf("替换程序失败: %w", err)
		}
		return nil
	}
	if err := os.Link(exe, old); err != nil {
		if err := copyFile(exe, old); err != nil {
			return fmt.Errorf("无法保留旧程序: %w", err)
		}
	}
	if err := os.Rename(newPath, exe); err != nil {
		os.Remove(old)
		return fmt.Errorf("替换程序失败: %w", err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
