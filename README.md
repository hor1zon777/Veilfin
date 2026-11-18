# Veilfin（帷影）

类似 Emby 的个人视频服务。**所有请求、响应、视频、图片和字幕都在应用层做了端到端加密**，即使服务端部署在 CDN 之后，CDN 也只能看到密文，没法审查内容。

- 单个可执行文件（约 13 MB），只依赖 ffmpeg；也有内置精简 ffmpeg 的服务端版本，完全免安装；数据库是内置的 SQLite
- 多用户、媒体库权限、受限媒体库加 PIN 锁
- 观看进度与上次观看时间、自动续播（可一键从头播放）、继续观看、观看记录、播放统计
- 随机播放：按当前媒体库 / 文件夹 / 筛选条件打乱生成播放队列，播完自动接下一个
- 元数据：NFO / JSON 附加文件 / 同目录图片 → MetaTube（番号）/ TMDB（电影）→ 手动编辑（编辑过的字段会被锁定）
- 自动截图做封面，可以在视频里挑一帧做海报；外挂和内嵌字幕（自动识别 GBK 编码）；进度条预览图
- 播放时切换画质：原画直接播放；网速慢或浏览器不支持原始编码（例如 HEVC）时，可以选 1080P / 720P / 480P / 360P，由服务端实时转码（内置 ffmpeg 的版本自带 OpenH264；普通版需要系统 ffmpeg 带 H.264 编码器，见下文）
- 目录监听、定时扫描、文件夹浏览、搜索
- 组合筛选：观看状态（未看 / 观看中 / 已看）、分辨率、时长、年份、类型、标签、演员、制作、系列
- 加密备份 / 恢复：在管理后台创建、下载、上传备份，一键在线恢复（服务端在同一进程内自动重启）；也可以用命令行

## 架构

```
                    ┌───────────── 你的电脑 ─────────────┐
 本地客户端模式：   浏览器 ──明文(不出本机)──► veilfin client ═══╗
                    └────────────────────────────────────┘          ║
                                                                     ║ 密文
 网页模式（手机）：  浏览器（JS 加解密 + Service Worker 解密视频）═══╣
                                                                     ▼
                                                   CDN（只看到密文）═══► veilfin server ──► 媒体目录
```

| 模式 | 适用 | 前端代码从哪来 | CDN 能否篡改代码 |
|---|---|---|---|
| **本地客户端**（推荐） | Windows / Mac / Linux 电脑 | 编译在客户端程序里 | 不能 |
| 网页模式 | 手机、平板、临时设备 | 服务端下发 | 前端走 CDN 时理论上可以；用 `-api-base` 让前端不走 CDN 就不能 |

## 加密设计

| 环节 | 算法 |
|---|---|
| 握手 | **X25519 + ML-KEM-768** 混合密钥交换（后量子，防“先存后解”），Ed25519 服务端身份签名，HKDF-SHA256 |
| API | **XChaCha20-Poly1305 内层 + AES-256-GCM 外层**，所有接口都走同一个入口 `POST /x`，按 1 KiB 填充长度 |
| 前向安全 | 两个方向各有一个哈希棘轮，每 500 条消息或 5 分钟换一代密钥，旧密钥用完即销毁；每 24 小时重新握手 |
| 防重放 | 请求带时间戳（±60 秒）和随机 ID，重复的请求会被拒绝 |
| 媒体 | 每个对象一把独立密钥，1 MiB 分块 AES-256-GCM；nonce 由“块序号 + 内容”经 HMAC 派生（SIV 思路，原地改文件也不会重用 nonce）；AAD 绑定对象 ID、块序号和“是否最后一块”，可防篡改、防调换、防截断 |
| 媒体访问 | 对象 ID 是随机数；必须带短期令牌（绑定用户和解锁状态）才能取到密文；密文是确定的，CDN 可以缓存 |
| 密码 | Argon2id（64 MiB，2 轮） |

CDN 能看到的只有：访问 IP、时间、请求大小，以及 `/h`、`/x`、`/m/{随机ID}` 这几个不透明路径。

## 快速开始

### 1. 服务端（源站）

用 Docker：

```bash
docker compose up -d
docker compose logs veilfin     # 记下“服务端公钥”和“公钥指纹”
```

或者直接运行。Release 里的 `veilfin-server-full-*`（Linux x64 / ARM64、Windows x64）内置了精简编译的 ffmpeg，下载即可运行；普通版需要先安装 ffmpeg：

```bash
./veilfin-server-full-linux-amd64 server -media /path/to/videos
```

查找 ffmpeg 的顺序是：`-ffmpeg` 参数 → 系统里已安装的（PATH）→ 内置版本。只有系统里找不到时，内置的 ffmpeg 才会解压到数据目录的 `bin/` 下使用（数据目录所在分区不能禁止执行程序）。

**播放时转码**需要 ffmpeg 带 H.264 编码器：libx264（CPU），NVIDIA NVENC / Intel Quick Sync / AMD AMF / Apple VideoToolbox 硬件编码，或者 OpenH264。内置的精简 ffmpeg 为了保持 LGPL 不用 x264，带的是 OpenH264（Cisco，BSD 许可），开箱即可转码，但同样码率下画质不如 libx264；想要更好的画质，可以安装系统自带的完整 ffmpeg（例如 `apt install ffmpeg`，系统里的 ffmpeg 优先使用），或者用 `-ffmpeg` 指定。服务端启动后会逐个试编码，确认哪些编码器真的可用（硬件编码器编进了 ffmpeg，但没有显卡或驱动时不可用），自动选择时 OpenH264 排在最后。管理后台的“服务器设置 → 播放时转码”里可以开关这个功能、选择编码器、限制同时转码的播放数（默认 2 路）。

转码的工作方式：ffmpeg 从播放位置开始把视频转成分片 MP4，服务端按 4 秒一段切成 HLS 分片，每段单独加密后下发（不缓存），浏览器用 hls.js 播放（iOS 17.1 以上用 ManagedMediaSource）。分片的时间戳就是源视频里的时间，所以进度、字幕、预览图和续播都不受影响。拖动到还没转码的位置时，服务端从新位置重新开始转码；转出的内容领先播放位置约 2 分钟后暂停 ffmpeg；暂停播放 1 分钟后结束 ffmpeg，20 分钟后删除临时文件（在数据目录的 `transcode/` 下）。

启动日志会打印两样东西：

- **服务端公钥**：客户端用它来固定信任
- **公钥指纹**：给人工核对用

首次启动后，用浏览器打开服务（或本地客户端）会进入**设置向导**：创建管理员 → 服务器设置 → 添加媒体库。为防止公网上的其他人抢先完成初始化，服务端只在**启动后 30 分钟内**接受初始化；超时后重启服务端即可重新计时。

`data/keys/` 下的两个密钥文件**丢了就没法恢复**，请在「管理后台 → 备份与恢复」或用 `veilfin backup` 定期备份，并把备份下载到别处保存。服务端公钥可以在「管理后台 → 服务器设置」一键复制，那里还有现成的客户端启动命令。

CDN 回源到 8097 端口即可。如果 CDN 到源站也想走 HTTPS，可以用 `deploy/Caddyfile`，或者给服务端加 `-tls-cert` / `-tls-key` 参数。

### 2. 本地客户端（电脑上看，推荐）

```bash
veilfin client -server https://你的CDN域名 -server-key '服务端公钥'
```

启动后会自动打开浏览器，地址是 `http://127.0.0.1:8096`。服务端还没初始化时，第一次访问会进入设置向导。

客户端默认监听 `0.0.0.0:8096`。局域网里的其他设备可以直接用 `http://电脑的IP:8096` 访问，也可以放在反向代理后面。**每个浏览器都要用自己的账号登录**，登录状态通过 HttpOnly Cookie 和浏览器绑定，一个人登录不会让别的设备也处于登录状态。

> 注意：本地客户端输出的是**解密后的明文**。从客户端到浏览器这一段，如果要经过公网或 CDN，请在反向代理上启用 HTTPS，并且这一段不要套 CDN，否则前面所有的端到端加密就白费了。需要从外网访问时，更推荐用服务端的网页模式：加密一直延续到浏览器里。

### 3. 网页模式（手机上看）

网页模式默认开启，直接在手机浏览器打开 `https://你的域名` 即可（只想用本地客户端时，可以用 `-web=false` 或 `VF_WEB=0` 关闭）。要求：

- 必须是 HTTPS。Service Worker 和 WebCrypto 只能在安全上下文中使用
- Firefox 的隐私模式用不了，因为它禁用了 Service Worker

**强隔离部署**：前端页面走一个不经过 CDN 的子域名（DNS 直接指向源站），API 和视频走 CDN。这样 CDN 就没有机会篡改前端代码：

```bash
veilfin server -api-base https://cdn.example.com -web-origin https://app.example.com
```

### 4. 添加媒体库

进入「管理后台 → 媒体库」，选择类型和目录：

| 类型 | 展示 | 元数据 |
|---|---|---|
| 自制视频 | 横版缩略图 | 读取 `<文件名>.json` / `.nfo` / 同名图片；没有封面时自动截图 |
| 电影 | 竖版海报 | NFO 优先，其次 TMDB（在“服务器设置”填 API Key） |
| 日本影片 | 竖版海报 | 从文件名识别番号（`ABC-123`、`FC2-PPV-xxx`、`-C` 中文字幕、`CD1` 分段等），通过 MetaTube 刮削 |

勾选「受限」后，这个库只对授权用户可见；用户设置了 PIN 的话，还要先解锁才能看到，包括首页、搜索和视频数据本身。

自制视频可以在旁边放一个同名 `.json` 文件来提供元数据：

```json
{ "title": "2023 年家庭聚会", "overview": "…", "date": "2023-10-01", "tags": ["家庭"], "actors": ["爷爷", "奶奶"] }
```

### 5. 在线更新

「管理后台 → 系统更新」可以检查 GitHub Release 上的新版本，一键下载并更新服务端：

1. 服务端下载与本机平台对应的程序（内置 ffmpeg 的版本会继续选内置 ffmpeg 的版本），用发布附带的 `SHA256SUMS.txt` 校验；
2. 试运行 `新程序 version`，确认能在本机运行且版本号一致；
3. 替换程序文件，旧程序保留为同目录下的 `veilfin.old`（Windows 为 `veilfin.old.exe`），出问题时停止服务端换回即可；
4. 关闭服务端，用新程序重新启动。Linux / macOS 上是原地替换进程（进程号不变，systemd 等进程管理不受影响）；Windows 上会启动新进程后退出旧进程。

服务端默认每 12 小时检查一次更新（只提示，不会自动安装），可以在「服务器设置」里关闭。访问 GitHub 使用「服务器设置 → 网络」里的代理，未设置时使用 `HTTPS_PROXY` 环境变量。以下情况不能在线更新，页面上会给出说明：

- 运行在 Docker 容器中：容器重建后替换的程序会丢失，请 `git pull && docker compose up -d --build`；
- 运行服务端的用户没有程序所在目录的写权限（例如程序放在 `/usr/local/bin` 而服务以普通用户运行）。

本地客户端不会随服务端更新，需要自行下载新版本。从其他仓库（例如自己的 fork）检查更新时，设置环境变量 `VF_UPDATE_REPO=用户名/仓库名`。

## 命令

```
veilfin server   [-listen 0.0.0.0:8097] [-data ./data] [-media 目录] [-web=false] [-api-base URL] [-web-origin URL] [-ffmpeg 目录] [-tls-cert/-tls-key]
veilfin client   -server URL [-server-key 公钥] [-listen 0.0.0.0:8096] [-open=false]
veilfin keyinfo  [-data ./data]                      显示服务端公钥和指纹
veilfin backup   [-data ./data] [-out 文件] [-no-assets]   加密备份（口令用 Argon2id 派生，XChaCha20-Poly1305 加密）
veilfin restore  -in 文件 [-data ./data] [-force]     恢复（先停止服务端）
veilfin user reset-password -name 用户名 [-data ./data]   忘记密码时重置
```

服务端的选项都可以用环境变量设置：`VF_LISTEN`、`VF_DATA`、`VF_MEDIA`、`VF_WEB=0`（关闭网页模式）、`VF_API_BASE`、`VF_WEB_ORIGIN`、`VF_FFMPEG`；`VF_UPDATE_REPO` 指定检查更新的仓库。

## 构建

```bash
sh build.sh          # 先构建前端，再交叉编译 Windows / Linux / macOS 到 dist/
```

Node 只在构建前端时需要，运行时不需要。仓库里已经带有构建好的前端（`internal/webui/dist`），所以只改 Go 代码时直接 `go build ./cmd/veilfin` 就行。

### 内置 ffmpeg 的版本

`scripts/build-ffmpeg.sh` 从源码静态编译一个精简的 ffmpeg / ffprobe：只包含用到的解析器、解码器（H.264 / HEVC / VP8 / VP9 / MPEG-4）、MJPEG 编码器、字幕组件和几个滤镜，以及播放时转码用的 OpenH264（从源码静态编译）、AAC 编码器和 MP4 复用器。许可证是 LGPL 2.1，不含 GPL 组件；OpenH264 是 BSD 2-Clause。注意 Cisco 代付 H.264 专利费只适用于它官方发布的 OpenH264 二进制，不适用于从源码编译、静态链接的版本。生成的包放到 `internal/media/ffbin/ffmpeg.tar.gz`，再加上 `-tags ffmpeg_embed` 构建即可，详见 `internal/media/ffbin/README.md`。

`internal/media` 的测试会用 `testdata/` 里的样片，把截图、进度条预览图、内嵌和外挂字幕都跑一遍：普通构建测试系统 ffmpeg，`-tags ffmpeg_embed` 构建测试内置版本。

### 发布

推送 `v` 开头的标签会触发 GitHub Actions（`.github/workflows/release.yml`）：先跑 Go 测试和浏览器端加密互通测试，同时在三个平台上编译精简 ffmpeg，并在各自的平台上用样片测试（`.github/workflows/ffmpeg.yml`）；然后构建前端，交叉编译 Windows / Linux / macOS / FreeBSD 共 8 个平台的普通版，以及 3 个内置 ffmpeg 的服务端版本，生成 `SHA256SUMS.txt`，最后发布到 Release。

```bash
git tag v1.0.0 && git push origin v1.0.0
```

也可以在 Actions 页面手动运行 Release 工作流并填写版本号。标签名里带 `-`（例如 `v1.1.0-beta.1`）会标记为预发布。

## 测试

```bash
go test ./...                              # 单元测试和端到端测试（媒体相关的测试需要 ffmpeg）
cd web && npm run build
node tests/crypto-interop.mjs              # 浏览器端 JS 加密实现与 Go 服务端的互通测试
node tests/browser.mjs                     # 真实浏览器（Edge/Chrome 无头）测试两种模式的完整流程，截图存在 tests/screenshots/
```

端到端测试中有一个模拟的 CDN 中间层，会记录所有经过的 URL、请求头、请求体和响应体。测试会断言其中找不到任何片名、简介、演员、字幕文本、密码、PIN、接口名，以及视频和图片的原始字节。

## 已知限制

- 原画只支持浏览器能直接播放的格式（MP4/H.264、WebM），HEVC 视频要看浏览器是否支持硬解；不支持时，如果服务端能转码，播放页会自动改为转码播放。
- 转码不做 HDR 色调映射，HDR 片源转码后颜色会偏淡；转码只输出 H.264 + AAC 立体声。
- 没有电视剧的“季 / 集”层级。同一文件夹里的视频可以用“下一个”连续播放，也可以按文件夹浏览。
- MetaTube 的接口是按它公开的 HTTP API 实现的，只用模拟服务测试过，还没有对接过真实的 MetaTube 服务。
- 网页模式下，CDN 能看到访问时间、IP 和流量大小。如果有人知道某部公开影片的确切文件大小，理论上可以据此猜出你看的是哪部。
