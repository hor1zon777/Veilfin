package e2e

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

	"veilfin/internal/server"
	"veilfin/internal/update"
)

type updateStatus struct {
	Current  string `json:"current"`
	Reason   string `json:"reason"`
	CheckErr string `json:"checkErr"`
	Latest   *struct {
		Tag   string `json:"tag"`
		Asset string `json:"asset"`
		Size  int64  `json:"size"`
		Newer bool   `json:"newer"`
	} `json:"latest"`
	Job *struct {
		Tag     string `json:"tag"`
		Phase   string `json:"phase"`
		Running bool   `json:"running"`
		Err     string `json:"err"`
	} `json:"job"`
}

// fakeReleases 模拟 GitHub 上的发布：v0.0.9 和 v0.2.0，附件内容可在测试中修改。
func fakeReleases(t *testing.T, files map[string][]byte, sums map[string][]byte) *httptest.Server {
	mux := http.NewServeMux()
	var srv *httptest.Server
	release := func(tag string) map[string]any {
		name := update.AssetName(runtime.GOOS, runtime.GOARCH, false)
		return map[string]any{
			"tag_name": tag, "name": "Veilfin " + tag, "body": "## 变更\n\n- 新功能", "published_at": time.Now().Format(time.RFC3339),
			"assets": []map[string]any{
				{"name": name, "size": len(files[tag]), "browser_download_url": srv.URL + "/dl/" + tag + "/" + name},
				{"name": update.SumsFile, "browser_download_url": srv.URL + "/dl/" + tag + "/" + update.SumsFile},
			},
		}
	}
	mux.HandleFunc("GET /repos/o/r/releases", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]any{release("v0.2.0"), release("v0.0.9")})
	})
	mux.HandleFunc("GET /repos/o/r/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(release(r.PathValue("tag")))
	})
	mux.HandleFunc("GET /dl/{tag}/{name}", func(w http.ResponseWriter, r *http.Request) {
		tag := r.PathValue("tag")
		if r.PathValue("name") == update.SumsFile {
			h := sha256.Sum256(sums[tag])
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(h[:]), update.AssetName(runtime.GOOS, runtime.GOARCH, false))
			return
		}
		w.Write(files[tag])
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func waitUpdateJob(t *testing.T, u *user) updateStatus {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var st updateStatus
		u.must("admin.update.status", nil, &st)
		if st.Job != nil && (!st.Job.Running || st.Job.Phase == "restart") {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待更新超时: %+v", st.Job)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 管理后台检查更新、下载校验失败时保持原样、成功时替换程序并请求重启。
func TestAdminUpdate(t *testing.T) {
	newBin := []byte("new-program-v0.2.0")
	files := map[string][]byte{"v0.2.0": []byte("tampered"), "v0.0.9": []byte("old")}
	sums := map[string][]byte{"v0.2.0": newBin, "v0.0.9": []byte("old")}
	gh := fakeReleases(t, files, sums)

	exe := filepath.Join(t.TempDir(), "veilfin.exe")
	os.WriteFile(exe, []byte("current-program"), 0o755)
	reexec := make(chan struct{}, 1)
	verified := make(chan string, 4)
	e := newEnvWith(t, server.Config{
		Version: "0.1.0 (abc1234)", Executable: exe, UpdateAPI: gh.URL, UpdateRepo: "o/r",
		Reexec: func() { reexec <- struct{}{} },
		VerifyUpdate: func(ctx context.Context, path string, want update.Version) error {
			b, _ := os.ReadFile(path)
			verified <- string(b)
			if !strings.HasSuffix(string(b), "v"+want.String()) {
				return fmt.Errorf("版本号不一致")
			}
			return nil
		},
	})
	admin := e.setupAdmin()
	admin.must("admin.users.create", map[string]any{"username": "bob", "password": "bob-password"}, nil)
	bob := e.newClient()
	bob.must("auth.login", map[string]string{"username": "bob", "password": "bob-password"}, nil)
	bob.expectCode("admin.update.status", nil, "forbidden")
	bob.expectCode("admin.update.check", nil, "forbidden")
	bob.expectCode("admin.update.apply", map[string]string{"tag": "v0.2.0"}, "forbidden")

	var st updateStatus
	admin.must("admin.update.status", nil, &st)
	if st.Current != "0.1.0 (abc1234)" || st.Reason != "" || st.Latest != nil {
		t.Fatalf("初始状态不正确: %+v", st)
	}
	admin.must("admin.update.check", map[string]bool{"prerelease": false}, &st)
	if st.CheckErr != "" || st.Latest == nil || st.Latest.Tag != "v0.2.0" || !st.Latest.Newer ||
		st.Latest.Asset != update.AssetName(runtime.GOOS, runtime.GOARCH, false) {
		t.Fatalf("检查结果不正确: %+v %+v", st, st.Latest)
	}
	admin.expectCode("admin.update.apply", map[string]string{"tag": "v0.0.9"}, "bad_request")
	admin.expectCode("admin.update.apply", map[string]string{"tag": "nonsense"}, "bad_request")

	// 附件被篡改：校验失败，程序保持不变，也没有重启
	admin.must("admin.update.apply", map[string]string{"tag": "v0.2.0"}, nil)
	st = waitUpdateJob(t, admin)
	if !strings.Contains(st.Job.Err, "校验失败") {
		t.Fatalf("应校验失败: %+v", st.Job)
	}
	if b, _ := os.ReadFile(exe); string(b) != "current-program" {
		t.Fatal("校验失败后程序不应被替换")
	}
	if _, err := os.Stat(update.NewPath(exe)); !os.IsNotExist(err) {
		t.Fatal("校验失败后应删除下载的文件")
	}
	// 更新失败后可以正常备份
	admin.must("admin.backup.create", map[string]any{"pass": "backup-pass-1"}, nil)
	waitBackupJob(t, admin)

	// 正常更新：替换程序、保留旧程序、请求重启
	files["v0.2.0"] = newBin
	admin.must("admin.update.apply", map[string]string{"tag": "v0.2.0"}, nil)
	st = waitUpdateJob(t, admin)
	if st.Job.Err != "" || st.Job.Phase != "restart" {
		t.Fatalf("更新失败: %+v", st.Job)
	}
	if v := <-verified; v != string(newBin) {
		t.Fatalf("试运行的不是下载的新程序: %q", v)
	}
	select {
	case <-reexec:
	case <-time.After(5 * time.Second):
		t.Fatal("没有请求重启")
	}
	if b, _ := os.ReadFile(exe); string(b) != string(newBin) {
		t.Fatalf("程序没有被替换: %q", b)
	}
	if b, _ := os.ReadFile(update.OldPath(exe)); string(b) != "current-program" {
		t.Fatalf("旧程序没有保留: %q", b)
	}
	// 重启前不再接受备份和再次更新
	admin.expectCode("admin.backup.create", map[string]any{"pass": "backup-pass-1"}, "bad_request")
	admin.expectCode("admin.update.apply", map[string]string{"tag": "v0.2.0"}, "bad_request")
}
