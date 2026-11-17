package e2e

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"veilfin/internal/backup"
	"veilfin/internal/proto"
	"veilfin/internal/server"
)

type backupList struct {
	Dir   string `json:"dir"`
	Files []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"files"`
	Job *struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Running bool   `json:"running"`
		Err     string `json:"err"`
	} `json:"job"`
	Staged *struct {
		Name        string `json:"name"`
		PublicKey   string `json:"publicKey"`
		Fingerprint string `json:"fingerprint"`
		KeyChanged  bool   `json:"keyChanged"`
		HasAssets   bool   `json:"hasAssets"`
	} `json:"staged"`
	CanRestore bool `json:"canRestore"`
}

// waitBackupJob 轮询直到后台任务结束。
func waitBackupJob(t *testing.T, u *user) backupList {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		var l backupList
		u.must("admin.backup.list", nil, &l)
		if l.Job == nil || !l.Job.Running {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatal("等待备份任务超时")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 管理后台的备份、下载、上传和在线恢复。
func TestAdminBackupRestore(t *testing.T) {
	restarted := make(chan struct{}, 1)
	e := newEnvWith(t, server.Config{Restart: func() { restarted <- struct{}{} }})
	admin := e.setupAdmin()
	admin.must("admin.users.create", map[string]any{"username": "bob", "password": "bob-password"}, nil)
	os.MkdirAll(filepath.Join(e.dataDir, "assets", "ab"), 0o755)
	os.WriteFile(filepath.Join(e.dataDir, "assets", "ab", "x.jpg"), []byte("asset-data"), 0o644)

	// 公钥可以通过 sys.info 获取（用于复制到客户端）
	var info struct {
		PublicKey   string `json:"publicKey"`
		Fingerprint string `json:"fingerprint"`
	}
	admin.must("sys.info", nil, &info)
	if info.PublicKey != base64.StdEncoding.EncodeToString(e.srv.PublicKey()) || info.Fingerprint != proto.Fingerprint(e.srv.PublicKey()) {
		t.Fatal("sys.info 返回的公钥不正确")
	}

	// 普通用户不能使用备份接口
	bob := e.newClient()
	bob.must("auth.login", map[string]string{"username": "bob", "password": "bob-password"}, nil)
	bob.expectCode("admin.backup.list", nil, "forbidden")

	// 创建备份
	admin.expectCode("admin.backup.create", map[string]any{"pass": "short"}, "bad_request")
	var created struct {
		Name string `json:"name"`
	}
	admin.must("admin.backup.create", map[string]any{"pass": "backup-pass-1", "assets": true}, &created)
	l := waitBackupJob(t, admin)
	if l.Job.Err != "" || len(l.Files) != 1 || l.Files[0].Name != created.Name || !l.CanRestore {
		t.Fatalf("备份结果不正确: %+v", l)
	}
	onDisk, err := os.ReadFile(filepath.Join(e.dataDir, "backups", created.Name))
	if err != nil || !backup.IsBackupFile(filepath.Join(e.dataDir, "backups", created.Name)) {
		t.Fatal("备份文件没有写到 backups 目录")
	}

	// 分块下载，内容与磁盘上的一致
	var got []byte
	for {
		var r struct {
			Data string `json:"data"`
			Size int64  `json:"size"`
			EOF  bool   `json:"eof"`
		}
		admin.must("admin.backup.read", map[string]any{"name": created.Name, "offset": len(got)}, &r)
		b, _ := base64.StdEncoding.DecodeString(r.Data)
		got = append(got, b...)
		if r.EOF {
			break
		}
	}
	if !bytes.Equal(got, onDisk) {
		t.Fatal("下载的备份与原文件不一致")
	}
	if e.cdn.contains([]byte("VFBK1")) {
		t.Fatal("CDN 看到了备份文件的明文文件头")
	}
	// 路径穿越
	for _, bad := range []string{"../keys/identity.key", "..\\x.vfbak", "/etc/passwd", ".hidden.vfbak", "a/b.vfbak"} {
		admin.expectCode("admin.backup.read", map[string]any{"name": bad}, "bad_request")
		admin.expectCode("admin.backup.delete", map[string]any{"name": bad}, "bad_request")
	}

	// 分块上传（文件名会被整理成安全的名字）
	half := len(onDisk) / 2
	var up struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	admin.must("admin.backup.upload", map[string]any{"offset": 0, "data": base64.StdEncoding.EncodeToString(onDisk[:half])}, &up)
	admin.expectCode("admin.backup.upload", map[string]any{"id": up.ID, "offset": 3, "data": "AAAA"}, "bad_request") // 位置不连续
	admin.must("admin.backup.upload", map[string]any{"offset": 0, "data": base64.StdEncoding.EncodeToString(onDisk[:half])}, &up)
	admin.must("admin.backup.upload", map[string]any{"id": up.ID, "offset": half, "data": base64.StdEncoding.EncodeToString(onDisk[half:]),
		"done": true, "filename": `C:\tmp\../我的 备份.vfbak`}, &up)
	if up.Name != "我的_备份.vfbak" {
		t.Fatalf("上传后的文件名不正确: %q", up.Name)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dataDir, "backups", up.Name)); !bytes.Equal(b, onDisk) {
		t.Fatal("上传的备份内容不一致")
	}
	var up2 struct {
		ID string `json:"id"`
	}
	admin.must("admin.backup.upload", map[string]any{"offset": 0, "data": base64.StdEncoding.EncodeToString([]byte("not a backup"))}, &up2)
	admin.expectCode("admin.backup.upload", map[string]any{"id": up2.ID, "offset": 12, "data": "", "done": true, "filename": "x.vfbak"}, "bad_request")
	admin.must("admin.backup.delete", map[string]any{"name": created.Name}, nil)
	if l := waitBackupJob(t, admin); len(l.Files) != 1 || l.Files[0].Name != up.Name {
		t.Fatalf("删除后列表不正确: %+v", l.Files)
	}

	// 备份之后的改动，恢复后应该消失
	admin.must("admin.users.create", map[string]any{"username": "carol", "password": "carol-password"}, nil)

	// 错误口令：校验失败，不留下暂存
	admin.must("admin.backup.restore", map[string]any{"name": up.Name, "pass": "wrong-pass"}, nil)
	if l := waitBackupJob(t, admin); l.Job.Err == "" || l.Staged != nil {
		t.Fatalf("错误口令应该校验失败: %+v", l)
	}
	admin.expectCode("admin.backup.restore.apply", nil, "bad_request")

	// 正确口令：解包到暂存目录，等待确认
	admin.must("admin.backup.restore", map[string]any{"name": up.Name, "pass": "backup-pass-1"}, nil)
	l = waitBackupJob(t, admin)
	if l.Job.Err != "" || l.Staged == nil || l.Staged.KeyChanged || !l.Staged.HasAssets ||
		l.Staged.Fingerprint != proto.Fingerprint(e.srv.PublicKey()) {
		t.Fatalf("暂存结果不正确: %+v %+v", l.Job, l.Staged)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, backup.StagedDir, "keys", "identity.key")); err != nil {
		t.Fatal("没有解包到暂存目录")
	}
	// 取消后再来一次
	admin.must("admin.backup.restore.cancel", nil, nil)
	if _, err := os.Stat(filepath.Join(e.dataDir, backup.StagedDir)); err == nil {
		t.Fatal("取消后暂存目录应被删除")
	}
	admin.must("admin.backup.restore", map[string]any{"name": up.Name, "pass": "backup-pass-1"}, nil)
	waitBackupJob(t, admin)
	admin.must("admin.backup.restore.apply", nil, nil)
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("确认恢复后没有请求重启")
	}
	admin.expectCode("admin.backup.create", map[string]any{"pass": "backup-pass-1"}, "bad_request") // 重启中
	if _, err := os.Stat(filepath.Join(e.dataDir, backup.PendingDir)); err != nil {
		t.Fatal("确认后应改为待恢复目录")
	}

	// 模拟外层重启：关闭服务端 → 应用恢复 → 重新启动
	e.srv.Close()
	os.WriteFile(filepath.Join(e.dataDir, "assets", "ab", "x.jpg"), []byte("changed"), 0o644)
	applied, err := backup.ApplyPending(e.dataDir)
	if err != nil || !applied {
		t.Fatalf("应用恢复失败: %v %v", applied, err)
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, backup.PendingDir)); err == nil {
		t.Fatal("应用后待恢复目录应被删除")
	}
	if b, _ := os.ReadFile(filepath.Join(e.dataDir, "assets", "ab", "x.jpg")); string(b) != "asset-data" {
		t.Fatal("资源文件没有恢复")
	}
	srv2, err := server.New(e.ctx, server.Config{DataDir: e.dataDir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv2.Close() })
	e.srv = srv2
	e.cdn.next = srv2.Handler()

	// 同一个客户端（公钥未变）重新握手、重新登录；carol 已不存在
	admin.must("auth.login", map[string]string{"username": "admin", "password": adminPass}, nil)
	var users []struct {
		Username string `json:"username"`
	}
	admin.must("admin.users.list", nil, &users)
	names := map[string]bool{}
	for _, u := range users {
		names[u.Username] = true
	}
	if !names["admin"] || !names["bob"] || names["carol"] {
		t.Fatalf("恢复后的用户不正确: %v", names)
	}
}
