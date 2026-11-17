# 内嵌 ffmpeg

`ffmpeg.tar.gz` 是用 `-tags ffmpeg_embed` 构建“内置 ffmpeg 版”时嵌入程序的精简 ffmpeg / ffprobe（LGPL），
不提交到仓库。发布流程（`.github/workflows/ffmpeg.yml`）会为每个平台生成它；本地需要时运行：

```bash
# Linux（在 Alpine 中编译出静态程序）
docker run --rm -v "$PWD":/w -w /w alpine:3.22 sh scripts/build-ffmpeg.sh linux-amd64 internal/media/ffbin/ffmpeg.tar.gz
# Windows（在装有 mingw-w64 和 nasm 的 Linux 上交叉编译）
sh scripts/build-ffmpeg.sh windows-amd64 internal/media/ffbin/ffmpeg.tar.gz

go build -tags ffmpeg_embed ./cmd/veilfin
```

注意：包里的程序必须和 `GOOS/GOARCH` 是同一个平台。
