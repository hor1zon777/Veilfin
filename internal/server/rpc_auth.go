package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"veilfin/internal/api"
	"veilfin/internal/auth"
	"veilfin/internal/db"
	"veilfin/internal/mediacrypt"
	"veilfin/internal/proto"
)

func init() {
	register("sys.info", public, rpcSysInfo)
	register("sys.ping", public, func(s *Server, c *call) (any, error) {
		return map[string]int64{"time": s.now().UnixMilli()}, nil
	})
	register("setup.create", public, rpcSetup)
	register("auth.login", public, rpcLogin)
	register("auth.resume", public, rpcResume)
	register("auth.logout", public, rpcLogout)
	register("auth.me", user, func(s *Server, c *call) (any, error) { return s.me(c), nil })
	register("auth.blobToken", user, rpcBlobToken)
	register("auth.changePassword", user, rpcChangePassword)
	register("auth.setPin", user, rpcSetPin)
	register("auth.unlock", user, rpcUnlock)
	register("auth.lock", user, func(s *Server, c *call) (any, error) {
		c.ss.mu.Lock()
		c.ss.unlockedUntil = time.Time{}
		c.ss.mu.Unlock()
		// 让已签发的（可能带解锁标志的）访问令牌立即失效
		if err := s.DB.BumpTokenGen(c.ctx, c.u.ID); err != nil {
			return nil, err
		}
		if s.tc != nil {
			s.tc.CloseUser(c.u.ID)
		}
		s.invalidateAccess()
		return s.me(c), nil
	})
	register("auth.setPrefs", user, func(s *Server, c *call) (any, error) {
		var p struct {
			BlurRestricted bool `json:"blurRestricted"`
		}
		if err := c.bind(&p); err != nil {
			return nil, err
		}
		if err := s.DB.SetBlur(c.ctx, c.u.ID, p.BlurRestricted); err != nil {
			return nil, err
		}
		c.u.BlurRestricted = p.BlurRestricted
		return s.me(c), nil
	})
}

type meResult struct {
	ID             int64  `json:"id"`
	Username       string `json:"username"`
	IsAdmin        bool   `json:"isAdmin"`
	AllowTranscode bool   `json:"allowTranscode"`
	HasPin         bool   `json:"hasPin"`
	Unlocked       bool   `json:"unlocked"`
	UnlockedUntil  int64  `json:"unlockedUntil"`
	BlurRestricted bool   `json:"blurRestricted"`
	ServerName     string `json:"serverName"`
	KeepPageTitle  bool   `json:"keepPageTitle"`
}

func (s *Server) me(c *call) *meResult {
	c.ss.mu.Lock()
	until := c.ss.unlockedUntil
	c.ss.mu.Unlock()
	unlocked := c.u.PinHash == "" || s.now().Before(until)
	r := &meResult{ID: c.u.ID, Username: c.u.Username, IsAdmin: c.u.IsAdmin, AllowTranscode: c.u.AllowTranscode, HasPin: c.u.PinHash != "",
		Unlocked: unlocked, BlurRestricted: c.u.BlurRestricted}
	st := s.Lib.Settings(c.ctx)
	r.ServerName, r.KeepPageTitle = st.ServerName, st.KeepPageTitle
	if c.u.PinHash != "" && unlocked {
		r.UnlockedUntil = until.Unix()
	}
	return r
}

func rpcSysInfo(s *Server, c *call) (any, error) {
	return map[string]any{
		"needsSetup": s.NeedsSetup(),
		// 剩余的初始化时间（秒），为 0 且 needsSetup 为真表示时间窗口已过，需要重启服务端
		"setupRemaining": int(s.SetupRemaining().Seconds()),
		"serverName":     s.Lib.Settings(c.ctx).ServerName,
		"version":        s.cfg.Version,
		"loggedIn":       c.u != nil,
		// 服务端身份公钥本来就是公开的（GET /k），给管理员复制到客户端的 -server-key 参数
		"publicKey":   base64.StdEncoding.EncodeToString(s.PublicKey()),
		"fingerprint": proto.Fingerprint(s.PublicKey()),
	}, nil
}

func validUsername(name string) error {
	name = strings.TrimSpace(name)
	if len([]rune(name)) < 2 || len([]rune(name)) > 32 {
		return errf(api.CodeBadRequest, "用户名长度应为 2 到 32 个字符")
	}
	if strings.ContainsAny(name, "\x00\r\n\t") {
		return errf(api.CodeBadRequest, "用户名包含非法字符")
	}
	return nil
}

func rpcSetup(s *Server, c *call) (any, error) {
	s.setupMu.Lock() // 防止并发初始化创建出多个管理员
	defer s.setupMu.Unlock()
	if !s.needsSetup {
		return nil, errf(api.CodeForbidden, "已经初始化过了")
	}
	if !s.now().Before(s.setupDeadline) {
		return nil, errf(api.CodeForbidden, "初始化时间窗口已过期：请重启服务端，然后在 %d 分钟内完成初始化", int(s.setupWindow().Minutes()))
	}
	var p struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	if err := validUsername(p.Username); err != nil {
		return nil, err
	}
	if err := auth.ValidatePassword(p.Password); err != nil {
		return nil, errf(api.CodeBadRequest, "%v", err)
	}
	uid, err := s.DB.CreateUser(c.ctx, strings.TrimSpace(p.Username), auth.Hash(p.Password), true, true)
	if err != nil {
		return nil, err
	}
	s.needsSetup = false
	if s.cfg.InitialMedia != "" {
		if libID, err := s.DB.CreateLibrary(c.ctx, &db.Library{Name: "视频", Kind: "home", Scraper: "none", Trickplay: true, Paths: []string{s.cfg.InitialMedia}}); err == nil {
			s.Lib.EnqueueScan(libID, false)
			s.Lib.RestartWatcher()
		}
	}
	return s.finishLogin(c, uid)
}

func (s *Server) setupWindow() time.Duration {
	if s.cfg.SetupWindow > 0 {
		return s.cfg.SetupWindow
	}
	return DefaultSetupWindow
}

func hashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

// finishLogin 把会话绑定到用户并签发“记住登录”令牌。
func (s *Server) finishLogin(c *call, uid int64) (any, error) {
	u, err := s.DB.UserByID(c.ctx, uid)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	if err := s.DB.AddLoginToken(c.ctx, hashToken(tok), uid, s.now().Add(loginTokenTTL).Unix()); err != nil {
		return nil, err
	}
	c.ss.setUser(uid)
	c.u = u
	return map[string]any{"token": tok, "me": s.me(c)}, nil
}

func rpcLogin(s *Server, c *call) (any, error) {
	var p struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	// 所有失败计数都是“先记一次再校验，成功后清零”：并发请求不能同时通过“是否已锁定”的检查，
	// 校验途中客户端断开也不会漏记
	if !c.ss.reserveLoginAttempt(maxLoginFails) {
		return nil, errf(api.CodeTooMany, "失败次数过多，请稍后重试")
	}
	name := strings.TrimSpace(p.Username)
	u, err := s.DB.UserByName(c.ctx, name)
	if errors.Is(err, db.ErrNotFound) {
		// 不存在的用户名走同样的流程：同样的耗时、同样的锁定规则，不能借此判断用户名是否存在
		ok := s.ghostReserve(name)
		auth.VerifyDummy(p.Password)
		if !ok {
			return nil, errAccountLocked()
		}
		return nil, s.loginFailed()
	}
	if err != nil {
		return nil, err
	}
	ok, err := s.DB.ReserveLoginAttempt(c.ctx, u.ID, attemptPolicy, s.now().Unix())
	if err != nil {
		return nil, err
	}
	if !ok {
		auth.VerifyDummy(p.Password)
		return nil, errAccountLocked()
	}
	if auth.Verify(u.PwHash, p.Password) != nil {
		return nil, s.loginFailed()
	}
	// 提前记上的那次失败要清掉；用不会被取消的 ctx，客户端断开了也要清
	if err := s.DB.RecordLoginSuccess(context.WithoutCancel(c.ctx), u.ID); err != nil {
		return nil, err
	}
	if u.Disabled {
		return nil, errf(api.CodeForbidden, "账号已被禁用")
	}
	return s.finishLogin(c, u.ID)
}

func errAccountLocked() error {
	return errf(api.CodeTooMany, "账号因多次密码错误被临时锁定，请稍后再试")
}

func (s *Server) loginFailed() error {
	s.slowFail()
	return errf(api.CodeUnauthorized, "用户名或密码错误")
}

// checkPassword 校验当前用户的密码（改密码、设置 PIN 时用）。和登录共用失败计数与锁定，
// 否则已登录的会话可以借这两个接口无限次地猜密码。
func (s *Server) checkPassword(c *call, password, wrongMsg string) error {
	ok, err := s.DB.ReserveLoginAttempt(c.ctx, c.u.ID, attemptPolicy, s.now().Unix())
	if err != nil {
		return err
	}
	if !ok {
		return errAccountLocked()
	}
	if auth.Verify(c.u.PwHash, password) != nil {
		s.slowFail()
		return errf(api.CodeUnauthorized, "%s", wrongMsg)
	}
	return s.DB.ResetLoginFailures(context.WithoutCancel(c.ctx), c.u.ID)
}

func rpcResume(s *Server, c *call) (any, error) {
	var p api.TokenParams
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	if p.Token == "" {
		return nil, errf(api.CodeUnauthorized, "登录已失效")
	}
	uid, err := s.DB.UseLoginToken(c.ctx, hashToken(p.Token), s.now().Add(loginTokenTTL).Unix())
	if err != nil {
		return nil, errf(api.CodeUnauthorized, "登录已失效")
	}
	u, err := s.DB.UserByID(c.ctx, uid)
	if err != nil || u.Disabled {
		return nil, errf(api.CodeUnauthorized, "登录已失效")
	}
	c.ss.setUser(uid)
	c.u = u
	return s.me(c), nil
}

func rpcLogout(s *Server, c *call) (any, error) {
	var p api.TokenParams
	c.bind(&p)
	if p.Token != "" {
		s.DB.DeleteLoginToken(c.ctx, hashToken(p.Token))
	}
	if c.u != nil && s.tc != nil {
		s.tc.CloseUser(c.u.ID)
	}
	c.ss.setUser(0)
	return struct{}{}, nil
}

func rpcBlobToken(s *Server, c *call) (any, error) {
	now := s.now()
	exp := now.Add(blobTokenTTL)
	var flags byte
	c.ss.mu.Lock()
	until := c.ss.unlockedUntil
	c.ss.mu.Unlock()
	if c.u.PinHash == "" {
		flags |= mediacrypt.TokenUnlocked
	} else if now.Before(until) {
		flags |= mediacrypt.TokenUnlocked
		if until.Before(exp) {
			exp = until // 解锁到期后，带解锁标志的令牌也随之失效
		}
	}
	return api.BlobToken{Token: s.keys.UserToken(c.u.ID, c.u.TokenGen, flags, exp), Exp: exp.Unix()}, nil
}

func rpcChangePassword(s *Server, c *call) (any, error) {
	var p struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	if err := auth.ValidatePassword(p.New); err != nil {
		return nil, errf(api.CodeBadRequest, "%v", err)
	}
	if err := s.checkPassword(c, p.Old, "原密码不正确"); err != nil {
		return nil, err
	}
	if err := s.DB.SetPassword(c.ctx, c.u.ID, auth.Hash(p.New)); err != nil {
		return nil, err
	}
	// 改密码多半是怀疑密码泄露：其他设备上的会话一律退出，权限缓存里的旧令牌代数也要作废。
	// 改密码还会吊销所有记住登录的令牌，给当前会话重新签发一个
	s.sessions.logoutUser(c.u.ID, c.ss)
	s.invalidateAccess()
	return s.finishLogin(c, c.u.ID)
}

func rpcSetPin(s *Server, c *call) (any, error) {
	var p struct {
		Password string `json:"password"`
		Pin      string `json:"pin"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	if err := s.checkPassword(c, p.Password, "密码不正确"); err != nil {
		return nil, err
	}
	hash := ""
	if p.Pin != "" {
		if err := auth.ValidatePin(p.Pin); err != nil {
			return nil, errf(api.CodeBadRequest, "%v", err)
		}
		hash = auth.Hash(p.Pin)
	}
	if err := s.DB.SetPin(c.ctx, c.u.ID, hash); err != nil {
		return nil, err
	}
	c.u.PinHash = hash
	s.invalidateAccess()
	c.ss.mu.Lock()
	c.ss.unlockedUntil = time.Time{}
	c.ss.mu.Unlock()
	return s.me(c), nil
}

func rpcUnlock(s *Server, c *call) (any, error) {
	var p struct {
		Pin string `json:"pin"`
	}
	if err := c.bind(&p); err != nil {
		return nil, err
	}
	if c.u.PinHash == "" {
		return s.me(c), nil
	}
	now := s.now()
	// 失败次数记在数据库里（重新握手、换设备都不会清零），并且先记后校验，见 rpcLogin
	ok, err := s.DB.ReservePinAttempt(c.ctx, c.u.ID, attemptPolicy, now.Unix())
	if err != nil {
		return nil, err
	}
	if !ok {
		if u, err := s.DB.UserByID(c.ctx, c.u.ID); err == nil && u.PinLockedUntil > now.Unix() {
			return nil, errf(api.CodeTooMany, "PIN 错误次数过多，已锁定 %d 分钟", (u.PinLockedUntil-now.Unix())/60+1)
		}
		return nil, errf(api.CodeTooMany, "PIN 错误次数过多，请稍后再试")
	}
	if auth.Verify(c.u.PinHash, p.Pin) != nil {
		s.slowFail()
		return nil, errf(api.CodeUnauthorized, "PIN 不正确")
	}
	if err := s.DB.ResetPinFailures(context.WithoutCancel(c.ctx), c.u.ID); err != nil {
		return nil, err
	}
	c.ss.mu.Lock()
	c.ss.unlockedUntil = now.Add(unlockDuration)
	c.ss.mu.Unlock()
	return s.me(c), nil
}

// ---- 不存在的用户名的失败计数（只在内存中） ----

// ghostReserve 给不存在的用户名记一次失败，规则与真实账号相同（见 db.AttemptPolicy）。
// 返回 false 表示正处于锁定期。
func (s *Server) ghostReserve(name string) bool {
	s.ghostMu.Lock()
	defer s.ghostMu.Unlock()
	if len(s.ghosts) > 10000 {
		s.ghosts = map[string]*ghost{}
	}
	now := s.now()
	key := strings.ToLower(name)
	g := s.ghosts[key]
	if g == nil {
		g = &ghost{}
		s.ghosts[key] = g
	}
	if now.Before(g.until) {
		return false
	}
	if now.Sub(g.last) >= lockoutTime {
		g.fails = 0
	}
	g.fails++
	g.last = now
	if g.fails >= maxLoginFails {
		g.until = now.Add(lockoutTime)
	}
	return true
}
