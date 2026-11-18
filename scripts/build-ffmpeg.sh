#!/bin/sh
# 构建精简的静态 ffmpeg / ffprobe（LGPL 2.1，不含任何 GPL 组件），打包成 tar.gz，
# 供 `go build -tags ffmpeg_embed` 内嵌到“内置 ffmpeg 版”服务端程序里。
#
# 用法：sh scripts/build-ffmpeg.sh <linux-amd64|linux-arm64|windows-amd64> <输出文件.tar.gz>
#
#   linux-*        在对应架构的 Alpine（musl）环境中运行，得到完全静态链接的程序，例如：
#                  docker run --rm -v "$PWD":/w -w /w alpine:3.22 sh scripts/build-ffmpeg.sh linux-amd64 out.tar.gz
#   windows-amd64  在装有 mingw-w64（gcc 和 g++）、nasm、pkg-config 的 Linux 上交叉编译
#
# 只启用 Veilfin 用到的功能：读取 mp4/mov/webm/mkv、解码 H.264/HEVC/VP8/VP9/MPEG-4、
# 截图（MJPEG）、进度条预览图（scale/fps/tile 滤镜）、内嵌与外挂文本字幕转 WebVTT，
# 以及播放时转码：OpenH264（Cisco，BSD 2-Clause，与 LGPL 兼容）编码 H.264、AAC 编码音频、输出分片 MP4。
# 不用 x264（GPL），整个程序仍是 LGPL。
set -eu

FFMPEG_VERSION=9.0.2
FFMPEG_SHA256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e
FFMPEG_URL="https://ffmpeg.org/releases/ffmpeg-$FFMPEG_VERSION.tar.xz"

OPENH264_VERSION=2.7.0
OPENH264_SHA256=5178775c8b440073e70920e97d195efff721c0e56d0a78cc82c919e95d54924d
OPENH264_URL="https://github.com/cisco/openh264/archive/refs/tags/v$OPENH264_VERSION.tar.gz"

target=${1:?缺少目标平台}
out=${2:?缺少输出文件}
case $out in /*) ;; *) out="$PWD/$out" ;; esac

PROTOCOLS="file pipe"
DEMUXERS="mov matroska srt ass webvtt"
MUXERS="image2 webvtt mp4"
DECODERS="h264 hevc vp8 vp9 mpeg4 mjpeg aac mp3float opus vorbis ac3 eac3 flac movtext subrip srt ass ssa webvtt text"
ENCODERS="mjpeg webvtt libopenh264 aac"
PARSERS="h264 hevc vp8 vp9 mpeg4video mjpeg aac mpegaudio opus vorbis ac3 flac"
# 转码用到：精确跳转（trim / atrim）、像素格式和声道转换（format / aformat / aresample）、没有滤镜时的直通（null / anull）
FILTERS="scale fps tile format aformat aresample trim atrim null anull"

join() { echo "$*" | tr ' ' ','; }

case $target in
linux-amd64 | linux-arm64)
  if command -v apk >/dev/null; then
    apk add --no-cache build-base nasm curl xz tar file pkgconf >/dev/null
  fi
  exe=""
  extra="--enable-pthreads --extra-ldflags=-static"
  if [ "$target" = linux-amd64 ]; then
    extra="$extra --arch=x86_64"
    h264_make="OS=linux ARCH=x86_64"
  else
    extra="$extra --arch=aarch64"
    h264_make="OS=linux ARCH=aarch64"
  fi
  ;;
windows-amd64)
  exe=".exe"
  extra="--enable-cross-compile --target-os=mingw32 --arch=x86_64 --cross-prefix=x86_64-w64-mingw32- \
    --enable-w32threads --extra-ldflags=-static"
  h264_make="OS=mingw_nt ARCH=x86_64 CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++ AR=x86_64-w64-mingw32-ar"
  ;;
*)
  echo "不支持的目标平台：$target" >&2
  exit 2
  ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"
deps="$work/deps"

echo ">> 下载 $OPENH264_URL"
curl -fsSL --retry 3 -o openh264.tar.gz "$OPENH264_URL"
echo "$OPENH264_SHA256  openh264.tar.gz" | sha256sum -c -
tar -xzf openh264.tar.gz
echo ">> 编译 OpenH264 $OPENH264_VERSION（静态库）"
# shellcheck disable=SC2086
make -C "openh264-$OPENH264_VERSION" -j"$(nproc)" $h264_make PREFIX="$deps" install-static >openh264.log 2>&1 || {
  tail -80 openh264.log
  exit 1
}
ls -l "$deps/lib"
cat "$deps/lib/pkgconfig/openh264.pc"
# 只让 configure 看到这里编译的库（交叉编译时不能用到宿主机的）
export PKG_CONFIG_LIBDIR="$deps/lib/pkgconfig"
export PKG_CONFIG_PATH="$deps/lib/pkgconfig"

echo ">> 下载 $FFMPEG_URL"
curl -fsSL --retry 3 -o src.tar.xz "$FFMPEG_URL"
echo "$FFMPEG_SHA256  src.tar.xz" | sha256sum -c -
tar -xJf src.tar.xz
cd "ffmpeg-$FFMPEG_VERSION"

configure_args="--disable-everything --disable-autodetect --disable-network --disable-doc --disable-debug
  --disable-ffplay --disable-avdevice --disable-shared --enable-static
  --pkg-config=pkg-config --pkg-config-flags=--static --enable-libopenh264
  --enable-protocol=$(join $PROTOCOLS) --enable-demuxer=$(join $DEMUXERS) --enable-muxer=$(join $MUXERS)
  --enable-decoder=$(join $DECODERS) --enable-encoder=$(join $ENCODERS) --enable-parser=$(join $PARSERS)
  --enable-filter=$(join $FILTERS) $extra"
echo ">> configure $configure_args"
# shellcheck disable=SC2086
./configure $configure_args >configure.log 2>&1 || {
  tail -50 configure.log
  tail -80 ffbuild/config.log
  exit 1
}
grep -i "warning" configure.log || true

# configure 对拼错的组件名只给警告，这里逐个确认真的启用了
missing=""
check() { grep -q "^#define CONFIG_$(echo "$1_$2" | tr 'a-z' 'A-Z') 1" config_components.h || missing="$missing $1:$2"; }
for x in $PROTOCOLS; do check "$x" protocol; done
for x in $DEMUXERS; do check "$x" demuxer; done
for x in $MUXERS; do check "$x" muxer; done
for x in $DECODERS; do check "$x" decoder; done
for x in $ENCODERS; do check "$x" encoder; done
for x in $PARSERS; do check "$x" parser; done
for x in $FILTERS; do check "$x" filter; done
if [ -n "$missing" ]; then
  echo "以下组件没有启用：$missing" >&2
  exit 1
fi
grep -E "^#define HAVE_(PTHREADS|W32THREADS|THREADS) " config.h
grep -q "^#define HAVE_THREADS 1" config.h || { echo "没有启用多线程" >&2; exit 1; }
grep -E "^#define CONFIG_(GPL|NONFREE|VERSION3) " config.h
grep -q "^#define CONFIG_GPL 0" config.h || { echo "不应包含 GPL 组件" >&2; exit 1; }

echo ">> 编译"
make -j"$(nproc)" >make.log 2>&1 || {
  tail -80 make.log
  exit 1
}

pkg="$work/pkg"
mkdir -p "$pkg"
cp "ffmpeg$exe" "ffprobe$exe" "$pkg/"
{
  cat COPYING.LGPLv2.1
  printf '\n\n==============================================================================\n'
  printf 'OpenH264 %s（https://github.com/cisco/openh264）\n\n' "$OPENH264_VERSION"
  cat "../openh264-$OPENH264_VERSION/LICENSE"
} >"$pkg/LICENSE.txt"
echo "$FFMPEG_VERSION" >"$pkg/VERSION"
cat >"$pkg/BUILD.txt" <<EOF
FFmpeg $FFMPEG_VERSION，许可证 LGPL 2.1（见 LICENSE.txt），不含 GPL / nonfree 组件。
源码：$FFMPEG_URL
SHA256：$FFMPEG_SHA256
静态链接 OpenH264 $OPENH264_VERSION（BSD 2-Clause，见 LICENSE.txt），从源码编译。
注意：Cisco 代付 H.264 专利许可费只适用于 Cisco 官方发布的 OpenH264 二进制，不适用于这里从源码编译的版本。
源码：$OPENH264_URL
SHA256：$OPENH264_SHA256
构建脚本：https://github.com/hor1zon777/Veilfin/blob/main/scripts/build-ffmpeg.sh
目标平台：$target
configure 参数：
$(echo "$configure_args" | tr -s ' \n' ' ')
EOF
ls -l "$pkg"
if [ -z "$exe" ]; then
  file "$pkg/ffmpeg" "$pkg/ffprobe" || true
  "$pkg/ffmpeg" -hide_banner -version | head -3
  "$pkg/ffmpeg" -hide_banner -encoders | grep -E "libopenh264|aac"
else
  x86_64-w64-mingw32-objdump -p "$pkg/ffmpeg.exe" | grep "DLL Name" || true
fi

mkdir -p "$(dirname "$out")"
tar -C "$pkg" -cf - VERSION "ffmpeg$exe" "ffprobe$exe" LICENSE.txt BUILD.txt | gzip -9 >"$out"
ls -l "$out"
