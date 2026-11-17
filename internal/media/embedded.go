package media

// 内嵌 ffmpeg：用 -tags ffmpeg_embed 构建时，程序里带一个精简的静态 ffmpeg / ffprobe（LGPL），
// 系统里找不到 ffmpeg 时才解压到数据目录下使用，之后直接复用。普通构建不带，bundled 为空。
//
// 内嵌包是 tar.gz，由 scripts/build-ffmpeg.sh 生成，包含：
//
//	VERSION              ffmpeg 版本号
//	ffmpeg[.exe]         ffprobe[.exe]
//	LICENSE.txt          LGPL 许可证
//	BUILD.txt            源码地址、校验和与编译参数

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// bundled 是编译时内嵌的 ffmpeg 包，只在 ffmpeg_embed 构建中由 embed_ffmpeg.go 赋值。
var bundled []byte

// HasEmbedded 报告程序是否内置了 ffmpeg。
func HasEmbedded() bool { return len(bundled) > 0 }

var (
	embVerOnce sync.Once
	embVer     string
)

// EmbeddedVersion 返回内置 ffmpeg 的版本号，没有内置时返回空字符串。
func EmbeddedVersion() string {
	embVerOnce.Do(func() {
		if !HasEmbedded() {
			return
		}
		_ = walkBundle(func(name string, r io.Reader) error {
			if name == "VERSION" {
				b, _ := io.ReadAll(io.LimitReader(r, 64))
				embVer = strings.TrimSpace(string(b))
				return errStopWalk
			}
			return nil
		})
	})
	return embVer
}

var errStopWalk = errors.New("stop")

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// bundleFiles 是允许从内嵌包中解出的文件（其他条目一律忽略）。
func bundleFiles() map[string]bool {
	return map[string]bool{"VERSION": true, "LICENSE.txt": true, "BUILD.txt": true, exeName("ffmpeg"): true, exeName("ffprobe"): true}
}

func walkBundle(fn func(name string, r io.Reader) error) error {
	gz, err := gzip.NewReader(bytes.NewReader(bundled))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || !bundleFiles()[hdr.Name] {
			continue
		}
		if err := fn(hdr.Name, tr); err != nil {
			if err == errStopWalk {
				return nil
			}
			return err
		}
	}
}

// ExtractEmbedded 把内置的 ffmpeg 解压到 root 下的版本目录并返回该目录。
// 目录名包含内嵌包的哈希，升级程序后自动换新目录并清理旧的；已解压的文件每次启动都会校验，损坏时重新解压。
func ExtractEmbedded(root string) (string, error) {
	if !HasEmbedded() {
		return "", errors.New("程序没有内置 ffmpeg")
	}
	sum := sha256.Sum256(bundled)
	name := "ffmpeg-" + EmbeddedVersion() + "-" + hex.EncodeToString(sum[:6])
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	if !verifyExtracted(dir) {
		os.RemoveAll(dir)
		tmp, err := os.MkdirTemp(root, ".ffmpeg-*")
		if err != nil {
			return "", err
		}
		if err := extractTo(tmp); err != nil {
			os.RemoveAll(tmp)
			return "", fmt.Errorf("解压内置 ffmpeg 失败: %w", err)
		}
		if err := os.Rename(tmp, dir); err != nil {
			os.RemoveAll(tmp)
			if !verifyExtracted(dir) { // 可能是另一个进程同时解压完成了
				return "", err
			}
		}
	}
	// 清理旧版本和中断的解压
	if old, _ := filepath.Glob(filepath.Join(root, "ffmpeg-*")); len(old) > 0 {
		for _, o := range old {
			if filepath.Base(o) != name {
				os.RemoveAll(o)
			}
		}
	}
	if tmps, _ := filepath.Glob(filepath.Join(root, ".ffmpeg-*")); len(tmps) > 0 {
		for _, t := range tmps {
			if st, err := os.Stat(t); err == nil && time.Since(st.ModTime()) > time.Hour {
				os.RemoveAll(t)
			}
		}
	}
	// 确认能执行（数据目录所在的分区可能挂载为 noexec）
	for _, n := range []string{"ffmpeg", "ffprobe"} {
		if _, err := run(context.Background(), 20*time.Second, nil, filepath.Join(dir, exeName(n)), "-version"); err != nil {
			return "", fmt.Errorf("内置 %s 无法运行（数据目录所在分区是否禁止执行程序？可用 -ffmpeg 指定其他 ffmpeg）: %w", n, err)
		}
	}
	return dir, nil
}

const sumFile = ".sha256"

func extractTo(dir string) error {
	var sums strings.Builder
	err := walkBundle(func(name string, r io.Reader) error {
		mode := os.FileMode(0o644)
		if strings.HasPrefix(name, "ffmpeg") || strings.HasPrefix(name, "ffprobe") {
			mode = 0o755
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		h := sha256.New()
		if _, err := io.Copy(io.MultiWriter(f, h), r); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%s %s\n", hex.EncodeToString(h.Sum(nil)), name)
		return nil
	})
	if err != nil {
		return err
	}
	for _, n := range []string{"ffmpeg", "ffprobe"} {
		if _, err := os.Stat(filepath.Join(dir, exeName(n))); err != nil {
			return fmt.Errorf("内置包中缺少 %s", exeName(n))
		}
	}
	return os.WriteFile(filepath.Join(dir, sumFile), []byte(sums.String()), 0o644)
}

// verifyExtracted 用解压时记录的哈希校验已解压的文件。
func verifyExtracted(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, sumFile))
	if err != nil {
		return false
	}
	seen := 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		want, name, ok := strings.Cut(line, " ")
		if !ok || !bundleFiles()[name] {
			return false
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return false
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil || hex.EncodeToString(h.Sum(nil)) != want {
			return false
		}
		if name == exeName("ffmpeg") || name == exeName("ffprobe") {
			seen++
		}
	}
	return seen == 2
}
