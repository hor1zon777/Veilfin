package e2e

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"veilfin/internal/api"
)

// 失败锁定、改密码、扫描相关的回归测试（第二轮代码审查）。

func loginRaw(t *testing.T, e *env, name, pass string) func(m string, p any) api.Response {
	t.Helper()
	send, _ := rawSession(t, e)
	if r := send("auth.login", map[string]string{"username": name, "password": pass}); !r.OK {
		t.Fatalf("登录失败: %s", r.Err)
	}
	return send
}

// 并发猜 PIN 不能越过 5 次的上限
func TestConcurrentPinAttempts(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin()
	admin.must("admin.users.create", map[string]any{"username": "bob", "password": "bob-password"}, nil)
	if r := loginRaw(t, e, "bob", "bob-password")("auth.setPin", map[string]any{"password": "bob-password", "pin": "1357"}); !r.OK {
		t.Fatal(r.Err)
	}

	const n = 12
	sends := make([]func(string, any) api.Response, n)
	for i := range sends {
		sends[i] = loginRaw(t, e, "bob", "bob-password")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[string]int{}
	for _, send := range sends {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := send("auth.unlock", map[string]string{"pin": "0000"})
			mu.Lock()
			codes[r.Code]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[api.CodeUnauthorized] != 5 || codes[api.CodeTooMany] != n-5 {
		t.Fatalf("并发猜 PIN 应只有 5 次真正被校验: %v", codes)
	}
	if r := loginRaw(t, e, "bob", "bob-password")("auth.unlock", map[string]string{"pin": "1357"}); r.OK || r.Code != api.CodeTooMany {
		t.Fatalf("锁定期内正确的 PIN 也应被拒绝: %+v", r)
	}
}

// 改密码、设置 PIN 时校验密码和登录共用失败计数，不能借它们无限次猜密码
func TestPasswordGuessViaSettings(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin()
	admin.must("admin.users.create", map[string]any{"username": "bob", "password": "bob-password"}, nil)
	send := loginRaw(t, e, "bob", "bob-password")

	// 新密码不合格时不消耗尝试次数
	for i := 0; i < 6; i++ {
		if r := send("auth.changePassword", map[string]string{"old": "guess", "new": "short"}); r.Code != api.CodeBadRequest {
			t.Fatalf("新密码太短应先报 bad_request: %+v", r)
		}
	}
	for i := 0; i < 3; i++ {
		if r := send("auth.changePassword", map[string]string{"old": "guess", "new": "new-password"}); r.Code != api.CodeUnauthorized {
			t.Fatalf("旧密码错误应报 unauthorized: %+v", r)
		}
	}
	for i := 0; i < 2; i++ {
		if r := send("auth.setPin", map[string]any{"password": "guess", "pin": "1234"}); r.Code != api.CodeUnauthorized {
			t.Fatalf("密码错误应报 unauthorized: %+v", r)
		}
	}
	if r := send("auth.setPin", map[string]any{"password": "bob-password", "pin": "1234"}); r.Code != api.CodeTooMany {
		t.Fatalf("错 5 次后即使密码正确也应被锁定: %+v", r)
	}
	other, _ := rawSession(t, e)
	if r := other("auth.login", map[string]string{"username": "bob", "password": "bob-password"}); r.Code != api.CodeTooMany {
		t.Fatalf("设置页里猜错密码也应锁定登录: %+v", r)
	}
}

// 不存在的用户名和真实账号的锁定表现一致，不能借此枚举用户名
func TestGhostUserLockout(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin()
	admin.must("admin.users.create", map[string]any{"username": "bob", "password": "bob-password"}, nil)
	results := map[string]api.Response{}
	for _, name := range []string{"bob", "nobody"} {
		for i := 0; i < 5; i++ {
			s, _ := rawSession(t, e)
			if r := s("auth.login", map[string]string{"username": name, "password": "wrong-pass"}); r.Code != api.CodeUnauthorized {
				t.Fatalf("%s 第 %d 次: %+v", name, i+1, r)
			}
		}
		s, _ := rawSession(t, e)
		results[name] = s("auth.login", map[string]string{"username": name, "password": "wrong-pass"})
	}
	b, g := results["bob"], results["nobody"]
	if b.Code != api.CodeTooMany || g.Code != b.Code || g.Err != b.Err {
		t.Fatalf("真实账号和不存在的用户名锁定后的响应应相同: %+v / %+v", b, g)
	}
}

// 改密码后其他设备上的会话退出登录，当前会话保持登录
func TestChangePasswordLogsOutOthers(t *testing.T) {
	e := newEnv(t)
	admin := e.setupAdmin()
	admin.must("admin.users.create", map[string]any{"username": "bob", "password": "bob-password"}, nil)
	a := loginRaw(t, e, "bob", "bob-password")
	b := loginRaw(t, e, "bob", "bob-password")
	if r := a("auth.changePassword", map[string]string{"old": "bob-password", "new": "new-password"}); !r.OK {
		t.Fatal(r.Err)
	}
	if r := a("auth.me", nil); !r.OK {
		t.Fatalf("改密码的会话应保持登录: %+v", r)
	}
	if r := b("auth.me", nil); r.Code != api.CodeUnauthorized {
		t.Fatalf("其他会话应被登出: %+v", r)
	}
}

// 本地客户端：改密码 / 设置 PIN 时输错密码，服务端什么都没变，不应清掉页面上的对象引用
func TestFailedSettingsKeepBlobs(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.mediaDir, "v")
	writeRandom(t, filepath.Join(dir, "a.mp4"), 100_000)
	admin := e.setupAdmin()
	var lib struct {
		ID int64 `json:"id"`
	}
	admin.must("admin.libs.create", map[string]any{"name": "v", "kind": "home", "paths": []string{dir}}, &lib)
	e.waitIdle()
	var all listResult
	admin.must("items.list", map[string]any{"libraryId": lib.ID}, &all)
	blob := blobIDOf(t, admin, all.Items[0].ID)
	fetch := func(why string) {
		t.Helper()
		if code, _, _ := admin.get("/b/"+blob, "bytes=0-99"); code != 206 {
			t.Fatalf("%s：视频应仍可读取，实际 HTTP %d", why, code)
		}
	}
	fetch("初始")
	admin.expectCode("auth.changePassword", map[string]string{"old": "guess", "new": "another-pass"}, api.CodeUnauthorized)
	fetch("改密码输错旧密码后")
	admin.expectCode("auth.setPin", map[string]any{"password": "guess", "pin": "1234"}, api.CodeUnauthorized)
	fetch("设置 PIN 输错密码后")
}

// 子目录变空（多半是没挂载上）时保留其下条目；整个子目录被删掉、或子目录里还有别的文件时才删除
func TestScanKeepsEmptySubdir(t *testing.T) {
	e := newEnv(t)
	root := filepath.Join(e.mediaDir, "lib")
	writeRandom(t, filepath.Join(root, "a", "1.mp4"), 100_000)
	writeRandom(t, filepath.Join(root, "b", "2.mp4"), 100_000)
	writeRandom(t, filepath.Join(root, "c", "3.mp4"), 100_000)
	os.WriteFile(filepath.Join(root, "b", "readme.txt"), []byte("x"), 0o644)
	admin := e.setupAdmin()
	var lib struct {
		ID int64 `json:"id"`
	}
	admin.must("admin.libs.create", map[string]any{"name": "v", "kind": "home", "paths": []string{root}}, &lib)
	e.waitIdle()
	rescan := func(want int, why string) {
		t.Helper()
		admin.must("admin.libs.scan", map[string]any{"id": lib.ID}, nil)
		e.waitIdle()
		var all listResult
		admin.must("items.list", map[string]any{"libraryId": lib.ID}, &all)
		if all.Total != want {
			t.Fatalf("%s：应有 %d 个条目，实际 %d", why, want, all.Total)
		}
	}
	rescan(3, "初次扫描")

	os.Remove(filepath.Join(root, "a", "1.mp4"))
	rescan(3, "子目录 a 变空（像是没挂载上）")

	os.RemoveAll(filepath.Join(root, "a"))
	rescan(2, "子目录 a 被删掉")

	os.Remove(filepath.Join(root, "b", "2.mp4"))
	rescan(1, "子目录 b 里只删了视频")
}
