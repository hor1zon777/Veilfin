// 真实浏览器端到端测试（Edge / Chrome，无头模式）。
// 用法：npm run build && node tests/browser.mjs
// 覆盖：网页模式（浏览器内加密 + Service Worker 解密）与本地客户端模式下的初始化、登录、浏览、
// 播放、拖动、字幕、进度条预览图、继续观看、管理后台页面、复制公钥、备份 / 下载 / 在线恢复。截图保存在 tests/screenshots/。
import { spawn, execFileSync } from 'node:child_process'
import { mkdtempSync, mkdirSync, writeFileSync, existsSync, rmSync, readFileSync, readdirSync, utimesSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import puppeteer, { KnownDevices } from 'puppeteer-core'

const here = dirname(fileURLToPath(import.meta.url))
const root = join(here, '..', '..')
const shots = join(here, 'screenshots')
mkdirSync(shots, { recursive: true })
const work = mkdtempSync(join(tmpdir(), 'vf-browser-'))
const exe = join(work, process.platform === 'win32' ? 'veilfin.exe' : 'veilfin')

const browsers = [
  process.env.BROWSER,
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
].filter(Boolean)
const browserPath = browsers.find((p) => existsSync(p))
if (!browserPath) {
  console.error('找不到 Edge / Chrome，可用 BROWSER 环境变量指定')
  process.exit(2)
}

let failed = 0
const ok = (cond, msg) => {
  console.log(cond ? '  ✓' : '  ✗', msg)
  if (!cond) failed++
  return cond
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

function ffmpeg(args) {
  execFileSync('ffmpeg', ['-v', 'error', '-y', ...args], { stdio: 'inherit' })
}

console.log('准备：编译、生成测试视频…')
execFileSync('go', ['build', '-o', exe, './cmd/veilfin'], { cwd: root, stdio: 'inherit' })
const media = join(work, 'media')
mkdirSync(join(media, '旅行'), { recursive: true })
// 1080p 测试视频，带关键帧间隔，模拟真实片源
ffmpeg(['-f', 'lavfi', '-i', 'testsrc2=size=1280x720:rate=30:duration=120', '-f', 'lavfi', '-i', 'sine=frequency=440:duration=120',
  '-c:v', 'libx264', '-preset', 'veryfast', '-g', '60', '-b:v', '2M', '-c:a', 'aac', '-movflags', '+faststart', join(media, '家庭聚会.mp4')])
writeFileSync(join(media, '家庭聚会.srt'), '1\n00:00:00,500 --> 00:01:59,000\n字幕测试文本\n')
writeFileSync(join(media, '家庭聚会.nfo'), '<movie><title>二零二三年家庭聚会</title><plot>一家人的聚会。</plot><year>2023</year><genre>家庭</genre><actor><name>爷爷</name></actor></movie>')
// moov 在文件尾的视频（拖动更依赖 Range）
ffmpeg(['-f', 'lavfi', '-i', 'testsrc=size=640x360:rate=25:duration=60', '-f', 'lavfi', '-i', 'sine=frequency=660:duration=60',
  '-c:v', 'libx264', '-preset', 'veryfast', '-g', '50', '-c:a', 'aac', join(media, '旅行', '海边.mp4')])
// 第二个目录：通过管理后台界面添加成受限媒体库
const media2 = join(work, 'media2')
mkdirSync(media2, { recursive: true })
ffmpeg(['-f', 'lavfi', '-i', 'testsrc=size=640x360:rate=25:duration=20', '-c:v', 'libx264', '-preset', 'ultrafast', '-movflags', '+faststart', join(media2, 'ABC-123.mp4')])

// 服务端会跳过刚修改过（可能正在复制）的视频：把测试视频的修改时间调到一小时前
function age(dir) {
  const t = new Date(Date.now() - 3600 * 1000)
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, e.name)
    if (e.isDirectory()) age(p)
    else utimesSync(p, t, t)
  }
}
age(media)
age(media2)

const port = 19000 + Math.floor(Math.random() * 500)
const cport = port + 600
const data = join(work, 'data')
const procs = []
function start(args, waitFor) {
  return new Promise((resolve, reject) => {
    const p = spawn(exe, args)
    procs.push(p)
    let log = ''
    const t = setTimeout(() => reject(new Error('启动超时：\n' + log)), 30000)
    const on = (d) => {
      log += d
      const m = waitFor(log)
      if (m) {
        clearTimeout(t)
        resolve({ p, log, m })
      }
    }
    p.stdout.on('data', on)
    p.stderr.on('data', on)
  })
}

const srv = await start(['server', '-listen', `127.0.0.1:${port}`, '-data', data, '-media', media, '-web'], (log) =>
  log.includes('服务端监听') && log.includes('尚未初始化'),
)
const pubkey = srv.log.match(/服务端公钥: ([A-Za-z0-9+/=]+)/)[1]
let srvLog = srv.log
srv.p.stdout.on('data', (d) => (srvLog += d))
srv.p.stderr.on('data', (d) => (srvLog += d))

const browser = await puppeteer.launch({
  executablePath: browserPath,
  headless: true,
  args: ['--autoplay-policy=no-user-gesture-required', '--mute-audio', '--window-size=1440,900', '--no-first-run'],
  defaultViewport: { width: 1440, height: 900 },
})

async function newPage(ctx, label) {
  const page = await ctx.newPage()
  page.errors = []
  page.on('console', (m) => {
    if (m.type() === 'error') page.errors.push(`[${label}] console: ${m.text()}`)
  })
  page.on('pageerror', (e) => page.errors.push(`[${label}] pageerror: ${e.message}`))
  return page
}

async function shot(page, name) {
  await page.screenshot({ path: join(shots, name + '.png') })
}

// 等待首页出现视频卡片（媒体库扫描是异步的）
async function waitForCards(page, base) {
  for (let i = 0; i < 60; i++) {
    if (await page.$('.card')) return true
    await sleep(1000)
    if (i % 5 === 4) await page.goto(base + '/#/', { waitUntil: 'networkidle2' })
  }
  return false
}

// 播放测试：打开详情页 → 播放 → 检查进度推进 → 拖动 → 字幕 → 进度条预览图
async function playbackTest(page, base, label) {
  await page.goto(base + '/#/library/1?view=', { waitUntil: 'networkidle2' })
  // 只改 hash 的跳转不会重新加载页面：等媒体库页面自己的工具栏和卡片出现，避免读到上一页的内容
  await page.waitForSelector('.toolbar')
  await page.waitForSelector('.grid .card')
  const href = await page.$$eval('.grid .card', (cs) => cs.map((c) => [c.querySelector('.title').textContent, c.getAttribute('href')]))
  const party = href.find(([t]) => t.includes('家庭聚会'))
  if (!ok(!!party, `${label}：媒体库中有 NFO 标题的视频`)) {
    console.log('      页面上的卡片：', JSON.stringify(href), page.url())
    await shot(page, `${label}-library-debug`)
    throw new Error('找不到测试视频')
  }
  await page.goto(base + '/' + party[1], { waitUntil: 'networkidle2' })
  await page.waitForSelector('.detail .title')
  await sleep(800)
  await shot(page, `${label}-detail`)
  const overview = await page.$eval('.overview', (e) => e.textContent).catch(() => '')
  ok(overview.includes('一家人的聚会'), `${label}：详情页显示 NFO 简介`)
  const posterOk = await page.$eval('.poster img', (img) => img.complete && img.naturalWidth > 0).catch(() => false)
  ok(posterOk, `${label}：封面图片经解密后正常显示`)

  await page.click('.actions .btn.primary')
  await page.waitForSelector('video')
  const started = await page
    .waitForFunction(() => {
      const v = document.querySelector('video')
      return v && v.currentTime > 2 && !v.paused
    }, { timeout: 30000 })
    .then(() => true)
    .catch(() => false)
  ok(started, `${label}：视频开始播放并持续推进`)
  const info = await page.$eval('video', (v) => ({ w: v.videoWidth, h: v.videoHeight, d: v.duration }))
  ok(info.w === 1280 && info.h === 720 && Math.abs(info.d - 120) < 1, `${label}：解码出正确的画面尺寸和时长 ${info.w}×${info.h} ${info.d?.toFixed(1)}s`)

  // 拖动到 80 秒：只拉取需要的密文块
  await page.$eval('video', (v) => (v.currentTime = 80))
  const seeked = await page
    .waitForFunction(() => {
      const v = document.querySelector('video')
      return v.currentTime > 81 && !v.paused && v.readyState >= 3
    }, { timeout: 30000 })
    .then(() => true)
    .catch(() => false)
  ok(seeked, `${label}：拖动到 80 秒后继续播放`)

  // 连续快速拖动：前几次的上游请求被中止，最后的位置仍能正常播放
  await page.evaluate(async () => {
    const v = document.querySelector('video')
    for (const t of [10, 95, 30, 70, 20, 100]) {
      v.currentTime = t
      await new Promise((r) => setTimeout(r, 60))
    }
    v.currentTime = 55
  })
  const reseeked = await page
    .waitForFunction(() => {
      const v = document.querySelector('video')
      return v.currentTime > 56 && v.currentTime < 70 && !v.paused && v.readyState >= 3
    }, { timeout: 30000 })
    .then(() => true)
    .catch(() => false)
  ok(reseeked, `${label}：连续快速拖动后在最后的位置继续播放`)

  // 点击进度条：松开时只跳转一次，并跳到点击的位置
  {
    await page.evaluate(() => {
      const v = document.querySelector('video')
      window.__seeks = 0
      v.addEventListener('seeking', () => window.__seeks++)
    })
    const box = await (await page.$('.bar')).boundingBox()
    await page.mouse.click(box.x + box.width * 0.3, box.y + box.height / 2)
    const ratio = await page
      .waitForFunction(
        () => {
          const v = document.querySelector('video')
          return !v.seeking && v.readyState >= 3 && v.currentTime / v.duration
        },
        { timeout: 30000 },
      )
      .then((h) => h.jsonValue())
      .catch(() => 0)
    const seeks = await page.evaluate(() => window.__seeks)
    ok(ratio > 0.28 && ratio < 0.36 && seeks === 1, `${label}：点击进度条只跳转一次（${seeks} 次），到达 ${(ratio * 100).toFixed(1)}%`)
  }

  // 字幕：启用后应加载出字幕内容
  const cue = await page.evaluate(async () => {
    const v = document.querySelector('video')
    const tr = v.textTracks[0]
    if (!tr) return ''
    tr.mode = 'showing'
    for (let i = 0; i < 50 && !(tr.cues && tr.cues.length); i++) await new Promise((r) => setTimeout(r, 100))
    return tr.cues?.[0]?.text || ''
  })
  ok(cue === '字幕测试文本', `${label}：字幕经解密加载（“${cue}”）`)

  // 等待预览图生成后，悬停进度条
  let tp = false
  for (let i = 0; i < 20 && !tp; i++) {
    await page.reload({ waitUntil: 'networkidle2' })
    await page.waitForSelector('video')
    await sleep(500)
    const bar = await page.$('.bar')
    const box = await bar.boundingBox()
    await page.mouse.move(box.x + box.width * 0.4, box.y + box.height / 2)
    await sleep(600)
    tp = await page.evaluate(() => {
      const t = document.querySelector('.tip .thumb')
      const bg = t && getComputedStyle(t).backgroundImage
      return !!bg && (bg.includes('/b/') || bg.includes('blob:'))
    })
    if (!tp) await sleep(1500)
  }
  ok(tp, `${label}：进度条悬停显示预览图`)
  if (tp) {
    const loaded = await page.evaluate(
      () =>
        new Promise((resolve) => {
          const url = getComputedStyle(document.querySelector('.tip .thumb')).backgroundImage.slice(5, -2)
          const img = new Image()
          img.onload = () => resolve(img.naturalWidth > 0)
          img.onerror = () => resolve(false)
          img.src = url
        }),
    )
    ok(loaded, `${label}：预览图雪碧图解密成功`)
    // 开始播放后雪碧图在后台预载成 blob: URL，之后悬停直接用内存里的图片
    const pre = await page
      .waitForFunction(
        () => {
          const t = document.querySelector('.tip .thumb')
          return t && getComputedStyle(t).backgroundImage.includes('blob:')
        },
        { timeout: 10000 },
      )
      .then(() => true)
      .catch(() => false)
    ok(pre, `${label}：雪碧图已在后台预载到内存`)
  }
  await shot(page, `${label}-player`)

  // 播放一会儿后返回，首页应出现“继续观看”
  await page.$eval('video', (v) => (v.currentTime = 40))
  await sleep(3000)
  await page.click('.top .icon-btn')
  await sleep(1500)
  await page.goto(base + '/#/', { waitUntil: 'networkidle2' })
  await page.waitForSelector('.section-title')
  const rows = await page.$$eval('.section-title', (h) => h.map((x) => x.textContent))
  ok(rows.some((r) => r.includes('继续观看')), `${label}：首页出现“继续观看”`)
  await shot(page, `${label}-home`)
}

// 播放时切换画质（服务端实时转码成 HLS）：时间不跳、拖动、记住选择、切回原画
// 失败时打印播放器现场，方便定位
async function dumpPlayer(page) {
  const st = await page
    .evaluate(() => {
      const v = document.querySelector('video')
      const buf = []
      for (let i = 0; v && i < v.buffered.length; i++) buf.push([v.buffered.start(i).toFixed(1), v.buffered.end(i).toFixed(1)])
      return {
        t: v?.currentTime, rs: v?.readyState, paused: v?.paused, seeking: v?.seeking, h: v?.videoHeight, src: v?.src,
        buf, err: document.querySelector('.center-msg.err')?.textContent?.trim(), url: location.href,
      }
    })
    .catch((e) => String(e))
  console.log('      播放器状态：', JSON.stringify(st))
  console.log('      页面错误：', JSON.stringify(page.errors.slice(-5)))
  console.log('      服务端日志末尾：\n' + srvLog.split('\n').slice(-40).join('\n'))
}

async function transcodeTest(page, base, label) {
  await page.goto(base + '/#/library/1?view=', { waitUntil: 'networkidle2' })
  await page.waitForSelector('.grid .card')
  const href = await page.$$eval('.grid .card', (cs) => cs.map((c) => [c.querySelector('.title').textContent, c.getAttribute('href')]))
  const id = href.find(([t]) => t.includes('家庭聚会'))[1].split('/').pop()
  await page.evaluate(() => localStorage.removeItem('vf.quality'))
  await page.goto(base + `/#/play/${id}?t=30`, { waitUntil: 'networkidle2' })
  const playing = (cond, timeout = 40000) =>
    page
      .waitForFunction(cond, { timeout, polling: 200 })
      .then(() => true)
      .catch(() => false)
  await playing(() => {
    const v = document.querySelector('video')
    return v && v.currentTime > 31 && !v.paused && v.videoHeight === 720
  })
  const opts = await page.evaluate(async () => {
    document.querySelector('.q-btn')?.click()
    await new Promise((r) => setTimeout(r, 400))
    return [...document.querySelectorAll('.q-menu button')].map((b) => b.textContent.trim())
  })
  ok(opts.length === 4 && opts[0].startsWith('原画') && opts[1].startsWith('720P') && opts[3].startsWith('360P'),
    `${label}：画质菜单列出原画和 720P / 480P / 360P（${opts.join('，')}）`)
  const before = await page.$eval('video', (v) => v.currentTime)
  await page.evaluate(() => [...document.querySelectorAll('.q-menu button')].find((b) => b.textContent.trim().startsWith('480P')).click())
  const switched = await playing(() => {
    const v = document.querySelector('video')
    return v.videoHeight === 480 && !v.paused && v.readyState >= 3 && v.src.startsWith('blob:')
  })
  const after = await page.$eval('video', (v) => v.currentTime)
  ok(switched && Math.abs(after - before) < 8, `${label}：切换到 480P 转码播放，进度保持（${before.toFixed(1)}s → ${after.toFixed(1)}s）`)
  const label480 = await page.$eval('.q-label', (e) => e.textContent).catch(() => '')
  ok(label480 === '480P', `${label}：画质按钮显示当前画质（${label480}）`)

  // 拖动：服务端从新位置重新开始转码
  await page.$eval('video', (v) => (v.currentTime = 95))
  const seeked = await playing(() => {
    const v = document.querySelector('video')
    return v.currentTime > 96 && v.currentTime < 110 && !v.paused && v.readyState >= 3 && v.videoHeight === 480
  })
  if (!ok(seeked, `${label}：转码播放时拖动到 95 秒后继续播放`)) await dumpPlayer(page)

  // 记住画质：重新打开时直接转码播放，并从指定位置开始
  await page.goto(base + `/#/play/${id}?t=60`, { waitUntil: 'networkidle2' })
  await page.reload({ waitUntil: 'networkidle2' })
  const remembered = await playing(() => {
    const v = document.querySelector('video')
    return v && v.videoHeight === 480 && v.currentTime > 60 && v.currentTime < 75 && !v.paused
  })
  if (!ok(remembered, `${label}：重新打开时按记住的画质（480P）从指定位置播放`)) await dumpPlayer(page)

  // 切回原画
  await page.evaluate(async () => {
    document.querySelector('.q-btn').click()
    await new Promise((r) => setTimeout(r, 400))
    ;[...document.querySelectorAll('.q-menu button')].find((b) => b.textContent.trim().startsWith('原画')).click()
  })
  const orig = await playing(() => {
    const v = document.querySelector('video')
    return v.videoHeight === 720 && !v.paused && v.readyState >= 3 && v.src.includes('/b/') && v.currentTime > 60
  })
  ok(orig, `${label}：切回原画后继续播放`)
  await shot(page, `${label}-transcode`)
  await page.evaluate(() => localStorage.removeItem('vf.quality'))
}

// 续播、上次观看时间、筛选、随机播放（接在 playbackTest 之后：家庭聚会已有约 40 秒的进度）
async function featureTest(page, base, label) {
  // 点“继续观看”里的卡片直接进入播放器，并从上次的位置继续
  const contHref = await page.$$eval('.row-sec', (secs) => {
    const s = secs.find((x) => x.querySelector('.section-title')?.textContent.includes('继续观看'))
    return s?.querySelector('.card')?.getAttribute('href')
  })
  ok(!!contHref && contHref.includes('/play/'), `${label}：继续观看的卡片直接打开播放器（${contHref}）`)
  await page.goto(base + '/' + contHref, { waitUntil: 'networkidle2' })
  const resumedAt = await page
    .waitForFunction(() => {
      const v = document.querySelector('video')
      return v && !v.paused && v.currentTime > 30 && v.currentTime
    }, { timeout: 20000 })
    .then((h) => h.jsonValue())
    .catch(() => 0)
  ok(resumedAt > 30, `${label}：自动从上次的位置继续播放（${Number(resumedAt).toFixed(1)}s）`)
  ok(!!(await page.$('.resume-chip')), `${label}：显示“已从 xx 继续播放”提示`)
  await shot(page, `${label}-resume`)
  await page.click('.resume-chip .btn')
  const restarted = await page
    .waitForFunction(() => document.querySelector('video').currentTime < 5 && !document.querySelector('.resume-chip'), { timeout: 10000 })
    .then(() => true)
    .catch(() => false)
  ok(restarted, `${label}：点击“从头播放”回到开头`)
  await page.$eval('video', (v) => (v.currentTime = 50))
  await sleep(2500)
  await page.click('.top .icon-btn')
  await sleep(1200)

  // 详情页显示上次观看时间和进度
  const partyId = contHref.match(/play\/(\d+)/)[1]
  await page.goto(`${base}/#/item/${partyId}`, { waitUntil: 'networkidle2' })
  await page.waitForSelector('.detail .title')
  const watch = await page.$eval('.watch', (e) => e.textContent).catch(() => '')
  ok(watch.includes('上次观看') && watch.includes('看到'), `${label}：详情页显示上次观看时间和进度（${watch.replace(/\s+/g, ' ').trim()}）`)
  const btn = await page.$eval('.actions .btn.primary', (e) => e.textContent)
  ok(btn.includes('继续播放'), `${label}：详情页主按钮为“继续播放”`)

  // 筛选面板
  await page.goto(`${base}/#/library/1`, { waitUntil: 'networkidle2' })
  await page.waitForSelector('.grid .card')
  const before = await page.$$eval('.grid .card', (c) => c.length)
  await page.click('.filter-btn')
  await page.waitForSelector('.fpanel-wrap.open')
  await sleep(700)
  await shot(page, `${label}-filters`)
  const yearChip = await page.evaluateHandle(() =>
    [...document.querySelectorAll('.fsec')].find((s) => s.querySelector('.fname').textContent === '年份')?.querySelector('.fchip'),
  )
  ok(!!yearChip.asElement(), `${label}：筛选面板列出年份`)
  if (yearChip.asElement()) {
    await yearChip.asElement().click()
    const filtered = await page
      .waitForFunction(() => location.hash.includes('year=2023') && document.querySelectorAll('.grid .card').length === 1, { timeout: 10000 })
      .then(() => true)
      .catch(() => false)
    ok(filtered && before > 1, `${label}：按年份筛选后只剩 1 个视频（之前 ${before} 个）`)
    ok(!!(await page.$('.achip')), `${label}：显示已选筛选条件`)
    await shot(page, `${label}-filtered`)
    await page.click('.clear-all')
    await page.waitForFunction((n) => !location.hash.includes('year=') && document.querySelectorAll('.grid .card').length === n, { timeout: 10000 }, before)
    ok(true, `${label}：清除筛选后恢复全部视频`)
  }
  // 状态筛选：“观看中”
  await page.goto(`${base}/#/library/1?status=inprogress`, { waitUntil: 'networkidle2' })
  await page.waitForSelector('.grid .card')
  await sleep(500)
  const inprog = await page.$$eval('.grid .card .title', (t) => t.map((x) => x.textContent))
  ok(inprog.length === 1 && inprog[0].includes('家庭聚会'), `${label}：“观看中”筛选（${inprog.join('、')}）`)

  // 随机播放：生成队列，播完自动切到下一个
  await page.goto(`${base}/#/library/1`, { waitUntil: 'networkidle2' })
  await page.waitForSelector('.grid .card')
  await page.click('.shuffle-btn')
  await page.waitForFunction(() => location.hash.includes('/play/') && location.hash.includes('queue=1'), { timeout: 10000 })
  await page.waitForSelector('.qinfo')
  const q1 = await page.$eval('.qinfo', (e) => e.textContent)
  ok(/1\/2/.test(q1), `${label}：随机播放显示队列位置（${q1.trim()}）`)
  const firstId = await page.evaluate(() => location.hash.match(/play\/(\d+)/)[1])
  await page.waitForFunction(() => document.querySelector('video')?.readyState >= 1, { timeout: 20000 })
  await page.$eval('video', (v) => (v.currentTime = v.duration - 1))
  const nextShown = await page.waitForSelector('.next-card', { timeout: 15000 }).then(() => true).catch(() => false)
  ok(nextShown, `${label}：播完后提示队列中的下一个`)
  await shot(page, `${label}-shuffle-next`)
  if (nextShown) {
    await page.click('.next-card .btn.primary')
    const moved = await page
      .waitForFunction((id) => location.hash.includes('/play/') && !location.hash.includes(`/play/${id}?`) && document.querySelector('.qinfo')?.textContent.includes('2/2'), { timeout: 10000 }, firstId)
      .then(() => true)
      .catch(() => false)
    ok(moved, `${label}：切换到队列中的第 2 个视频`)
  }

  // 旋转画面：R 顺时针 90°，宽高对调铺满屏幕；按视频记住角度，Shift+R 转回去
  const rotState = () =>
    page.$eval('video', (v) => ({
      sideways: v.classList.contains('sideways'),
      fit: Math.abs(v.offsetWidth - innerHeight) < 2 && Math.abs(v.offsetHeight - innerWidth) < 2,
      rot: getComputedStyle(v).getPropertyValue('--rot').trim(),
    }))
  await page.waitForFunction(() => document.querySelector('video')?.readyState >= 1, { timeout: 20000 })
  await page.keyboard.press('r')
  await sleep(900)
  let rs = await rotState()
  ok(rs.sideways && rs.fit && rs.rot === '90deg', `${label}：按 R 旋转 90° 后画面宽高对调（${JSON.stringify(rs)}）`)
  await shot(page, `${label}-rotated`)
  await page.reload({ waitUntil: 'networkidle2' })
  await page.waitForFunction(() => document.querySelector('video')?.readyState >= 1, { timeout: 20000 })
  rs = await rotState()
  ok(rs.sideways && rs.rot === '90deg', `${label}：刷新后保持该视频的旋转角度`)
  await page.keyboard.down('Shift')
  await page.keyboard.press('R')
  await page.keyboard.up('Shift')
  await sleep(900)
  rs = await rotState()
  ok(!rs.sideways && rs.rot === '0deg', `${label}：Shift+R 逆时针转回原始方向`)
}

try {
  // ================= 网页模式 =================
  console.log('\n[网页模式] 浏览器内加密 + Service Worker 解密')
  const webBase = `http://localhost:${port}`
  const ctx1 = await browser.createBrowserContext()
  const page = await newPage(ctx1, 'web')
  await page.goto(webBase + '/', { waitUntil: 'networkidle2' })
  // ---- 首次设置向导：欢迎 → 管理员 → 服务器 → 媒体库 → 完成 ----
  ok(!srv.log.includes('初始化代码'), '服务端控制台不再输出初始化代码')
  await page.waitForSelector('#start', { timeout: 20000 })
  await sleep(800)
  const cd = await page.$eval('.countdown', (e) => e.textContent).catch(() => '')
  ok(/^(29|30):\d\d$/.test(cd), `设置向导显示剩余初始化时间（${cd}）`)
  await shot(page, 'web-setup')
  await page.click('#start')
  await page.waitForSelector('#u')
  await page.$eval('#u', (e) => (e.value = ''))
  await page.type('#u', 'admin')
  await page.type('#p', 'admin-password-1')
  await page.type('#p2', 'admin-password-1')
  await shot(page, 'web-setup-admin')
  await page.click('button[type=submit]')
  await page.waitForSelector('#sn', { timeout: 20000 })
  ok(true, '设置向导：创建管理员后进入服务器设置')
  await page.$eval('#sn', (e) => (e.value = ''))
  await page.type('#sn', '我的影院')
  await page.click('.fold')
  await sleep(700)
  await shot(page, 'web-setup-server')
  await page.click('#srv-next')
  await page.waitForSelector('#libs-next', { timeout: 10000 })
  await sleep(700)
  const setupLibs = await page.$$eval('.lib', (l) => l.map((x) => x.textContent))
  ok(setupLibs.length === 1 && setupLibs[0].includes('视频'), `设置向导：列出启动参数 -media 自动创建的媒体库（${setupLibs.length} 个）`)
  await page.click('.pane > .btn') // + 再添加一个媒体库
  await page.waitForSelector('.addbox .libform')
  await shot(page, 'web-setup-libs')
  await page.click('#libs-next')
  await page.waitForSelector('#finish')
  await sleep(700)
  await shot(page, 'web-setup-done')
  await page.click('#finish')
  await page.waitForSelector('.topbar', { timeout: 20000 })
  ok(true, '网页模式：完成设置向导并登录')
  const title = await page.$eval('.topbar', (e) => e.textContent).catch(() => '')
  ok(title.includes('我的影院'), '设置向导里填写的服务器名称已生效')
  ok(await page.evaluate(() => !!navigator.serviceWorker.controller), '网页模式：Service Worker 已接管页面')
  ok(await waitForCards(page, webBase), '网页模式：首页出现扫描到的视频')
  await sleep(1000)
  await shot(page, 'web-home-first')
  await playbackTest(page, webBase, 'web')
  await featureTest(page, webBase, 'web')
  await transcodeTest(page, webBase, 'web')

  // ---- 卡片中间的播放键：直接播放，不经过详情页；点卡片其他位置进详情页 ----
  await page.goto(webBase + '/#/library/1', { waitUntil: 'networkidle2' })
  await page.waitForSelector('.grid .card')
  let card = await page.$('.grid .card')
  await card.hover()
  await sleep(500)
  await (await card.$('.play')).click()
  const direct = await page.waitForFunction(() => location.hash.startsWith('#/play/'), { timeout: 5000 }).then(() => true).catch(() => false)
  ok(direct, '点击卡片上的播放键直接进入播放页')
  const playing = await page
    .waitForFunction(() => document.querySelector('video')?.currentTime > 0.5, { timeout: 20000 })
    .then(() => true)
    .catch(() => false)
  ok(playing, '点击播放键后视频直接开始播放')
  await page.goto(webBase + '/#/library/1', { waitUntil: 'networkidle2' })
  await page.waitForSelector('.grid .card')
  card = await page.$('.grid .card')
  await (await card.$('.title')).click()
  const toDetail = await page.waitForFunction(() => location.hash.startsWith('#/item/'), { timeout: 5000 }).then(() => true).catch(() => false)
  ok(toDetail, '点击卡片的其他位置仍然进入详情页')

  // ---- 从详情页返回列表、从播放页返回详情页时，回到原来的滚动位置 ----
  {
    const sp = await newPage(ctx1, 'scroll')
    await sp.setViewport({ width: 900, height: 360 })
    await sp.goto(webBase + '/#/library/1', { waitUntil: 'networkidle2' })
    await sp.waitForSelector('.grid .card')
    await sleep(1000)
    const scrollable = await sp.evaluate(() => document.documentElement.scrollHeight - innerHeight)
    if (ok(scrollable > 80, `滚动测试：媒体库页面可以滚动（${scrollable}px）`)) {
      await sp.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight))
      await sleep(400)
      const y = await sp.evaluate(() => scrollY)
      await sp.evaluate(() => [...document.querySelectorAll('.grid .card')].at(-1).querySelector('.title').click())
      await sp.waitForSelector('.detail .title')
      await sleep(800)
      await sp.evaluate(() => history.back())
      const back = await sp
        .waitForFunction((y) => location.hash.startsWith('#/library/') && document.querySelector('.grid .card') && Math.abs(scrollY - y) < 4, { timeout: 5000 }, y)
        .then(() => true)
        .catch(() => false)
      ok(back, `从详情页返回媒体库后回到原来的滚动位置（${y}px → ${await sp.evaluate(() => scrollY)}px）`)
    }
    // 详情页 → 播放页 → 返回：详情页数据重新加载，等页面撑开后再还原位置
    await sp.evaluate(() => document.querySelector('.grid .card .title').click())
    await sp.waitForSelector('.detail .title')
    await sleep(800)
    const iy = await sp.evaluate(() => {
      window.scrollTo(0, document.documentElement.scrollHeight)
      return scrollY
    })
    if (ok(iy > 40, `滚动测试：详情页可以滚动（${iy}px）`)) {
      await sp.evaluate(() => document.querySelector('.actions .btn.primary').click())
      await sp.waitForFunction(() => location.hash.startsWith('#/play/'), { timeout: 5000 })
      await sleep(1500)
      await sp.evaluate(() => history.back())
      const back = await sp
        .waitForFunction((y) => location.hash.startsWith('#/item/') && Math.abs(scrollY - y) < 4, { timeout: 5000 }, iy)
        .then(() => true)
        .catch(() => false)
      ok(back, `从播放页返回详情页后回到原来的滚动位置（${iy}px → ${await sp.evaluate(() => scrollY)}px）`)
    }
    ok(sp.errors.length === 0, '滚动测试：没有控制台错误' + (sp.errors.length ? '\n      ' + sp.errors.join('\n      ') : ''))
    await sp.close()
  }

  for (const [path, name] of [['/#/admin', 'admin-dashboard'], ['/#/admin/libraries', 'admin-libraries'], ['/#/admin/users', 'admin-users'], ['/#/admin/server', 'admin-server'], ['/#/admin/backup', 'admin-backup'], ['/#/settings', 'settings'], ['/#/history', 'history'], ['/#/library/1?view=folders', 'library-folders']]) {
    await page.goto(webBase + path, { waitUntil: 'networkidle2' })
    await sleep(1200)
    await shot(page, `web-${name}`)
    if (name === 'settings') {
      const conn = await page.$eval('.kv', (e) => e.textContent).catch(() => '')
      ok(conn.includes('ML-KEM-768'), '网页模式：设置页显示后量子加密套件')
      await page.waitForFunction(() => document.querySelector('.ver')?.textContent.startsWith('dev'), { timeout: 5000 }).catch(() => {})
      const vers = await page.$$eval('.ver', (v) => v.map((x) => x.textContent))
      ok(vers.length === 1 && /^dev \([0-9a-f]{7}/.test(vers[0]), `网页模式：设置页显示服务端版本号（${vers.join(' / ')}）`)
    }
  }
  // ---- 管理后台：类型管理（重命名后列表和详情页都更新） ----
  await page.goto(webBase + '/#/admin/genres', { waitUntil: 'networkidle2' })
  await page.waitForSelector('table.list tbody tr')
  const genreRows = () => page.$$eval('table.list tbody tr', (rs) => rs.map((r) => [...r.querySelectorAll('td')].map((td) => td.textContent.trim())))
  ok((await genreRows()).some((r) => r[1] === '家庭' && r[2] === '1'), '类型管理：列出类型和条目数')
  await sleep(900)
  await shot(page, 'web-admin-genres')
  const renameBtn = await page.evaluateHandle(() =>
    [...document.querySelectorAll('table.list tbody tr')].find((r) => r.children[1]?.textContent.trim() === '家庭')?.querySelector('.ops .btn'),
  )
  await renameBtn.click()
  await page.waitForSelector('.dialog input.input')
  await page.$eval('.dialog input.input', (e) => (e.value = ''))
  await page.type('.dialog input.input', '亲情')
  await sleep(900)
  await shot(page, 'web-admin-genre-rename')
  await page.click('.dialog .btn.primary')
  const renamed = await page
    .waitForFunction(() => !document.querySelector('.dialog') && [...document.querySelectorAll('table.list tbody td')].some((td) => td.textContent.trim() === '亲情'), { timeout: 5000 })
    .then(() => true)
    .catch(() => false)
  ok(renamed, '类型管理：重命名后列表更新')
  await page.goto(webBase + '/#/library/1', { waitUntil: 'networkidle2' })
  await page.waitForSelector('.grid .card')
  const partyCard = await page.evaluateHandle(() => [...document.querySelectorAll('.grid .card .title')].find((t) => t.textContent.includes('家庭聚会')))
  await partyCard.click()
  const genreChips = await page
    .waitForFunction(() => [...document.querySelectorAll('.tagrow')].find((r) => r.textContent.includes('类型'))?.querySelector('.chips')?.textContent.trim(), { timeout: 8000 })
    .then((h) => h.jsonValue())
    .catch(() => '')
  ok(genreChips === '亲情', `类型管理：详情页显示改名后的类型（${genreChips}）`)

  const webErrors = page.errors.filter((e) => !e.includes('favicon'))
  ok(webErrors.length === 0, '网页模式：没有控制台错误 / CSP 违规' + (webErrors.length ? '\n      ' + webErrors.join('\n      ') : ''))

  // ---- 管理后台界面：添加受限媒体库 ----
  await page.goto(webBase + '/#/admin/libraries', { waitUntil: 'networkidle2' })
  await page.waitForSelector('.head .btn.primary')
  await page.click('.head .btn.primary')
  await page.waitForSelector('.dialog input.input')
  const dlg = await page.$('.dialog')
  const inputs = await dlg.$$('input.input')
  await inputs[0].type('私人收藏')
  await dlg.$eval('select', (s) => {
    s.value = 'jav'
    s.dispatchEvent(new Event('change'))
  })
  await page.evaluate(() => {
    const sel = document.querySelectorAll('.dialog select')[1]
    sel.value = 'none' // 测试环境没有 MetaTube，不刮削
    sel.dispatchEvent(new Event('change'))
  })
  const pathInput = (await dlg.$$('.pathrow input.input'))[0]
  await pathInput.type(media2)
  const restrictedChecked = await dlg.$eval('input[type=checkbox]', (c) => c.checked)
  ok(restrictedChecked, '管理后台：选择“日本影片”类型时自动勾选“受限”')
  const checks = await dlg.$$eval('input[type=checkbox]', (cs) => cs.map((c) => c.checked))
  ok(checks.length === 2 && checks[1] === false, `管理后台：媒体库表单有“生成进度条预览图”开关，日本影片默认不勾选（${JSON.stringify(checks)}）`)
  await shot(page, 'web-admin-add-library')
  await page.click('.dialog footer .btn.primary')
  await page.waitForFunction(() => !document.querySelector('.dialog'), { timeout: 10000 })
  await sleep(2500)
  await page.goto(webBase + '/#/admin/libraries', { waitUntil: 'networkidle2' })
  await sleep(800)
  const libNames = await page.$$eval('.lib h2', (h) => h.map((x) => x.textContent.trim()))
  ok(libNames.some((n) => n.includes('私人收藏')), '管理后台：通过界面创建了受限媒体库')

  // ---- 媒体库扫描进度：刷新元数据会重新处理全部条目 ----
  const firstLib = (await page.$$('.lib'))[0]
  await (await firstLib.$$('.ops .btn'))[1].click()
  const scanShown = await firstLib.waitForSelector('.scan', { timeout: 5000 }).then(() => true).catch(() => false)
  const scanBtn = await firstLib.$eval('.ops .btn', (b) => ({ text: b.textContent, disabled: b.disabled, spin: !!b.querySelector('.spin-icon.on') }))
  ok(scanShown && scanBtn.text.includes('扫描中') && scanBtn.disabled && scanBtn.spin, '管理后台：点击后立即显示扫描进度，按钮变为“扫描中”并转动')
  await sleep(400)
  await shot(page, 'web-admin-scan-progress')
  const doneShown = await page
    .waitForFunction((el) => el.querySelector('.scan.done .title')?.textContent.includes('扫描完成'), { timeout: 90000 }, firstLib)
    .then(() => true)
    .catch(() => false)
  const doneText = doneShown ? await firstLib.$eval('.scan.done', (e) => e.textContent) : ''
  ok(doneShown && doneText.includes('扫描完成') && /共 \d+ 个视频/.test(doneText), `管理后台：扫描结束后显示完成状态（${doneText.trim()}）`)
  await sleep(300)
  await shot(page, 'web-admin-scan-done')
  const collapsed = await page
    .waitForFunction((el) => !el.querySelector('.expand.open') && !el.querySelector('.ops .btn').disabled, { timeout: 15000 }, firstLib)
    .then(() => true)
    .catch(() => false)
  ok(collapsed, '管理后台：完成状态停留几秒后自动收起，按钮恢复')

  // ---- 设置 PIN ----
  await page.goto(webBase + '/#/settings', { waitUntil: 'networkidle2' })
  await page.waitForSelector('input[placeholder="当前登录密码"]')
  await page.type('input[placeholder="当前登录密码"]', 'admin-password-1')
  await page.type('input[placeholder^="新 PIN"]', '1357')
  await page.click('form.row .btn.primary')
  await sleep(1500)
  const pinState = await page.$eval('section.card-panel:nth-of-type(2) b', (b) => b.textContent)
  ok(pinState.includes('已锁定'), `设置 PIN 后受限内容处于锁定状态（${pinState}）`)

  // ---- 首页：受限库显示为锁定，点击后弹出 PIN 输入框 ----
  await page.goto(webBase + '/#/', { waitUntil: 'networkidle2' })
  await page.waitForSelector('.tile')
  const tiles = await page.$$('.tile')
  let lockedTile = null
  for (const t of tiles) if ((await t.evaluate((e) => e.textContent)).includes('私人收藏')) lockedTile = t
  ok(!!lockedTile, '首页显示受限库的入口')
  await lockedTile.click()
  await page.waitForSelector('.pin', { timeout: 5000 })
  await shot(page, 'web-pin-dialog')
  await page.type('.pin', '0000')
  await page.keyboard.press('Enter')
  await page.waitForFunction(() => document.querySelector('.dialog .err')?.textContent.includes('PIN'), { timeout: 5000 })
  ok(true, '输入错误的 PIN 被拒绝')
  await page.$eval('.pin', (e) => (e.value = ''))
  await page.type('.pin', '1357')
  await page.keyboard.press('Enter')
  const unlocked = await page
    .waitForFunction(() => location.hash.startsWith('#/library/') && document.querySelector('.grid .card'), { timeout: 20000 })
    .then(() => true)
    .catch(() => false)
  ok(unlocked, '输入正确的 PIN 后进入受限库并看到内容')
  await shot(page, 'web-restricted-library')

  // ---- 顶栏一键隐藏 / 恢复受限封面的模糊蒙层（用户级，刷新后保持） ----
  const blurred = () => page.$$eval('.grid .card img', (is) => [is.length, is.filter((i) => i.classList.contains('blur')).length])
  await page.waitForSelector('.grid .card img')
  const [imgCount, blurCount] = await blurred()
  ok(imgCount > 0 && blurCount === imgCount, `受限库的封面默认模糊（${blurCount}/${imgCount}）`)
  await page.click('.topbar .blur-toggle')
  const unblurred = await page
    .waitForFunction(() => !document.querySelector('.grid .card img.blur') && document.querySelector('.topbar .blur-toggle')?.title === '模糊受限内容的封面', { timeout: 5000 })
    .then(() => true)
    .catch(() => false)
  ok(unblurred, '点击顶栏按钮后受限封面的蒙层立即消失')
  await sleep(800)
  await shot(page, 'web-restricted-unblurred')
  // 服务端返回的用户信息里偏好已经改了（界面状态来自服务端的返回值）；离开再回来也保持
  const restrictedUrl = page.url()
  await page.goto(webBase + '/#/', { waitUntil: 'networkidle2' })
  await page.goto(restrictedUrl, { waitUntil: 'networkidle2' })
  await page.waitForSelector('.grid .card img')
  await sleep(500)
  ok((await blurred())[1] === 0, '离开再回到受限库仍不模糊')
  await page.click('.topbar .blur-toggle')
  const reblurred = await page
    .waitForFunction(() => document.querySelectorAll('.grid .card img.blur').length > 0, { timeout: 5000 })
    .then(() => true)
    .catch(() => false)
  ok(reblurred, '再次点击恢复模糊蒙层')

  // ---- 顶栏一键锁定 ----
  await page.click('.topbar button[title="锁定受限内容"]')
  await sleep(1200)
  await page.goto(webBase + '/#/', { waitUntil: 'networkidle2' })
  await page.waitForSelector('.tile')
  const relocked = await page.$$eval('.tile', (ts) => ts.some((t) => t.textContent.includes('私人收藏') && t.querySelector('svg')))
  const latestRows = await page.$$eval('.section-title', (h) => h.map((x) => x.textContent))
  ok(relocked && !latestRows.some((r) => r.includes('私人收藏')), '锁定后首页不再展示受限库的内容')

  // 刷新后仍保持登录（令牌恢复 + 新握手）
  await page.reload({ waitUntil: 'networkidle2' })
  ok(!!(await page.$('.topbar')), '网页模式：刷新页面后保持登录')

  // ---- 全屏：桌面端全屏整个播放器（保留自定义控件），移动端交给系统播放器 ----
  const fsState = (p) =>
    p.evaluate(() => {
      const el = document.fullscreenElement
      return { tag: el?.tagName || '', player: !!el?.classList.contains('player'), controls: !!document.querySelector('video')?.controls }
    })
  const openFs = async (p, tap) => {
    await p.goto(webBase + '/#/play/1?t=0', { waitUntil: 'networkidle2' })
    await p.waitForFunction(() => document.querySelector('video')?.currentTime > 0.3, { timeout: 20000 })
    const sel = 'button[title="全屏 (F)"]'
    if (tap) {
      // 控制栏显示时轻点画面会把它隐藏，所以等它自动隐藏后再轻点唤出
      await p.waitForSelector('.player.hide-ui', { timeout: 8000 })
      await p.tap('.player')
    }
    else await p.mouse.move(700, 450)
    await sleep(500)
    await (tap ? p.tap(sel) : p.click(sel))
    await sleep(800)
    return fsState(p)
  }
  let fs = await openFs(page, false)
  ok(fs.player && !fs.controls, `桌面端：全屏的是整个播放器，使用自定义控件（${JSON.stringify(fs)}）`)
  await page.evaluate(() => document.exitFullscreen())

  const mp = await newPage(ctx1, 'mobile')
  await mp.emulate(KnownDevices['Pixel 5'])
  ok(await mp.evaluate(() => matchMedia('(pointer: coarse)').matches), '移动端模拟：触屏为主的设备')
  fs = await openFs(mp, true)
  ok(fs.tag === 'VIDEO' && fs.controls, `移动端：全屏交给系统播放器（video 元素自身全屏并显示系统控件，${JSON.stringify(fs)}）`)
  // 系统全屏时点击画面交给系统控件处理，不触发我们自己的播放 / 暂停（自己的处理会弹出中央反馈气泡）
  const ourHandler = await mp.evaluate(async () => {
    document.querySelector('video').click()
    await new Promise((r) => setTimeout(r, 100))
    return !!document.querySelector('.bubble')
  })
  ok(!ourHandler, '移动端：系统全屏时点击画面不会被自定义控件拦截')
  await mp.evaluate(() => document.exitFullscreen())
  await sleep(500)
  fs = await fsState(mp)
  ok(!fs.tag && !fs.controls, '移动端：退出系统全屏后恢复自定义控件')

  // ---- 移动端播放页：时间在进度条上方左对齐，按钮居中；轻点画面只切换控制栏、不暂停 ----
  const layout = await mp.evaluate(() => {
    const r = (s) => document.querySelector(s)?.getBoundingClientRect()
    const t = r('.m-time')
    const bar = r('.bar')
    const btns = [...document.querySelectorAll('.controls > *')].map((e) => e.getBoundingClientRect()).filter((b) => b.width)
    const c = r('.controls')
    const left = Math.min(...btns.map((b) => b.left))
    const right = Math.max(...btns.map((b) => b.right))
    return {
      time: !!t && t.bottom <= bar.top + 1 && Math.abs(t.left - bar.left) < 6,
      oldTime: !!document.querySelector('.controls .time'),
      centered: Math.abs((left + right) / 2 - (c.left + c.right) / 2) < 3,
      fits: right <= innerWidth && left >= 0,
    }
  })
  ok(layout.time && !layout.oldTime, `移动端：时间显示在进度条上方并左对齐（${JSON.stringify(layout)}）`)
  ok(layout.centered && layout.fits, `移动端：控制按钮在底部居中且不超出屏幕（${JSON.stringify(layout)}）`)
  // 后退 / 播放暂停 / 前进不在底部，而是在画面中央弹出
  const center = await mp.evaluate(() => {
    const r = (s) => document.querySelector(s)?.getBoundingClientRect()
    const play = r('.m-center .m-play')
    const p = r('.player')
    return {
      inBottom: !!document.querySelector('.controls .big'),
      play: !!play,
      centered: !!play && Math.abs(play.left + play.width / 2 - (p.left + p.width / 2)) < 3 && Math.abs(play.top + play.height / 2 - (p.top + p.height / 2)) < 3,
      skips: document.querySelectorAll('.m-center .m-skip').length,
    }
  })
  ok(!center.inBottom && center.play && center.centered && center.skips === 2, `移动端：后退 / 播放暂停 / 前进在画面中央（${JSON.stringify(center)}）`)
  await shot(mp, 'web-mobile-player')
  // 轻点画面上部的空白处（避开中央按钮）
  const tapBlank = async () => {
    const vp = mp.viewport()
    await mp.touchscreen.tap(vp.width / 2, Math.round(vp.height * 0.3))
  }
  const st = () => mp.evaluate(() => ({ paused: document.querySelector('video').paused, hidden: !!document.querySelector('.player.hide-ui') }))
  await mp.evaluate(() => document.querySelector('video').play())
  if (await mp.$('.player.hide-ui')) await tapBlank()
  await sleep(300)
  await tapBlank()
  await sleep(400)
  const afterTap = await st()
  ok(!afterTap.paused && afterTap.hidden, `移动端：播放中轻点画面不暂停，只隐藏控制栏（${JSON.stringify(afterTap)}）`)
  // 控制栏收起时，中央按钮不响应点击，点到的是画面
  const hiddenHit = await mp.evaluate(() => {
    const b = document.querySelector('.m-play').getBoundingClientRect()
    return document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2)?.tagName
  })
  ok(hiddenHit === 'VIDEO', `移动端：控制栏收起时中央按钮不拦截点击（点到 ${hiddenHit}）`)
  await tapBlank()
  await sleep(400)
  const afterTap2 = await st()
  ok(!afterTap2.paused && !afterTap2.hidden, `移动端：再次轻点唤出控制栏，仍在播放（${JSON.stringify(afterTap2)}）`)
  await mp.tap('.m-center .m-play')
  await sleep(400)
  const paused = await st()
  ok(paused.paused && !paused.hidden, `移动端：点中央按钮暂停，暂停时控制栏保持显示（${JSON.stringify(paused)}）`)
  await sleep(3500)
  ok(!(await st()).hidden, '移动端：暂停 3 秒后控制栏仍然显示')
  await mp.tap('.m-center .m-play')
  await sleep(400)
  ok(!(await st()).paused, '移动端：再点中央按钮继续播放')
  await sleep(3800)
  ok((await st()).hidden, '移动端：播放中 3 秒后控制栏自动收起')

  // ---- 移动端卡片：没选中时点中间进详情页；点一下选中（iOS 会保持悬停状态、显示播放键）后再点播放键直接播放 ----
  {
    const openLib = async () => {
      await mp.goto(webBase + '/#/library/1', { waitUntil: 'networkidle2' })
      await mp.waitForSelector('.grid .card .play')
      await sleep(300)
      return mp.$eval('.grid .card .play', (b) => {
        const r = b.getBoundingClientRect()
        return { x: r.left + r.width / 2, y: r.top + r.height / 2 }
      })
    }
    let at = await openLib()
    await mp.touchscreen.tap(at.x, at.y)
    const toItem = await mp.waitForFunction(() => location.hash.startsWith('#/item/'), { timeout: 5000 }).then(() => true).catch(() => false)
    ok(toItem, '移动端：卡片没选中时点中间（播放键还没显示）进入详情页')
    at = await openLib()
    // 模拟“已选中”：卡片处于悬停状态，播放键已显示
    await mp.mouse.move(at.x, at.y)
    await sleep(400)
    const shown = await mp.$eval('.grid .card', (c) => c.matches(':hover') && getComputedStyle(c.querySelector('.hover')).opacity > 0.9)
    await mp.touchscreen.tap(at.x, at.y)
    const toPlay = await mp.waitForFunction(() => location.hash.startsWith('#/play/'), { timeout: 5000 }).then(() => true).catch(() => false)
    ok(shown && toPlay, `移动端：卡片选中后点播放键直接进入播放页（播放键已显示 ${shown}，进入播放页 ${toPlay}）`)
  }
  // ---- 移动端管理后台 - 用户页：表格改为一人一张卡片，不出现横向滚动 ----
  {
    await mp.goto(webBase + '/#/admin/users', { waitUntil: 'networkidle2' })
    await mp.waitForSelector('table.list tbody tr')
    // 在手机上添加一个普通用户（顺带验证弹窗表单在小屏上可用）
    await mp.$$eval('.head .btn.primary', (b) => b[0].click())
    await mp.waitForSelector('.dialog input.input')
    const [nameInput, pwInput] = await mp.$$('.dialog input.input')
    await nameInput.type('mobile-user-with-long-name')
    await pwInput.type('mobile-password-1')
    await mp.$$eval('.dialog footer .btn.primary', (b) => b[0].click())
    await mp.waitForFunction(() => document.querySelectorAll('table.list tbody tr').length >= 2 && !document.querySelector('.dialog'), { timeout: 8000 }).catch(() => {})
    await sleep(800)
    const ul = await mp.evaluate(() => {
      const rows = [...document.querySelectorAll('table.list tbody tr')]
      const inView = (e) => {
        const r = e.getBoundingClientRect()
        return r.left >= 0 && r.right <= innerWidth + 0.5
      }
      return {
        rows: rows.length,
        noHScroll: document.documentElement.scrollWidth <= innerWidth,
        headHidden: getComputedStyle(document.querySelector('table.list thead')).display === 'none',
        cards: rows.every((r) => getComputedStyle(r).display === 'grid'),
        fits: rows.every((r) => [...r.querySelectorAll('td, .btn')].every(inView)),
        labels: getComputedStyle(rows[0].querySelector('td.libs'), '::before').content,
      }
    })
    ok(ul.rows >= 2 && ul.noHScroll && ul.headHidden && ul.cards && ul.fits && ul.labels.includes('媒体库'), `移动端：用户页显示为卡片、内容不超出屏幕（${JSON.stringify(ul)}）`)
    await shot(mp, 'web-mobile-admin-users')
  }
  ok(mp.errors.length === 0, '移动端：没有控制台错误' + (mp.errors.length ? '\n      ' + mp.errors.join('\n      ') : ''))
  await mp.close()

  // ================= 本地客户端模式 =================
  console.log('\n[本地客户端模式] 浏览器 → 127.0.0.1 → Go 客户端加密 → 服务端')
  await start(['client', '-server', `http://127.0.0.1:${port}`, '-server-key', pubkey, '-listen', `127.0.0.1:${cport}`, '-data', join(work, 'client'), '-open=false'], (log) =>
    log.includes('本地客户端已启动'),
  )
  const localBase = `http://127.0.0.1:${cport}`
  const dlDir = join(work, 'downloads')
  mkdirSync(dlDir)
  const ctx2 = await browser.createBrowserContext({ downloadBehavior: { policy: 'allow', downloadPath: dlDir } })
  await ctx2.overridePermissions(localBase, ['clipboard-read', 'clipboard-write', 'clipboard-sanitized-write'])
  const lp = await newPage(ctx2, 'local')
  await lp.goto(localBase + '/', { waitUntil: 'networkidle2' })
  await lp.waitForSelector('#u', { timeout: 20000 })
  await shot(lp, 'local-login')
  await lp.type('#u', 'admin')
  await lp.type('#p', 'admin-password-1')
  await lp.click('button[type=submit]')
  await lp.waitForSelector('.topbar', { timeout: 20000 })
  ok(true, '本地模式：登录成功')
  ok(await lp.evaluate(() => !navigator.serviceWorker.controller), '本地模式：不使用 Service Worker（由 Go 客户端解密）')
  await playbackTest(lp, localBase, 'local')
  await transcodeTest(lp, localBase, 'local')
  await lp.goto(localBase + '/#/settings', { waitUntil: 'networkidle2' })
  await lp.waitForFunction(() => [...document.querySelectorAll('.ver')].filter((v) => v.textContent.startsWith('dev')).length === 2, { timeout: 8000 }).catch(() => {})
  const lvers = await lp.$$eval('.ver', (v) => v.map((x) => x.textContent))
  ok(lvers.length === 2 && lvers.every((v) => v.startsWith('dev')) && !(await lp.$('.settings .warn')), `本地模式：设置页显示服务端和客户端版本号（${lvers.join(' / ')}）`)
  await shot(lp, 'local-settings')

  // ---- 复制服务端公钥 ----
  const pkText = await lp.$eval('.copy-field code', (c) => c.textContent).catch(() => '')
  ok(pkText === pubkey, '本地模式：设置页显示完整的服务端公钥')
  await lp.click('.copy-field .icon-btn')
  await sleep(300)
  const clip = await lp.evaluate(() => navigator.clipboard.readText()).catch((e) => 'ERR ' + e.message)
  ok(clip === pubkey, '点击复制按钮后，公钥进入剪贴板')
  await lp.goto(localBase + '/#/admin/server', { waitUntil: 'networkidle2' })
  await lp.waitForFunction(() => document.querySelectorAll('.copy-field code').length === 3 && location.hash === '#/admin/server', { timeout: 8000 })
  await sleep(600)
  const fields = await lp.$$eval('.copy-field code', (cs) => cs.map((c) => c.textContent))
  ok(fields[0] === pubkey && fields[2]?.includes(`-server http://127.0.0.1:${port} -server-key '${pubkey}'`), '管理后台：服务器设置页显示公钥、指纹和客户端启动命令')
  await shot(lp, 'local-admin-server')

  // ---- 备份与恢复（在线恢复后服务端在同一进程内重启） ----
  const lrpc = (m, p) =>
    lp.evaluate(
      async (m, p) => {
        const r = await fetch('/api/rpc', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-VF-Local': '1' }, body: JSON.stringify({ m, p }) })
        return r.json()
      },
      m,
      p,
    )
  await lp.goto(localBase + '/#/admin/backup', { waitUntil: 'networkidle2' })
  await lp.waitForSelector('input.pw')
  const pws = await lp.$$('input.pw')
  await pws[0].type('backup-pass-1')
  await pws[1].type('backup-pass-1')
  await lp.click('form.row .btn.primary')
  await lp.waitForSelector('.file .name', { timeout: 60000 })
  const bkName = await lp.$eval('.file .name', (e) => e.textContent)
  ok(/^veilfin-\d{8}-\d{6}\.vfbak$/.test(bkName), `备份页：创建了备份 ${bkName}`)
  await shot(lp, 'local-admin-backup')
  await lp.click('.file .ops .btn') // 下载
  const dlFile = join(dlDir, bkName)
  for (let i = 0; i < 50 && !existsSync(dlFile); i++) await sleep(200)
  await sleep(300)
  ok(existsSync(dlFile) && readFileSync(dlFile).equals(readFileSync(join(data, 'backups', bkName))), '备份页：下载的备份与服务端上的文件一致')
  const created = await lrpc('admin.users.create', { username: 'tmpuser', password: 'tmp-password-1' })
  ok(created.ok, '备份之后新建用户 tmpuser')
  await (await lp.$$('.file .ops .btn'))[1].click() // 恢复
  await lp.waitForSelector('.dialog input[type=password]')
  await lp.type('.dialog input[type=password]', 'backup-pass-1')
  await lp.click('.dialog footer .btn.primary')
  await lp.waitForSelector('.staged', { timeout: 30000 })
  await sleep(600)
  const stagedText = await lp.$eval('.staged', (e) => e.textContent)
  ok(stagedText.includes('客户端不需要改动') && stagedText.includes(pubkey), '备份页：校验通过后显示待确认的恢复，公钥未变')
  await shot(lp, 'local-admin-restore-confirm')
  const nav = lp.waitForNavigation({ timeout: 60000 })
  await lp.click('.staged .btn.danger')
  await sleep(800)
  await shot(lp, 'local-admin-restarting')
  await nav
  ok(srvLog.includes('已从备份恢复数据目录'), '服务端在同一进程内应用恢复并重启')
  await lp.waitForSelector('.topbar', { timeout: 20000 })
  const users = await lrpc('admin.users.list', {})
  ok(users.ok && users.data.some((u) => u.username === 'admin') && !users.data.some((u) => u.username === 'tmpuser'), '恢复后页面自动刷新，仍保持登录，备份之后新建的用户已消失')
  const localErrors = lp.errors.filter((e) => !e.includes('favicon'))
  ok(localErrors.length === 0, '本地模式：没有控制台错误 / CSP 违规' + (localErrors.length ? '\n      ' + localErrors.join('\n      ') : ''))
} catch (e) {
  failed++
  console.error('测试异常：', e)
} finally {
  await browser.close().catch(() => {})
  for (const p of procs) p.kill()
  await sleep(800)
  try {
    rmSync(work, { recursive: true, force: true })
  } catch {}
}
console.log(failed ? `\n失败 ${failed} 项` : '\n全部通过')
console.log('截图目录：' + shots)
process.exit(failed ? 1 : 0)
