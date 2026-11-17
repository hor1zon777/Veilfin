package db

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestReserveAttempt(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	id, err := d.CreateUser(ctx, "u", "x", false)
	if err != nil {
		t.Fatal(err)
	}
	p := AttemptPolicy{Threshold: 5, Window: 900}
	reserve := func(now int64) bool {
		t.Helper()
		ok, err := d.ReserveLoginAttempt(ctx, id, p, now)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	user := func() *User {
		u, err := d.UserByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	// 并发占坑：最多只有 Threshold 次能通过
	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := d.ReserveLoginAttempt(ctx, id, p, 1000)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if passed != 5 {
		t.Fatalf("并发尝试应只通过 5 次，实际 %d", passed)
	}
	if u := user(); u.LockedUntil != 1900 {
		t.Fatalf("应锁定到 1900，实际 %d", u.LockedUntil)
	}
	// 锁定期内一律拒绝，也不延长锁定
	if reserve(1899) || user().LockedUntil != 1900 {
		t.Fatal("锁定期内不应通过，也不应延长锁定")
	}
	// 锁定到期后计数从头开始：错一次不会立刻再锁
	if !reserve(1900) || user().FailedLogins != 1 || user().LockedUntil > 1900 {
		t.Fatalf("锁定到期后应从 1 开始计数: %+v", user())
	}
	// 窗口内继续累计
	for i := 0; i < 3; i++ {
		reserve(1910)
	}
	if u := user(); u.FailedLogins != 4 || u.LockedUntil > 1910 {
		t.Fatalf("窗口内应累计到 4 次且未锁定: %+v", u)
	}
	// 隔了一个窗口才再错：旧的失败不再算数
	if !reserve(1910+900) || user().FailedLogins != 1 {
		t.Fatalf("超过窗口后应重新计数: %+v", user())
	}
	// 校验成功后清零
	if err := d.ResetLoginFailures(ctx, id); err != nil || user().FailedLogins != 0 {
		t.Fatalf("清零失败: %v %+v", err, user())
	}

	// PIN 用的是另一组字段，互不影响
	for i := 0; i < 5; i++ {
		if ok, _ := d.ReservePinAttempt(ctx, id, p, 5000); !ok {
			t.Fatal("PIN 前 5 次应通过")
		}
	}
	if ok, _ := d.ReservePinAttempt(ctx, id, p, 5001); ok {
		t.Fatal("PIN 第 6 次应被锁定")
	}
	if u := user(); u.PinLockedUntil != 5900 || u.FailedLogins != 0 {
		t.Fatalf("PIN 锁定不应影响登录计数: %+v", u)
	}
}
