package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 测试程序本身充当“下载的新程序”：带上这个环境变量运行 "<程序> version" 时打印指定的版本号。
const fakeVersionEnv = "VF_UPDATE_TEST_FAKE_VERSION"

func TestMain(m *testing.M) {
	if v := os.Getenv(fakeVersionEnv); v != "" && len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(v)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestParseCompare(t *testing.T) {
	for _, c := range []struct {
		s  string
		ok bool
		v  string
	}{
		{"v0.1.6", true, "0.1.6"},
		{"0.1.6 (abc1234)", true, "0.1.6"},
		{"1.2.3-rc.1 (abc1234, 有未提交的修改)", true, "1.2.3-rc.1"},
		{"v1.0.0+build.5", true, "1.0.0"},
		{"dev (abc1234)", false, ""},
		{"20261117", false, ""},
		{"1.2", false, ""},
		{"1.2.x", false, ""},
		{"1.2.3-", false, ""},
	} {
		v, ok := ParseVersion(c.s)
		if ok != c.ok || (ok && v.String() != c.v) {
			t.Errorf("ParseVersion(%q) = %v, %v", c.s, v, ok)
		}
	}
	order := []string{"0.1.0-alpha", "0.1.0-alpha.1", "0.1.0-alpha.beta", "0.1.0-beta.2", "0.1.0-beta.11", "0.1.0-rc.1", "0.1.0", "0.1.1", "0.2.0", "1.0.0"}
	for i := range order {
		for j := range order {
			a, _ := ParseVersion(order[i])
			b, _ := ParseVersion(order[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := Compare(a, b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
}

func TestAssetName(t *testing.T) {
	if n := AssetName("windows", "amd64", true); n != "veilfin-server-full-windows-amd64.exe" {
		t.Error(n)
	}
	if n := AssetName("linux", "arm", false); n != "veilfin-linux-arm" {
		t.Error(n)
	}
	r := &Release{Assets: []Asset{{Name: "veilfin-darwin-arm64"}}}
	if a := PickAsset(r, "darwin", "arm64", true); a == nil || a.Name != "veilfin-darwin-arm64" {
		t.Error("内置 ffmpeg 的版本不存在时应退回普通版本", a)
	}
	if NewPath(`C:\a\veilfin.exe`) != `C:\a\veilfin.new.exe` || OldPath("/usr/bin/veilfin") != "/usr/bin/veilfin.old" {
		t.Error(NewPath(`C:\a\veilfin.exe`), OldPath("/usr/bin/veilfin"))
	}
}

// fakeGitHub 模拟 GitHub API 和附件下载。
type fakeGitHub struct {
	srv      *httptest.Server
	releases []*Release
	files    map[string][]byte
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{files: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/releases", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(g.releases)
	})
	mux.HandleFunc("GET /repos/o/r/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		for _, rel := range g.releases {
			if rel.Tag == r.PathValue("tag") {
				json.NewEncoder(w).Encode(rel)
				return
			}
		}
		http.Error(w, `{"message":"Not Found"}`, 404)
	})
	mux.HandleFunc("GET /dl/{name}", func(w http.ResponseWriter, r *http.Request) {
		b, ok := g.files[r.PathValue("name")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// add 发布一个版本，files 为附件（自动生成 SHA256SUMS.txt）。
func (g *fakeGitHub) add(tag string, pre bool, files map[string][]byte) *Release {
	r := &Release{Tag: tag, Prerelease: pre, Published: time.Now()}
	var sums strings.Builder
	for name, b := range files {
		key := tag + "-" + name
		g.files[key] = b
		r.Assets = append(r.Assets, Asset{Name: name, Size: int64(len(b)), URL: g.srv.URL + "/dl/" + key})
		h := sha256.Sum256(b)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h[:]), name)
	}
	key := tag + "-" + SumsFile
	g.files[key] = []byte(sums.String())
	r.Assets = append(r.Assets, Asset{Name: SumsFile, URL: g.srv.URL + "/dl/" + key})
	g.releases = append(g.releases, r)
	return r
}

func TestLatestDownloadInstall(t *testing.T) {
	g := newFakeGitHub(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	name := AssetName(runtime.GOOS, runtime.GOARCH, false)
	g.add("v0.1.0", false, map[string][]byte{name: []byte("old")})
	g.add("v0.3.0-rc.1", true, map[string][]byte{name: []byte("rc")})
	rel := g.add("v0.2.0", false, map[string][]byte{name: bin})
	g.releases = append(g.releases, &Release{Tag: "v9.0.0", Draft: true})

	c := &Client{API: g.srv.URL, Repo: "o/r"}
	ctx := context.Background()
	if r, err := c.Latest(ctx, false); err != nil || r.Tag != "v0.2.0" {
		t.Fatalf("Latest(false) = %v, %v", r, err)
	}
	if r, err := c.Latest(ctx, true); err != nil || r.Tag != "v0.3.0-rc.1" {
		t.Fatalf("Latest(true) = %v, %v", r, err)
	}
	if r, err := c.ByTag(ctx, "v0.2.0"); err != nil || r.Tag != "v0.2.0" {
		t.Fatalf("ByTag = %v, %v", r, err)
	}
	if _, err := c.ByTag(ctx, "v8.8.8"); err == nil || !strings.Contains(err.Error(), "Not Found") {
		t.Fatalf("不存在的标签: %v", err)
	}

	dir := t.TempDir()
	exe := filepath.Join(dir, filepath.Base(AssetName(runtime.GOOS, runtime.GOARCH, false)))
	os.WriteFile(exe, []byte("current"), 0o755)
	tmp := NewPath(exe)
	var last int64
	if err := c.Download(ctx, rel, PickAsset(rel, runtime.GOOS, runtime.GOARCH, false), tmp, func(done, total int64) { last = done }); err != nil {
		t.Fatal(err)
	}
	if last != int64(len(bin)) {
		t.Errorf("进度 %d / %d", last, len(bin))
	}
	want, _ := ParseVersion("v0.2.0")
	t.Setenv(fakeVersionEnv, "0.2.0 (abc1234)")
	if err := Verify(ctx, tmp, want); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeVersionEnv, "0.1.9 (abc1234)")
	if err := Verify(ctx, tmp, want); err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("版本号不一致应失败: %v", err)
	}
	if err := Install(exe, tmp); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); len(b) != len(bin) {
		t.Error("程序没有被替换")
	}
	if b, _ := os.ReadFile(OldPath(exe)); string(b) != "current" {
		t.Errorf("旧程序没有保留: %q", b)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("临时文件应已改名", err)
	}
	// 再更新一次：上次保留的旧程序被覆盖
	os.WriteFile(tmp, []byte("third"), 0o755)
	if err := Install(exe, tmp); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "third" {
		t.Errorf("第二次替换失败: %q", b)
	}
	if b, _ := os.ReadFile(OldPath(exe)); len(b) != len(bin) {
		t.Error("第二次替换后旧程序应是上一个版本")
	}
}

func TestDownloadRejectsBadChecksum(t *testing.T) {
	g := newFakeGitHub(t)
	name := AssetName(runtime.GOOS, runtime.GOARCH, false)
	rel := g.add("v1.0.0", false, map[string][]byte{name: []byte("good")})
	g.files["v1.0.0-"+name] = []byte("evil") // 附件被篡改
	c := &Client{API: g.srv.URL, Repo: "o/r"}
	dst := filepath.Join(t.TempDir(), "new")
	err := c.Download(context.Background(), rel, rel.Asset(name), dst, nil)
	if err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("校验和不一致应失败: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("校验失败后应删除下载的文件")
	}

	// 没有 SHA256SUMS.txt 的发布拒绝更新
	rel2 := &Release{Tag: "v1.0.1", Assets: []Asset{{Name: name, URL: g.srv.URL + "/dl/v1.0.0-" + name}}}
	if err := c.Download(context.Background(), rel2, &rel2.Assets[0], dst, nil); err == nil || !strings.Contains(err.Error(), SumsFile) {
		t.Fatalf("缺少校验和文件应失败: %v", err)
	}
}
