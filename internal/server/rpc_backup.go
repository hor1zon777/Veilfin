package server

// 管理后台的备份与恢复。
//
// 备份文件保存在数据目录的 backups/ 下，格式与命令行 veilfin backup 相同（口令加密）。
// 下载和上传都通过加密 RPC 分块传输，CDN 看到的仍然只是不透明的密文。
//
// 恢复分两步：先在后台解密、校验并解包到 restore-staged/，把备份里的服务端公钥告诉管理员；
// 管理员确认后改名为 restore-pending/，请求外层重启服务端，重启前应用恢复。

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"veilfin/internal/api"
	"veilfin/internal/backup"
	"veilfin/internal/proto"
)

const (
	backupDirName = "backups"
	backupExt     = ".vfbak"
	// backupChunk 是分块下载 / 上传的块大小。base64 编码后约 5.4 MiB，低于单条消息 8 MiB 的上限。
	backupChunk = 4 << 20
	uploadTTL   = 24 * time.Hour
)

type backupState struct {
	mu         sync.Mutex
	job        *backupJob
	staged     *stagedRestore
	uploads    map[string]string // 上传 ID → 临时文件路径
	restarting bool
}

// backupJob 是正在进行或最近一次完成的后台任务（同一时间只允许一个）。
type backupJob struct {
	Kind    string `json:"kind"` // backup：创建备份；restore：解包校验备份
	Name    string `json:"name"`
	Running bool   `json:"running"`
	Err     string `json:"err,omitempty"`
	Started int64  `json:"started"`
	Ended   int64  `json:"ended,omitempty"`
	// Written 是正在创建的备份已写入的字节数（仅用于显示进度）
	Written int64 `json:"written,omitempty"`
}

// stagedRestore 是已解包、等待管理员确认的恢复。
type stagedRestore struct {
	Name        string `json:"name"`
	PublicKey   string `json:"publicKey"`
	Fingerprint string `json:"fingerprint"`
	KeyChanged  bool   `json:"keyChanged"` // 备份里的服务端密钥与当前不同，恢复后客户端需要重新信任
	HasAssets   bool   `json:"hasAssets"`
}

type backupFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Time int64  `json:"time"`
}

func init() {
	register("admin.backup.list", admin, rpcBackupList)
	register("admin.backup.create", admin, rpcBackupCreate)
	register("admin.backup.delete", admin, rpcBackupDelete)
	register("admin.backup.read", admin, rpcBackupRead)
	register("admin.backup.upload", admin, rpcBackupUpload)
	register("admin.backup.restore", admin, rpcBackupRestore)
	register("admin.backup.restore.cancel", admin, rpcBackupRestoreCancel)
	register("admin.backup.restore.apply", admin, rpcBackupRestoreApply)
}

func (s *Server) backupDir() string { return filepath.Join(s.cfg.DataDir, backupDirName) }

// cleanBackupDir 在启动时清理上次没有完成的备份和上传。
func (s *Server) cleanBackupDir() {
	parts, _ := filepath.Glob(filepath.Join(s.backupDir(), "*.part"))
	for _, p := range parts {
		os.Remove(p)
	}
}

var backupNameRe = regexp.MustCompile(`^[\p{L}\p{N}_\-][\p{L}\p{N}_.\-]*\.vfbak$`)

// backupPath 校验文件名（只允许备份目录下的 .vfbak 文件）并返回完整路径。
func (s *Server) backupPath(name string) (string, error) {
	if len(name) > 200 || !backupNameRe.MatchString(name) || filepath.Base(name) != name {
		return "", errf(api.CodeBadRequest, "备份文件名无效")
	}
	return filepath.Join(s.backupDir(), name), nil
}

// sanitizeBackupName 把上传的文件名整理成合法的备份文件名，已存在时自动加序号。
func (s *Server) sanitizeBackupName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, backupExt)
	var b strings.Builder
	for _, r := range name {
		if r == '_' || r == '-' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	base := strings.TrimLeft(b.String(), ".")
	if r := []rune(base); len(r) > 60 {
		base = string(r[:60])
	}
	if base == "" {
		base = "upload-" + time.Now().Format("20060102-150405")
	}
	cand := base + backupExt
	for i := 1; ; i++ {
		if _, err := os.Stat(filepath.Join(s.backupDir(), cand)); errors.Is(err, os.ErrNotExist) {
			return cand
		}
		cand = base + "-" + strconv.Itoa(i) + backupExt
	}
}

func rpcBackupList(s *Server, c *call) (any, error) {
	dir := s.backupDir()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	files := []backupFile{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if strings.HasPrefix(name, ".upload-") && time.Since(info.ModTime()) > uploadTTL {
			os.Remove(filepath.Join(dir, name)) // 放弃了的上传
			continue
		}
		if !backupNameRe.MatchString(name) {
			continue
		}
		files = append(files, backupFile{Name: name, Size: info.Size(), Time: info.ModTime().Unix()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Time > files[j].Time })

	s.bk.mu.Lock()
	defer s.bk.mu.Unlock()
	var job *backupJob
	if s.bk.job != nil {
		j := *s.bk.job
		if j.Running && j.Kind == "backup" {
			if st, err := os.Stat(filepath.Join(dir, j.Name+".part")); err == nil {
				j.Written = st.Size()
			}
		}
		job = &j
	}
	return map[string]any{
		"dir":        dir,
		"files":      files,
		"job":        job,
		"staged":     s.bk.staged,
		"canRestore": s.cfg.Restart != nil,
		"restarting": s.bk.restarting,
	}, nil
}

// startJobLocked 登记一个后台任务；已有任务在运行时拒绝。调用方需持有 s.bk.mu。
func (s *Server) startJobLocked(kind, name string) (*backupJob, error) {
	if s.bk.restarting {
		return nil, errf(api.CodeBadRequest, "服务端正在重启")
	}
	if s.bk.job != nil && s.bk.job.Running {
		return nil, errf(api.CodeBadRequest, "已有备份或恢复任务正在进行，请等它完成")
	}
	j := &backupJob{Kind: kind, Name: name, Running: true, Started: s.now().Unix()}
	s.bk.job = j
	return j, nil
}

func (s *Server) finishJob(j *backupJob, err error) {
	s.bk.mu.Lock()
	defer s.bk.mu.Unlock()
	j.Running = false
	j.Ended = s.now().Unix()
	if err != nil {
		j.Err = err.Error()
		log.Printf("%s %s 失败: %v", map[string]string{"backup": "备份", "restore": "恢复"}[j.Kind], j.Name, err)
	}
}

func rpcBackupCreate(s *Server, c *call) (any, error) {
	var p struct {
		Pass   string `json:"pass"`
		Assets bool   `json:"assets"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	if len(p.Pass) < 8 {
		return nil, errf(api.CodeBadRequest, "备份口令至少 8 位")
	}
	if err := os.MkdirAll(s.backupDir(), 0o700); err != nil {
		return nil, err
	}
	name := "veilfin-" + s.now().Format("20060102-150405") + backupExt
	out := filepath.Join(s.backupDir(), name)
	if _, err := os.Stat(out); err == nil {
		return nil, errf(api.CodeBadRequest, "同名备份已存在，请稍后再试")
	}
	s.bk.mu.Lock()
	j, err := s.startJobLocked("backup", name)
	s.bk.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go func() {
		part := out + ".part"
		err := backup.CreateFrom(s.ctx, s.DB, s.cfg.DataDir, part, p.Pass, p.Assets)
		if err == nil {
			if err = os.Rename(part, out); err != nil {
				os.Remove(part)
			}
		}
		if err == nil {
			log.Printf("管理后台创建了备份 %s", name)
		}
		s.finishJob(j, err)
	}()
	return map[string]string{"name": name}, nil
}

func rpcBackupDelete(s *Server, c *call) (any, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	path, err := s.backupPath(p.Name)
	if err != nil {
		return nil, err
	}
	s.bk.mu.Lock()
	busy := s.bk.job != nil && s.bk.job.Running && s.bk.job.Name == p.Name
	s.bk.mu.Unlock()
	if busy {
		return nil, errf(api.CodeBadRequest, "这个备份正在使用中")
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errf(api.CodeNotFound, "备份不存在")
		}
		return nil, err
	}
	return struct{}{}, nil
}

// rpcBackupRead 分块读取备份文件，前端拼接后保存到本地。
func rpcBackupRead(s *Server, c *call) (any, error) {
	var p struct {
		Name   string `json:"name"`
		Offset int64  `json:"offset"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	path, err := s.backupPath(p.Name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errf(api.CodeNotFound, "备份不存在")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if p.Offset < 0 || p.Offset > st.Size() {
		return nil, errf(api.CodeBadRequest, "读取位置无效")
	}
	buf := make([]byte, min(int64(backupChunk), st.Size()-p.Offset))
	if _, err := f.ReadAt(buf, p.Offset); err != nil && err != io.EOF {
		return nil, err
	}
	return map[string]any{
		"data": base64.StdEncoding.EncodeToString(buf),
		"size": st.Size(),
		"eof":  p.Offset+int64(len(buf)) >= st.Size(),
	}, nil
}

// rpcBackupUpload 分块上传备份文件。第一块不带 id，服务端分配；最后一块带 done 和原文件名。
func rpcBackupUpload(s *Server, c *call) (any, error) {
	var p struct {
		ID       string `json:"id"`
		Offset   int64  `json:"offset"`
		Data     string `json:"data"` // base64
		Done     bool   `json:"done"`
		Filename string `json:"filename"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(p.Data)
	if err != nil || len(data) > backupChunk {
		return nil, errf(api.CodeBadRequest, "上传数据格式错误")
	}
	if err := os.MkdirAll(s.backupDir(), 0o700); err != nil {
		return nil, err
	}
	s.bk.mu.Lock()
	defer s.bk.mu.Unlock()
	if s.bk.restarting {
		return nil, errf(api.CodeBadRequest, "服务端正在重启")
	}
	if s.bk.uploads == nil {
		s.bk.uploads = map[string]string{}
	}
	var path string
	if p.ID == "" {
		if p.Offset != 0 {
			return nil, errf(api.CodeBadRequest, "上传位置无效")
		}
		var b [12]byte
		rand.Read(b[:])
		p.ID = hex.EncodeToString(b[:])
		path = filepath.Join(s.backupDir(), ".upload-"+p.ID+".part")
		s.bk.uploads[p.ID] = path
	} else if path = s.bk.uploads[p.ID]; path == "" {
		return nil, errf(api.CodeBadRequest, "上传已失效，请重新上传")
	}
	fail := func(err error) (any, error) {
		os.Remove(path)
		delete(s.bk.uploads, p.ID)
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	st, err := f.Stat()
	if err != nil || st.Size() != p.Offset {
		f.Close()
		return fail(errf(api.CodeBadRequest, "上传位置不连续，请重新上传"))
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	if !p.Done {
		return map[string]any{"id": p.ID, "size": p.Offset + int64(len(data))}, nil
	}
	delete(s.bk.uploads, p.ID)
	if !backup.IsBackupFile(path) {
		os.Remove(path)
		return nil, errf(api.CodeBadRequest, "这不是 Veilfin 备份文件")
	}
	name := s.sanitizeBackupName(p.Filename)
	if err := os.Rename(path, filepath.Join(s.backupDir(), name)); err != nil {
		os.Remove(path)
		return nil, err
	}
	log.Printf("管理后台上传了备份 %s", name)
	return map[string]any{"name": name}, nil
}

// rpcBackupRestore 在后台解密、校验并解包备份，完成后等待管理员确认。
func rpcBackupRestore(s *Server, c *call) (any, error) {
	var p struct {
		Name string `json:"name"`
		Pass string `json:"pass"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	if s.cfg.Restart == nil {
		return nil, errf(api.CodeBadRequest, "当前运行方式不支持在线恢复，请停止服务端后使用 veilfin restore")
	}
	path, err := s.backupPath(p.Name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, errf(api.CodeNotFound, "备份不存在")
	}
	s.bk.mu.Lock()
	j, err := s.startJobLocked("restore", p.Name)
	if err == nil {
		s.bk.staged = nil
	}
	s.bk.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go func() {
		st, err := s.stageRestore(path, p.Name, p.Pass)
		if err == nil {
			s.bk.mu.Lock()
			s.bk.staged = st
			s.bk.mu.Unlock()
		}
		s.finishJob(j, err)
	}()
	return struct{}{}, nil
}

func (s *Server) stageRestore(path, name, pass string) (*stagedRestore, error) {
	staged := filepath.Join(s.cfg.DataDir, backup.StagedDir)
	os.RemoveAll(staged)
	// 解包到数据目录下的临时目录（与数据目录同一个文件系统，之后可以直接改名）
	tmp, err := os.MkdirTemp(s.cfg.DataDir, ".restore-*")
	if err != nil {
		return nil, err
	}
	info, err := backup.Extract(path, tmp, pass)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, staged); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	cur := s.PublicKey()
	return &stagedRestore{
		Name:        name,
		PublicKey:   base64.StdEncoding.EncodeToString(info.PublicKey),
		Fingerprint: proto.Fingerprint(info.PublicKey),
		KeyChanged:  !cur.Equal(info.PublicKey),
		HasAssets:   info.HasAssets,
	}, nil
}

func rpcBackupRestoreCancel(s *Server, c *call) (any, error) {
	s.bk.mu.Lock()
	defer s.bk.mu.Unlock()
	if s.bk.job != nil && s.bk.job.Running && s.bk.job.Kind == "restore" {
		return nil, errf(api.CodeBadRequest, "正在校验备份，请稍候")
	}
	s.bk.staged = nil
	os.RemoveAll(filepath.Join(s.cfg.DataDir, backup.StagedDir))
	return struct{}{}, nil
}

// rpcBackupRestoreApply 确认恢复：把暂存目录改为待恢复，然后请求重启服务端。
func rpcBackupRestoreApply(s *Server, c *call) (any, error) {
	s.bk.mu.Lock()
	defer s.bk.mu.Unlock()
	if s.cfg.Restart == nil {
		return nil, errf(api.CodeBadRequest, "当前运行方式不支持在线恢复")
	}
	if s.bk.restarting {
		return struct{}{}, nil
	}
	if s.bk.staged == nil {
		return nil, errf(api.CodeBadRequest, "没有待确认的恢复，请重新选择备份")
	}
	if s.bk.job != nil && s.bk.job.Running {
		return nil, errf(api.CodeBadRequest, "已有备份或恢复任务正在进行，请等它完成")
	}
	pending := filepath.Join(s.cfg.DataDir, backup.PendingDir)
	os.RemoveAll(pending)
	if err := os.Rename(filepath.Join(s.cfg.DataDir, backup.StagedDir), pending); err != nil {
		return nil, err
	}
	log.Printf("管理员 %s 确认从备份 %s 恢复，服务端即将重启", c.u.Username, s.bk.staged.Name)
	s.bk.staged = nil
	s.bk.restarting = true
	// 先把响应发出去，再重启
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.cfg.Restart()
	}()
	return struct{}{}, nil
}
