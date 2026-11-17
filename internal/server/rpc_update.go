package server

// 在线更新：管理后台检查 GitHub Release 上的新版本，服务端自己下载、校验、替换程序并重启。
//
// 流程：查询发布 → 下载与本机平台对应的程序到程序目录下的临时文件 → 用 SHA256SUMS.txt 校验 →
// 试运行确认版本号 → 替换程序（旧程序保留为 .old）→ 关闭服务端并用新程序重新启动。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"veilfin/internal/api"
	"veilfin/internal/media"
	"veilfin/internal/update"
)

const (
	updateCheckEvery    = 12 * time.Hour // 自动检查间隔
	updateCheckMinDelay = 10 * time.Second
)

type updateState struct {
	mu       sync.Mutex
	latest   *update.Release // 最近一次检查的结果（nil 表示没有找到可用的发布）
	checked  time.Time
	checkErr string
	pre      bool // 最近一次检查是否包含预发布版本
	job      *updateJob
}

// updateJob 是正在进行或最近一次失败的更新。
type updateJob struct {
	Tag     string `json:"tag"`
	Phase   string `json:"phase"` // download：下载；verify：校验；install：替换程序；wait：等备份任务结束；restart：重启
	Done    int64  `json:"done"`
	Total   int64  `json:"total"`
	Running bool   `json:"running"`
	Err     string `json:"err,omitempty"`
}

type releaseOut struct {
	Tag       string `json:"tag"`
	Name      string `json:"name"`
	Body      string `json:"body"`
	URL       string `json:"url"`
	Published int64  `json:"published"`
	Pre       bool   `json:"prerelease"`
	Asset     string `json:"asset,omitempty"` // 适用于本机的程序文件，为空表示这个版本没有本机平台的程序
	Size      int64  `json:"size,omitempty"`
	Newer     bool   `json:"newer"` // 比当前版本新（当前是开发版本、无法比较时也为真）
}

func init() {
	register("admin.update.status", admin, func(s *Server, c *call) (any, error) { return s.updateStatus(), nil })
	register("admin.update.check", admin, rpcUpdateCheck)
	register("admin.update.apply", admin, rpcUpdateApply)
}

func (s *Server) updateClient() (*update.Client, error) {
	hc, err := update.NewHTTPClient(s.Lib.Settings(s.ctx).ScraperProxy)
	if err != nil {
		return nil, err
	}
	return &update.Client{HTTP: hc, API: s.cfg.UpdateAPI, Repo: s.cfg.UpdateRepo, UserAgent: "Veilfin/" + s.cfg.Version}, nil
}

// updateBlocker 返回不能在线更新的原因，可以更新时返回空字符串。
func (s *Server) updateBlocker() string {
	if s.cfg.Reexec == nil || s.cfg.Executable == "" {
		return "当前运行方式不支持在线更新，请手动替换程序文件。"
	}
	if update.InContainer() {
		return "服务端运行在 Docker 容器中，容器重建后替换的程序会丢失。请拉取最新代码后重新构建镜像：git pull && docker compose up -d --build"
	}
	if err := update.CheckWritable(s.cfg.Executable); err != nil {
		return fmt.Sprintf("运行服务端的用户没有程序所在目录（%s）的写权限，无法替换程序。请手动更新，或授予该目录的写权限。", filepath.Dir(s.cfg.Executable))
	}
	return ""
}

func (s *Server) currentVersion() (update.Version, bool) { return update.ParseVersion(s.cfg.Version) }

func (s *Server) releaseOut(r *update.Release) *releaseOut {
	if r == nil {
		return nil
	}
	o := &releaseOut{Tag: r.Tag, Name: r.Name, Body: r.Body, URL: r.HTMLURL, Published: r.Published.Unix(), Pre: r.Prerelease}
	if a := update.PickAsset(r, runtime.GOOS, runtime.GOARCH, media.HasEmbedded()); a != nil {
		o.Asset, o.Size = a.Name, a.Size
	}
	v, ok := update.ParseVersion(r.Tag)
	cur, cok := s.currentVersion()
	o.Newer = ok && (!cok || update.Compare(v, cur) > 0)
	return o
}

func (s *Server) updateStatus() map[string]any {
	_, known := s.currentVersion()
	out := map[string]any{
		"current":        s.cfg.Version,
		"currentKnown":   known, // 为假表示开发版本或本地构建，无法与发布比较
		"platform":       runtime.GOOS + "/" + runtime.GOARCH,
		"embeddedFFmpeg": media.HasEmbedded(),
		"reason":         s.updateBlocker(),
	}
	if s.cfg.Executable != "" {
		out["oldExecutable"] = update.OldPath(s.cfg.Executable)
	}
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	out["latest"] = s.releaseOut(s.upd.latest)
	out["checkErr"] = s.upd.checkErr
	out["prerelease"] = s.upd.pre
	if !s.upd.checked.IsZero() {
		out["checkedAt"] = s.upd.checked.Unix()
	}
	if s.upd.job != nil {
		j := *s.upd.job
		out["job"] = &j
	}
	return out
}

// checkUpdate 查询最新版本并记录结果。
func (s *Server) checkUpdate(ctx context.Context, pre bool) error {
	uc, err := s.updateClient()
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var r *update.Release
		if r, err = uc.Latest(ctx, pre); err == nil {
			s.upd.mu.Lock()
			s.upd.latest, s.upd.checkErr = r, ""
			s.upd.pre, s.upd.checked = pre, s.now()
			s.upd.mu.Unlock()
			return nil
		}
	}
	s.upd.mu.Lock()
	s.upd.checkErr, s.upd.pre, s.upd.checked = "检查更新失败："+err.Error(), pre, s.now()
	s.upd.mu.Unlock()
	return err
}

// runUpdateChecker 按设置定时检查更新（只检查，不会自动安装）。
func (s *Server) runUpdateChecker(ctx context.Context) {
	if s.cfg.Version == "" || s.cfg.Version == "test" {
		return
	}
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.upd.mu.Lock()
		last, pre := s.upd.checked, s.upd.pre
		s.upd.mu.Unlock()
		if last.IsZero() {
			// 当前运行的是预发布版本时，默认也检查预发布版本
			cur, ok := s.currentVersion()
			pre = ok && cur.Pre != ""
		}
		wait := updateCheckEvery
		if s.Lib.Settings(ctx).AutoCheckUpdate {
			if d := s.now().Sub(last); !last.IsZero() && d < updateCheckEvery {
				wait = updateCheckEvery - d // 管理员刚手动检查过
			} else if err := s.checkUpdate(ctx, pre); err != nil {
				log.Printf("检查更新失败: %v", err)
				wait = time.Hour
			}
		}
		t.Reset(wait)
	}
}

func rpcUpdateCheck(s *Server, c *call) (any, error) {
	var p struct {
		Prerelease bool `json:"prerelease"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	s.upd.mu.Lock()
	recent := s.upd.checkErr == "" && !s.upd.checked.IsZero() && s.upd.pre == p.Prerelease && s.now().Sub(s.upd.checked) < updateCheckMinDelay
	s.upd.mu.Unlock()
	if !recent {
		if err := s.checkUpdate(c.ctx, p.Prerelease); err != nil {
			return nil, errf(api.CodeBadRequest, "检查更新失败：%v", err)
		}
	}
	return s.updateStatus(), nil
}

func rpcUpdateApply(s *Server, c *call) (any, error) {
	var p struct {
		Tag string `json:"tag"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	want, ok := update.ParseVersion(p.Tag)
	if !ok {
		return nil, errf(api.CodeBadRequest, "版本号无效")
	}
	if cur, ok := s.currentVersion(); ok && update.Compare(want, cur) <= 0 {
		return nil, errf(api.CodeBadRequest, "%s 不比当前版本新", p.Tag)
	}
	if reason := s.updateBlocker(); reason != "" {
		return nil, errf(api.CodeBadRequest, "%s", reason)
	}

	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	if s.upd.job != nil && s.upd.job.Running {
		return nil, errf(api.CodeBadRequest, "正在更新，请稍候")
	}
	s.bk.mu.Lock()
	switch {
	case s.bk.restarting:
		s.bk.mu.Unlock()
		return nil, errf(api.CodeBadRequest, "服务端正在重启")
	case s.bk.job != nil && s.bk.job.Running:
		s.bk.mu.Unlock()
		return nil, errf(api.CodeBadRequest, "正在进行备份或恢复，请等它完成后再更新")
	}
	s.updating.Store(true) // 更新期间不允许开始新的备份 / 恢复
	s.bk.mu.Unlock()

	j := &updateJob{Tag: p.Tag, Phase: "download", Running: true}
	s.upd.job = j
	log.Printf("管理员 %s 开始更新到 %s", c.u.Username, p.Tag)
	go func() {
		err := s.runUpdate(j, p.Tag, want)
		if err == nil {
			return // 已经开始重启
		}
		log.Printf("更新到 %s 失败: %v", p.Tag, err)
		s.updating.Store(false)
		s.upd.mu.Lock()
		j.Running, j.Err = false, err.Error()
		s.upd.mu.Unlock()
	}()
	return struct{}{}, nil
}

func (s *Server) setUpdatePhase(j *updateJob, phase string) {
	s.upd.mu.Lock()
	j.Phase = phase
	s.upd.mu.Unlock()
}

func (s *Server) runUpdate(j *updateJob, tag string, want update.Version) error {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Minute)
	defer cancel()
	uc, err := s.updateClient()
	if err != nil {
		return err
	}
	rel, err := uc.ByTag(ctx, tag)
	if err != nil {
		return fmt.Errorf("查询发布 %s 失败: %w", tag, err)
	}
	if rel.Draft {
		return errors.New("这个版本还没有正式发布")
	}
	a := update.PickAsset(rel, runtime.GOOS, runtime.GOARCH, media.HasEmbedded())
	if a == nil {
		return fmt.Errorf("发布 %s 中没有适用于 %s/%s 的程序", tag, runtime.GOOS, runtime.GOARCH)
	}
	exe := s.cfg.Executable
	tmp := update.NewPath(exe)
	s.upd.mu.Lock()
	j.Total = a.Size
	s.upd.mu.Unlock()
	err = uc.Download(ctx, rel, a, tmp, func(done, total int64) {
		s.upd.mu.Lock()
		j.Done, j.Total = done, total
		s.upd.mu.Unlock()
	})
	if err != nil {
		return err
	}
	s.setUpdatePhase(j, "verify")
	verify := s.cfg.VerifyUpdate
	if verify == nil {
		verify = update.Verify
	}
	if err := verify(ctx, tmp, want); err != nil {
		os.Remove(tmp)
		return err
	}
	s.setUpdatePhase(j, "install")
	if err := update.Install(exe, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	log.Printf("已下载并校验 %s（%s），旧程序保留为 %s", tag, a.Name, update.OldPath(exe))

	// 等正在进行的备份 / 恢复结束（开始更新时已禁止新的任务），然后重启
	s.setUpdatePhase(j, "wait")
	for {
		s.bk.mu.Lock()
		if s.bk.job == nil || !s.bk.job.Running {
			s.bk.restarting = true
			s.bk.mu.Unlock()
			break
		}
		s.bk.mu.Unlock()
		select {
		case <-s.ctx.Done():
			return nil // 服务端正在退出，下次启动就是新版本
		case <-time.After(time.Second):
		}
	}
	s.setUpdatePhase(j, "restart")
	log.Printf("服务端即将重启为 %s", tag)
	go func() {
		time.Sleep(800 * time.Millisecond) // 让管理后台有机会看到“正在重启”
		s.cfg.Reexec()
	}()
	return nil
}
