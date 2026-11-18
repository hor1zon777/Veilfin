package tasks

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 运行中调整工作协程数：增加后并发数随之提高，减少后不超过新的上限，且不会中断正在运行的任务。
func TestSetWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := New(ctx, "test", 2)
	defer func() {
		cancel()
		q.Stopped()
	}()

	var cur atomic.Int32
	release := make(chan struct{})
	var mu sync.Mutex
	finished := 0
	job := func(ctx context.Context) error {
		cur.Add(1)
		<-release
		cur.Add(-1)
		mu.Lock()
		finished++
		mu.Unlock()
		return nil
	}
	waitRunning := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for cur.Load() != want {
			if time.Now().After(deadline) {
				t.Fatalf("正在运行的任务数 %d，期望 %d", cur.Load(), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	for i := 0; i < 20; i++ {
		q.Add(fmt.Sprint(i), fmt.Sprint(i), job)
	}
	waitRunning(2)
	q.SetWorkers(6)
	waitRunning(6)
	if q.Workers() != 6 {
		t.Fatal(q.Workers())
	}

	// 降到 3：正在运行的 6 个任务继续完成，之后最多同时运行 3 个
	q.SetWorkers(3)
	for i := 0; i < 6; i++ {
		release <- struct{}{}
	}
	waitRunning(3)
	time.Sleep(50 * time.Millisecond)
	if n := cur.Load(); n != 3 {
		t.Fatalf("减少后同时运行 %d 个", n)
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		f := finished
		mu.Unlock()
		if f == 20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("只完成了 %d 个任务", f)
		}
		time.Sleep(5 * time.Millisecond)
	}
	q.SetWorkers(0) // 至少保留 1 个
	if q.Workers() != 1 {
		t.Fatal(q.Workers())
	}
}

// AddFront：新任务插到队首；已排队的移到队首且不重复；正在运行的不再加入。
func TestAddFront(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := New(ctx, "test", 1)
	defer func() {
		cancel()
		q.Stopped()
	}()
	var mu sync.Mutex
	var order []string
	started := make(chan struct{})
	release := make(chan struct{})
	job := func(name string) func(context.Context) error {
		return func(context.Context) error {
			if name == "a" {
				close(started)
				<-release
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		}
	}
	q.Add("a", "a", job("a"))
	<-started // a 正在运行
	q.Add("b", "b", job("b"))
	q.Add("c", "c", job("c"))
	q.Add("d", "d", job("d"))
	q.AddFront("c", "c", job("c2")) // 已在排队：移到队首，保留原来的函数
	q.AddFront("e", "e", job("e"))  // 新任务：插到队首
	q.AddFront("a", "a", job("a2")) // 正在运行：忽略
	if st := q.Status(); st.Pending != 4 {
		t.Fatalf("排队数 %d，期望 4", st.Pending)
	}
	close(release)
	if err := q.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(order); got != "[a e c b d]" {
		t.Fatalf("执行顺序 %s", got)
	}
}
