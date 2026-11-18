// Package tasks 是一个简单的后台任务队列：按 key 去重、有并发上限、可查询状态。
package tasks

import (
	"context"
	"log"
	"sync"
	"time"
)

type task struct {
	key   string
	label string
	fn    func(ctx context.Context) error
}

// Queue 是一个有并发上限的任务队列，worker 数可以在运行中调整（SetWorkers）。
type Queue struct {
	name string
	ctx  context.Context

	mu      sync.Mutex
	workers int // 当前的工作协程数
	target  int // 期望的工作协程数：多出来的协程做完手头的任务后退出
	cond    *sync.Cond
	pending []*task
	queued  map[string]bool
	running map[string]string // key → label
	done    int
	failed  int
	errors  []ErrorEntry
	closed  bool
	wg      sync.WaitGroup
}

type ErrorEntry struct {
	Label string `json:"label"`
	Err   string `json:"err"`
	Time  int64  `json:"time"`
}

type Status struct {
	Name    string       `json:"name"`
	Pending int          `json:"pending"`
	Running []string     `json:"running"`
	Done    int          `json:"done"`
	Failed  int          `json:"failed"`
	Errors  []ErrorEntry `json:"errors"`
}

// New 创建队列并启动 workers 个工作协程。
func New(ctx context.Context, name string, workers int) *Queue {
	q := &Queue{name: name, ctx: ctx, queued: map[string]bool{}, running: map[string]string{}}
	q.cond = sync.NewCond(&q.mu)
	q.SetWorkers(workers)
	go func() {
		<-ctx.Done()
		q.mu.Lock()
		q.closed = true
		q.cond.Broadcast()
		q.mu.Unlock()
	}()
	return q
}

// SetWorkers 调整工作协程数（至少 1 个）。增加时立即启动新的协程；
// 减少时多出来的协程会在做完当前任务后退出，不会中断正在进行的任务。
func (q *Queue) SetWorkers(n int) {
	if n < 1 {
		n = 1
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.target = n
	for q.workers < n {
		q.workers++
		q.wg.Add(1)
		go q.worker(q.ctx)
	}
	q.cond.Broadcast() // 让空闲的多余协程醒来退出
}

// Workers 返回期望的工作协程数。
func (q *Queue) Workers() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.target
}

// Stopped 等待所有工作协程退出（创建队列时传入的 ctx 结束后才会退出）。
func (q *Queue) Stopped() { q.wg.Wait() }

// Add 加入任务。同一个 key 已在排队时忽略（正在运行的不算，允许再排一次）。
func (q *Queue) Add(key, label string, fn func(ctx context.Context) error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.queued[key] {
		return
	}
	q.queued[key] = true
	q.pending = append(q.pending, &task{key: key, label: label, fn: fn})
	q.cond.Signal()
}

// AddFront 把任务插到队首；已在排队的同 key 任务会被移到队首（保留原来的函数），
// 同 key 任务正在运行时什么都不做（用于“优先处理”，不是“再做一次”）。
func (q *Queue) AddFront(key, label string, fn func(ctx context.Context) error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, running := q.running[key]; q.closed || running {
		return
	}
	t := &task{key: key, label: label, fn: fn}
	if q.queued[key] {
		for i, p := range q.pending {
			if p.key == key {
				t = p
				q.pending = append(q.pending[:i], q.pending[i+1:]...)
				break
			}
		}
	}
	q.queued[key] = true
	q.pending = append([]*task{t}, q.pending...)
	q.cond.Signal()
}

func (q *Queue) worker(ctx context.Context) {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		for len(q.pending) == 0 && !q.closed && q.workers <= q.target {
			q.cond.Wait()
		}
		if q.closed || q.workers > q.target {
			q.workers--
			q.mu.Unlock()
			return
		}
		t := q.pending[0]
		q.pending = q.pending[1:]
		delete(q.queued, t.key)
		q.running[t.key] = t.label
		q.mu.Unlock()

		err := safeRun(ctx, t)

		q.mu.Lock()
		delete(q.running, t.key)
		q.done++
		if err != nil && ctx.Err() == nil {
			q.failed++
			q.errors = append(q.errors, ErrorEntry{Label: t.label, Err: err.Error(), Time: time.Now().Unix()})
			if len(q.errors) > 50 {
				q.errors = q.errors[len(q.errors)-50:]
			}
			log.Printf("[%s] %s 失败: %v", q.name, t.label, err)
		}
		q.cond.Broadcast()
		q.mu.Unlock()
	}
}

func safeRun(ctx context.Context, t *task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("任务 %s 崩溃: %v", t.label, r)
			err = errPanic
		}
	}()
	return t.fn(ctx)
}

type panicErr struct{}

func (panicErr) Error() string { return "任务异常退出" }

var errPanic error = panicErr{}

func (q *Queue) Status() Status {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := Status{Name: q.name, Pending: len(q.pending), Done: q.done, Failed: q.failed,
		Errors: append([]ErrorEntry(nil), q.errors...)}
	for _, l := range q.running {
		s.Running = append(s.Running, l)
	}
	return s
}

// Idle 报告队列是否空闲。
func (q *Queue) Idle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending) == 0 && len(q.running) == 0
}

// Wait 等待队列空闲（测试用）。
func (q *Queue) Wait(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	stop := context.AfterFunc(ctx, func() {
		q.mu.Lock()
		q.cond.Broadcast()
		q.mu.Unlock()
	})
	defer stop()
	for len(q.pending) > 0 || len(q.running) > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		q.cond.Wait()
	}
	return nil
}
