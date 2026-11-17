// Veilfin：带应用层端到端加密的个人视频服务。
//
//	veilfin server   在源站运行
//	veilfin client   在自己电脑上运行，浏览器访问 http://127.0.0.1:8096
//	veilfin keyinfo  查看服务端公钥
//	veilfin backup   加密备份密钥和数据库
//	veilfin restore  从备份恢复
//	veilfin user     用户管理（忘记密码时重置）
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"veilfin/internal/auth"
	"veilfin/internal/backup"
	"veilfin/internal/client"
	"veilfin/internal/db"
	"veilfin/internal/media"
	"veilfin/internal/proto"
	"veilfin/internal/server"
	"veilfin/internal/update"
)

// version 在发布构建时通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func init() { version = describeVersion(version) }

// describeVersion 在版本号后面附上编译时的 git 提交（Go 自动写入程序），
// 例如 "20261117 (7ea7177)"、"dev (7ea7177, 有未提交的修改)"，方便确认运行的是哪次构建。
func describeVersion(v string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return v
	}
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) < 7 {
		return v
	}
	extra := rev[:7]
	if dirty {
		extra += ", 有未提交的修改"
	}
	return v + " (" + extra + ")"
}

func usage() {
	fmt.Fprint(os.Stderr, `Veilfin `+version+`

用法:
  veilfin server  [选项]   启动服务端（部署在源站，放在 CDN 之后）
  veilfin client  [选项]   启动本地客户端（在自己电脑上运行，然后用浏览器访问）
  veilfin keyinfo [选项]   显示服务端身份公钥，用于客户端固定信任
  veilfin backup  [选项]   加密备份服务端密钥、数据库和图片缓存
  veilfin restore [选项]   从备份恢复
  veilfin user reset-password [选项]   重置用户密码

使用 "veilfin <命令> -h" 查看各命令的选项。
`)
}

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "server":
		err = runServer(args)
	case "client":
		err = runClient(args)
	case "keyinfo":
		err = runKeyInfo(args)
	case "backup":
		err = runBackup(args)
	case "restore":
		err = runRestore(args)
	case "user":
		err = runUser(args)
	case "version", "-v", "--version":
		fmt.Println(version)
		if media.HasEmbedded() {
			fmt.Println("内置 ffmpeg " + media.EmbeddedVersion() + "（LGPL）")
		}
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("错误: %v", err)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fs.String("listen", envOr("VF_LISTEN", "0.0.0.0:8097"), "监听地址（环境变量 VF_LISTEN）")
	data := fs.String("data", envOr("VF_DATA", "./data"), "数据目录，存放密钥和数据库，务必备份（VF_DATA）")
	media := fs.String("media", os.Getenv("VF_MEDIA"), "可选：首次初始化时自动创建指向该目录的媒体库（VF_MEDIA）")
	webEnv := strings.ToLower(os.Getenv("VF_WEB"))
	web := fs.Bool("web", webEnv != "0" && webEnv != "false", "提供网页模式（浏览器直接访问，手机等无法运行客户端的设备使用），默认开启；-web=false 或 VF_WEB=0 关闭")
	apiBase := fs.String("api-base", os.Getenv("VF_API_BASE"), "网页模式下前端访问 API 的地址（前端与 API 分开部署时填写）")
	webOrigin := fs.String("web-origin", os.Getenv("VF_WEB_ORIGIN"), "允许跨域访问 API 的前端地址（前端与 API 分开部署时填写）")
	ffdir := fs.String("ffmpeg", os.Getenv("VF_FFMPEG"), "ffmpeg / ffprobe 所在目录，默认在 PATH 中查找，找不到时使用内置版本（如有）")
	tlsCert := fs.String("tls-cert", os.Getenv("VF_TLS_CERT"), "TLS 证书文件（可选）")
	tlsKey := fs.String("tls-key", os.Getenv("VF_TLS_KEY"), "TLS 私钥文件（可选）")
	fs.Parse(args)

	if *media != "" {
		abs, err := filepath.Abs(*media)
		if err != nil {
			return err
		}
		*media = abs
	}
	ctx, cancel := signalContext()
	defer cancel()
	cfg := server.Config{
		DataDir: *data, InitialMedia: *media, Web: *web, APIBase: strings.TrimRight(*apiBase, "/"),
		WebOrigin: strings.TrimRight(*webOrigin, "/"), FFmpegDir: *ffdir, Version: version,
		UpdateRepo: os.Getenv("VF_UPDATE_REPO"),
	}
	// 在线更新：替换程序文件后关闭服务端，再用新程序重新启动进程
	var reexec atomic.Bool
	if exe, err := update.Executable(); err == nil {
		cfg.Executable = exe
		cfg.Reexec = func() {
			reexec.Store(true)
			cfg.Restart()
		}
	} else {
		log.Printf("无法确定程序路径，在线更新不可用: %v", err)
	}
	// 管理后台在线恢复时会请求重启：在同一进程内关闭服务端、应用恢复、重新启动，不依赖外部的进程管理
	for first := true; ; first = false {
		restart, err := serveOnce(ctx, &cfg, *listen, *tlsCert, *tlsKey, first)
		if err != nil || !restart {
			return err
		}
		if reexec.Swap(false) {
			log.Printf("正在启动新版本 %s ……", cfg.Executable)
			err := update.Reexec(cfg.Executable) // 成功时不会返回
			log.Printf("启动新版本失败，继续运行当前版本（程序文件已经替换，下次启动时生效）: %v", err)
		}
		log.Printf("服务端正在重启……")
	}
}

// serveOnce 启动一次服务端，直到收到退出信号（返回 false）或在线恢复请求重启（返回 true）。
func serveOnce(parent context.Context, cfg *server.Config, listen, tlsCert, tlsKey string, first bool) (bool, error) {
	applied, err := backup.ApplyPending(cfg.DataDir)
	if err != nil {
		return false, err
	}
	if applied {
		log.Printf("已从备份恢复数据目录 %s", cfg.DataDir)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	restartCh := make(chan struct{}, 1)
	cfg.Restart = func() {
		select {
		case restartCh <- struct{}{}:
		default:
		}
	}
	srv, err := server.New(ctx, *cfg)
	if err != nil {
		return false, err
	}
	defer func() {
		cancel()
		srv.Lib.Stopped() // 等后台任务退出后再关闭数据库，之后才能安全地替换数据库文件
		srv.Close()
	}()
	pub := srv.PublicKey()
	if first || applied {
		log.Printf("Veilfin %s", version)
		log.Printf("服务端公钥: %s", base64.StdEncoding.EncodeToString(pub))
		log.Printf("公钥指纹:   %s", proto.Fingerprint(pub))
	}
	localURL := localServerURL(listen, tlsCert != "")
	clientCmd := fmt.Sprintf("%s client -server %s -server-key '%s'", filepath.Base(os.Args[0]), localURL, base64.StdEncoding.EncodeToString(pub))
	if srv.NeedsSetup() {
		mins := int(server.DefaultSetupWindow.Minutes())
		if cfg.Web {
			log.Printf("★ 尚未初始化：请在 %d 分钟内用浏览器打开 %s ，按设置向导创建管理员（超时后重启服务端即可重新计时）", mins, localURL)
		} else {
			log.Printf("★ 尚未初始化：请在 %d 分钟内用本地客户端连接本服务，在打开的页面里按设置向导创建管理员（超时后重启服务端即可重新计时）：", mins)
			log.Printf("    %s", clientCmd)
			log.Printf("  也可以去掉 -web=false（或 VF_WEB=0）重新启动，直接用浏览器打开 %s", localURL)
		}
	}
	if first {
		if cfg.Web {
			log.Printf("网页模式已开启：浏览器打开 %s（网页模式需要 HTTPS，本机的 localhost / 127.0.0.1 除外）", localURL)
		} else if !srv.NeedsSetup() {
			log.Printf("网页模式已关闭（-web=false 或 VF_WEB=0），浏览器直接访问本端口会返回 404，请用本地客户端连接")
		}
	}
	go srv.Run(ctx)

	hs := &http.Server{
		Addr:              listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	restart := make(chan bool, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			restart <- false
		case <-restartCh:
			restart <- true
		}
		// Shutdown 会等正在处理的请求结束，之后才能关闭数据库
		sctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		hs.Shutdown(sctx)
	}()
	log.Printf("服务端监听 %s", listen)
	if tlsCert != "" {
		err = hs.ListenAndServeTLS(tlsCert, tlsKey)
	} else {
		err = hs.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		<-stopped
		return <-restart && parent.Err() == nil, nil
	}
	return false, err
}

func runClient(args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	srvURL := fs.String("server", os.Getenv("VF_SERVER"), "服务端地址，例如 https://cdn.example.com（必填，VF_SERVER）")
	listen := fs.String("listen", envOr("VF_CLIENT_LISTEN", "0.0.0.0:8096"), "监听地址（VF_CLIENT_LISTEN）；每个浏览器都需要自己登录")
	data := fs.String("data", defaultClientDir(), "客户端数据目录")
	key := fs.String("server-key", os.Getenv("VF_SERVER_KEY"), "服务端公钥（base64，强烈建议填写；留空则首次连接时信任）")
	open := fs.Bool("open", true, "启动后自动打开浏览器")
	fs.Parse(args)
	if *srvURL == "" {
		return errors.New("必须用 -server 指定服务端地址")
	}

	// 客户端常驻后台：设置软内存上限，让 GC 在连续播放大文件时及时回收解密缓冲
	debug.SetMemoryLimit(160 << 20)
	ctx, cancel := signalContext()
	defer cancel()
	cli, err := client.New(ctx, client.Config{Server: *srvURL, DataDir: *data, ServerKey: *key, Version: version})
	if err != nil {
		return err
	}
	log.Printf("服务端公钥指纹: %s", cli.ServerFingerprint())
	if cli.Warning != "" {
		log.Printf("注意：%s", cli.Warning)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: cli.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		hs.Shutdown(sctx)
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	log.Printf("本地客户端已启动：%s（Veilfin %s）", url, version)
	if host, _, _ := net.SplitHostPort(*listen); host == "" || net.ParseIP(host).IsUnspecified() {
		for _, ip := range lanIPs() {
			log.Printf("局域网访问：http://%s/", net.JoinHostPort(ip, fmt.Sprint(port)))
		}
	}
	if *open {
		openBrowser(url)
	}
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// localServerURL 返回在本机访问服务端的地址，用于启动提示。
func localServerURL(listen string, tls bool) string {
	scheme := "http"
	if tls {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return scheme + "://" + listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// lanIPs 列出本机的局域网 IPv4 地址，用于提示其他设备如何访问。
func lanIPs() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			out = append(out, n.IP.String())
		}
	}
	return out
}

func defaultClientDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "Veilfin")
	}
	return "./client-data"
}

func runKeyInfo(args []string) error {
	fs := flag.NewFlagSet("keyinfo", flag.ExitOnError)
	data := fs.String("data", envOr("VF_DATA", "./data"), "服务端数据目录")
	fs.Parse(args)
	id, _, err := server.LoadKeys(*data)
	if err != nil {
		return err
	}
	pub := id.Public().(ed25519.PublicKey)
	fmt.Println("公钥:", base64.StdEncoding.EncodeToString(pub))
	fmt.Println("指纹:", proto.Fingerprint(pub))
	return nil
}

// readSecret 从环境变量或终端（不回显）读取口令。
func readSecret(prompt, env string, confirm bool) (string, error) {
	if v := os.Getenv(env); v != "" {
		return v, nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if confirm {
		fmt.Fprint(os.Stderr, "再输入一次：")
		b2, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(b) != string(b2) {
			return "", errors.New("两次输入不一致")
		}
	}
	return string(b), nil
}

func runBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	data := fs.String("data", envOr("VF_DATA", "./data"), "服务端数据目录")
	out := fs.String("out", "", "备份文件路径（默认 veilfin-日期.vfbak）")
	noAssets := fs.Bool("no-assets", false, "不备份图片 / 字幕 / 预览图缓存（可通过重新扫描生成，但手动设置的封面会丢失）")
	fs.Parse(args)
	if *out == "" {
		*out = "veilfin-" + time.Now().Format("20060102-150405") + ".vfbak"
	}
	pass, err := readSecret("设置备份口令（至少 8 位）：", "VF_BACKUP_PASS", true)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	if err := backup.Create(ctx, *data, *out, pass, !*noAssets); err != nil {
		return err
	}
	st, _ := os.Stat(*out)
	log.Printf("备份完成：%s（%.1f MB）。请把备份文件和口令分开保存。", *out, float64(st.Size())/(1<<20))
	return nil
}

func runRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	data := fs.String("data", envOr("VF_DATA", "./data"), "要恢复到的数据目录（恢复前请先停止服务端）")
	in := fs.String("in", "", "备份文件路径（必填）")
	force := fs.Bool("force", false, "覆盖目标目录中已有的密钥和数据库")
	fs.Parse(args)
	if *in == "" {
		return errors.New("必须用 -in 指定备份文件")
	}
	pass, err := readSecret("备份口令：", "VF_BACKUP_PASS", false)
	if err != nil {
		return err
	}
	if err := backup.Restore(*in, *data, pass, *force); err != nil {
		return err
	}
	log.Printf("恢复完成：%s", *data)
	return nil
}

func runUser(args []string) error {
	if len(args) == 0 || args[0] != "reset-password" {
		return errors.New("用法：veilfin user reset-password -name 用户名 [-data 数据目录]")
	}
	fs := flag.NewFlagSet("user", flag.ExitOnError)
	data := fs.String("data", envOr("VF_DATA", "./data"), "服务端数据目录")
	name := fs.String("name", "", "用户名（必填）")
	fs.Parse(args[1:])
	if *name == "" {
		return errors.New("必须用 -name 指定用户名")
	}
	d, err := db.Open(db.PathIn(*data))
	if err != nil {
		return err
	}
	defer d.Close()
	ctx := context.Background()
	u, err := d.UserByName(ctx, *name)
	if err != nil {
		return fmt.Errorf("找不到用户 %s", *name)
	}
	pass, err := readSecret("新密码（至少 8 位）：", "VF_NEW_PASSWORD", true)
	if err != nil {
		return err
	}
	if err := auth.ValidatePassword(pass); err != nil {
		return err
	}
	if err := d.SetPassword(ctx, u.ID, auth.Hash(pass)); err != nil {
		return err
	}
	if _, err := d.W.ExecContext(ctx, "UPDATE users SET disabled=0 WHERE id=?", u.ID); err != nil {
		return err
	}
	log.Printf("已重置 %s 的密码，并解除锁定 / 禁用状态", u.Username)
	return nil
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("自动打开浏览器失败，请手动访问 %s", url)
	}
}
