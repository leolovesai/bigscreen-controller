# 公司大屏幕网页控制器 — 设计文档

> 二进制名 `deskctl` · v0.1 / 2026-11-03 / macOS 实测通过
> 启动一个本地服务，网页上点场景 → Chrome 打开指定 URL、全屏展示；网页上实时看页面缩略图，并能直接在上面点击 / 拖动 / 左右滑。

---

## 1. 功能与验收

| # | 功能 | 实现方式 | 实测 |
|---|------|----------|------|
| F1 | 场景化打开 Chrome | CDP `Page.navigate`，URL 从 `config.yaml` 读 | ✅ 百度 / Bing / 空白页 |
| F2 | 全屏 / 退出全屏 | CDP `Browser.setWindowBounds`，macOS 上自动降级 | ✅ 视口 1440×900 |
| F3 | 页面缩略图实时显示 | CDP `Page.startScreencast` → 按面板宽度推帧 | ✅ 780 宽 / 8-34KB/帧，静止不推 |
| F4 | 缩略图上操作页面 | CDP `Input.dispatchMouseEvent` | ✅ 点击链接、拖动、左右滑 |

### 关键取舍：全部能力走 CDP，不碰系统 API

原本设计用 `CGDisplayCreateImage` 截整屏 + `CGEvent` 注入鼠标，现按需求收窄为「只管 Chrome」，于是整条系统 API 依赖被移除：

| 维度 | 原方案（系统 API） | 现方案（CDP） |
|------|-------------------|--------------|
| 截图 | `CGDisplayCreateImage` | `Page.startScreencast` |
| 鼠标 | `CGEventCreateMouseEvent` | `Input.dispatchMouseEvent` |
| 权限 | 屏幕录制 + 辅助功能（两个 TCC 授权） | **零授权** |
| 依赖 | CGO + CoreGraphics | 纯 Go，`CGO_ENABLED=0` 可交叉编译 |
| 影响面 | 全局，能误触桌面其他应用 | 仅页面内，不可能误操作别的窗口 |

实测印证：`CGDisplayCreateImage` 在 macOS 15+ 的 SDK 里已被标记 `obsoleted`，硬编译直接失败 —— 走 CDP 顺带绕过了这个坑。

---

## 2. 架构

```
┌──────────────┐   HTTP/WS 127.0.0.1:7788   ┌───────────────┐   CDP(ws)   ┌────────┐
│ 网页控制器    │ ◄────────────────────────► │ deskctl agent │ ◄─────────► │ Chrome │
│ canvas+按钮  │   JSON 指令 / JPEG 帧       │  Go 单二进制   │             └────────┘
└──────────────┘                            └───────────────┘
```

- **agent**：唯一后端。截图、鼠标、导航、全屏全部转成 CDP 调用。10MB 单文件，无运行时依赖。
- **控制器**：单文件 HTML，纯原生 JS。手机浏览器也能开。
- **Chrome**：独立 profile（`~/.deskctl/chrome-profile`），`--remote-debugging-port=0` 动态端口，不干扰日常浏览器。

---

## 3. 目录结构

```
deskctl/
├── main.go                     # 启动、embed 前端、优雅退出
├── internal/
│   ├── config/config.go        # yaml 加载 + 校验（URL scheme 白名单、密码强制）
│   ├── chrome/
│   │   ├── cdp.go              # 极简 CDP 客户端（call + 事件回调 + notify）
│   │   ├── manager.go          # 进程管理、attach、导航、全屏、screencast 推帧、鼠标
│   ├── server/
│   │   ├── server.go           # 路由、token/cookie 鉴权、密码登录
│   │   └── ws.go               # WS 协议、帧转发、手势合成
├── web/{index.html,app.js,style.css}
├── cmd/probe/main.go           # 冒烟测试工具（可选）
├── cmd/screencast/main.go      # screencast 集成探针（帧尺寸/去重/面板切换）
└── config.yaml
```

---

## 4. 关键实现

### 4.1 推流（变化驱动，服务端不碰像素）

```
前端 {type:"stream.start", quality, w}      w = 画面区域的物理像素宽
  └─ Page.startScreencast(maxWidth=w, maxHeight=w·h/w, quality)
       Chrome 只在画面变化时推帧，降采样也在 Chrome 内部完成
       服务端收到就转发：[4B 头长][JSON 头][JPEG]
       每帧必须回 Page.screencastFrameAck，否则推两三帧就停
```

- 分辨率跟着客户端面板走：手机画面 780px 宽就下发 780px，不再固定 1:1。
- 页面静止 = 0 帧 0 流量。服务端 500ms 轮询面板宽度/quality，变了就重启 screencast。
- **没有帧率参数**：帧率由页面变化频率 + ack 节流决定，页不动就不推。
- 服务端不解码、不缩放、不编码，单帧开销只有 base64 解码 + 一次 WS 写。
- 「没有帧」不等于断流，所以另有 3 秒一次的 `Runtime.evaluate` 心跳探活。
- 前端解码跟不上直接丢帧（`S.decoding` 标志），不排队。

### 4.2 全屏（踩坑最多的一处）

CDP 的 `Browser.setWindowBounds {windowState:"fullscreen"}` 在 **macOS + app 窗口上会被静默忽略**，不报错，但读回 `windowState` 仍是 `maximized`。

更糟的是：如果按"先 normal 再 fullscreen"的直觉去写，每次调用都会触发 macOS 窗口级联，窗口一轮比一轮小（实测 1440×900 → 1420×764 → 1434×746）。

最终实现：**先请求 fullscreen，读回状态确认；被忽略则退到 maximized**，判定依据是读回的 `Bounds.WindowState`，不是调用是否报错。配合 `--app` 模式（无标签栏/地址栏），实测视口可达 1440×900。

### 4.3 鼠标（只作用于页面）

坐标链路：前端归一化 0..1 → 服务端 × CSS 视口宽高 → `Input.dispatchMouseEvent`。

三个必须记住的细节：

1. `mouseMoved` 的 `button` 必须传 `none`，传 `left` 会被 Chrome 当成异常按键状态。
2. 单击要 `moved → pressed → released` 三步，很多页面靠 mousemove 建立 hover 态。
3. 手势（左滑/右滑）在**服务端**合成 12 步：`pressed → 12×moved(easeOut) → released`。即使网页卡顿或断线，位移也完整；无论中间哪步失败都用 defer 补一次 `released`。

### 4.4 坐标换算（前端）

canvas 填满容器，画面按 contain 居中，存在留边：

```js
const s  = Math.min(cw / fw, ch / fh);
const ox = (cw - fw * s) / 2;
const oy = (ch - fh * s) / 2;
const nx = (offsetX - ox) / (fw * s);   // 越界直接丢弃
```

漏掉 `ox` 就会整体偏移几十像素 —— 这是缩略图点击最典型的 bug。

### 4.5 链接拦截（app 模式必需）

页面里 `target="_blank"` 的链接会新开一个**普通 Chrome 窗口**（带 UI、不全屏），画面立刻退化。

处理：`Page.addScriptToEvaluateOnNewDocument` 注入脚本，捕获阶段把 `a.target` 改成 `_self`，并接管 `window.open`。注入时机是 attach 之后、导航之前，对新文档生效。

### 4.6 启动窗口与 target 选择（曾经导致「多一个黑窗口 + 页面乱闪」）

app 模式必须给一个初始 URL（`about:blank` 会被 Chrome 忽略，退化成普通窗口），所以用
`--app=data:text/html;base64,...` 打开一个「deskctl / 等待控制器选择场景」的待机页。这个窗口**就是被控窗口本身**，场景一导航它就被替换掉。

坑在于：`--app` 窗口从进程启动到出现在 CDP target 列表里有几百毫秒延迟。这期间
`Target.getTargets` 可能是空的，若此时用 `Target.createTarget` 兜底，就会**额外开一个窗口**：

- 业务页跑进新窗口，原来的 app 窗口变成一块纯黑留在桌面上（用户以为多了个页面）；
- 两个 page target 并存，attach 目标在两个窗口之间摆动 → 画面不停闪；
- 新窗口是普通窗口，全屏只能到 `1440×752`，看起来像全屏没生效。

现在的做法：

1. `connect` 轮询 `Target.getTargets`（最多 6s）等自己的页面出现，只有真等不到才 `createTarget`；
2. `pickPageTarget` 按 **app 待机页 → 已加载的真实页面 → about:blank → chrome://** 的优先级挑，
   避免把 `chrome://newtab` 当成业务页；
3. 实测：连续跑 3 次场景，page target 恒为 1；20s 采样 target 零变化；全屏视口 `1440×900`。

### 4.7 鉴权

- 启动生成 32 位随机 token，打印带 `?k=` 的地址；访问后种 cookie，URL 变干净。
- 局域网访问必须配 `server.password`（`host` 非回环且密码为空 → 启动直接报错）。
- WS 校验 `Origin` 与 `Host` 同源，防其他网页 CSRF 打到本地端口。
- 密码错误有 1s/IP 限流 + 600ms 延迟。

---

## 5. WebSocket 协议

| 方向 | type | 字段 |
|------|------|------|
| → | `stream.start` | `quality, w`（w = 画面物理像素宽，决定下发分辨率） |
| → | `stream.stop` | – |
| → | `panel` | `w`（面板尺寸变化，只改下发宽度） |
| → | `mouse` | `action: down\|move\|up\|click, x, y, buttons` |
| → | `gesture.swipe` | `dir: left\|right\|up\|down, distance` |
| → | `wheel` | `x, y, dx, dy` |
| → | `scenes.run` | `id` |
| → | `chrome.fullscreen` | `on` |
| → | `chrome.navigate` | `url`（scheme 白名单） |
| → | `page.scan` | –（重扫当前页可点元素） |
| → | `page.press` | `index`（扫描结果里的编号，0 起） |
| → | `ping` | `t` |
| ← | `hello` | `chrome, scenes, capture` |
| ← | binary | `4B 头长 + {seq,ts,cw,ch,bytes} + JPEG`（尺寸从 JPEG 头取，不再单独下发） |
| ← | `scenes.state` | `id, phase: running\|ready\|error, msg` |
| ← | `page.buttons` | `scan: {url, title, total, items[], at}` |
| ← | `page.pressed` | `index, error?` |
| ← | `error` | `code, msg` |

---

## 5.1 页面按钮识别

目标：新页面打开后，把页面上「能点的东西」列到控制器里，手机点一下就等于在页面上点一下。

**扫描（纯 JS，`Runtime.evaluate` 取回 JSON）**

候选选择器：
`a[href], button, input[type=submit|button|reset], summary, [role=button|link|tab|menuitem], [onclick]`

过滤规则：
- 可见且有实际尺寸（`w≥12 && h≥10`，`display/visibility/opacity` 排除）
- 排除 `disabled`、`aria-hidden=true`
- **祖先也是候选时保留祖先**（点击区域更贴合视觉边界，避免点 `button` 里的 `span`）
- 排除面积 > 视口 92% 的容器（否则整页的 `onclick` 会顶上来）
- 文本优先 `innerText`，退化到 `aria-label / title / placeholder / value / img[alt]`，再退化到 `(标签名)`
- 按文档位置从上到下、从左到右排序，截断 42 字，上限 60 条（`total` 保留真实数量）

**点击（不依赖 `el.click()`）**

扫描时给每个元素打 `data-dc="编号"`。点击流程：

1. `scrollIntoView({block:'center'})` → 取 `getBoundingClientRect()` 中心
2. `document.elementFromPoint()` 判断中心点是否真被该元素接住
3. 接住 → 派发真实鼠标事件 `moved → pressed → released`（与手动点画面完全一致，能触发 hover / mousedown 逻辑）
4. 被吸顶导航遮住 → 退回 `el.click()`

**触发时机**

| 场景 | 触发 |
|------|------|
| 控制器连上 | 立即扫一次 |
| 场景打开完成 | 扫 |
| 页面 URL 变化 | `WatchPages` 每 1.2s 比对 URL（无人观看时不跑） |
| 点了按钮 | 600ms 后轮询重扫，直到拿到非空结果或 9s 超时 |
| 手动 | 列表右上角「刷新」 |

扫到空结果不认为正常，记 `emptyHits` 让下一轮重试（上限 6 次），
因为跳转期间 DOM 还没就绪，一次空扫就把列表清空是错的行为。

**限流**：全局 `scanMu` 串行，多个控制器同时刷新只扫一次，结果广播给所有人。

---

## 6. 配置

```yaml
server:
  host: 0.0.0.0        # 局域网设备可访问；改 127.0.0.1 则只本机
  port: 7788
  password: "deskctl"  # 非回环地址时必填
  autoOpen: true

capture:
  quality: 60          # 下发 JPEG 质量；分辨率由客户端面板决定

chrome:
  binary: ""           # 留空自动探测
  userDataDir: ~/.deskctl/chrome-profile
  windowSize: [1512, 945]
  appMode: true        # 无标签栏/地址栏，全屏后是纯网页
  extraArgs: ["--disable-features=Translate,MediaRouter"]

scenes:
  - id: baidu
    name: 百度
    url: https://www.baidu.com
    fullscreen: true
    wait: load
    timeoutMs: 8000
```

新增场景只需加一段 yaml，重启生效。

---

## 7. 使用

```bash
go build -o deskctl .
./deskctl                  # 静默启动：打印本机/局域网地址 + 局域网二维码，不弹浏览器、不启 Chrome
./deskctl -open            # 启动后顺便打开控制器页面
./deskctl -v               # 额外打印版本/Chrome 路径
./deskctl -addr 127.0.0.1:8000
```

启动是静默的，具体指三件事：

| 行为 | 默认 | 说明 |
|------|------|------|
| 打开控制器页面 | 否（`-open` 或 `autoOpen: true` 才开） | 服务器场景下不该抢焦点 |
| 预热 Chrome | 否 | 点场景时才拉起（冷启动 3-4s），拉起来直接进全屏 |
| 终端输出 | 两行带 token 的地址 + 局域网二维码 | 0.0.0.0 模式下额外把局域网地址印成二维码，手机扫一下就进控制器 |

**局域网二维码**：`0.0.0.0` 监听时，启动日志在地址行下面直接印一个二维码，
内容是 `http://<内网IP>:<port>/?k=<token>`，手机扫码即带 token 登录，不用手动输密码。

- 编码：`github.com/skip2/go-qrcode`，Medium 纠错；41 列 × 22 行，80 列终端不折行。
- 渲染：`ToSmallString` 半块字符（一行顶两行模块，模块保持正方），外面套 `\x1b[30;47m`
  黑前景 + 白背景，浅色终端不会反色导致扫不出来。
- 选地址：优先 10/8、172.16/12、192.168/16，跳过回环与 169.254，避免选中 utun / docker 网段。

Chrome 由第一次操作按需拉起（`Ensure` 惰性），所以「服务起着但没人用」时进程里没有任何 Chrome、CPU 占用为 0。

控制器上：
- 点场景按钮 → **Chrome 没起就先拉起**（冷启动约 3-4s），再导航、按场景 `fullscreen` 进全屏，
  全屏后的真实窗口状态会以 `chrome.state` 回推，顶栏「全屏中」角标随即同步
- **进入新页面自动列出可点按钮**（「页面按钮」分段），点一下就相当于在页面上点
- 画面直接点击 / 拖动 = 真实操作页面
- 「← 左滑 / 右滑 →」= 水平拖拽手势
- 「↑ 上一页 / 下一页 ↓」= 按可视高度向下/向上滚一屏（`wheel` 通道）
- 帧率 / 清晰度 滑块可实时调（手机端在右上角 ⚙ 抽屉里）

### 7.1 界面布局（移动优先）

操作员主要用手机 / 平板，因此界面按「画面让位、按钮优先」设计：

| 视口 | 布局 | 画面占比 |
|------|------|----------|
| 手机竖屏（< 900px） | 上画面 + 下按钮区单列 | 画面固定 40dvh，其余全给按钮 |
| 手机横屏（landscape，高 < 560px） | 左右分栏，按钮区 46% 宽 | 画面自适应 |
| 平板竖屏（834×1194） | 同手机竖屏，按钮自动变宽 | 40dvh |
| 桌面 / 大平板（≥ 900px 横屏） | 三栏：画面 / 按钮 / 设置常驻 | 自适应 |

按钮区内部从上到下两段：分段切换（场景 / 页面按钮）→ 可滚动列表；底部只剩一行常驻的
四个按钮：`左滑 / 右滑 / 上一页 / 下一页`（高频，放最底部最好按，等宽一行不折行）。
场景列表为单列平铺，每行「名称 + URL」，不再用多列卡片。

低频操作全部收进顶栏右上角的图标组，不占按钮区空间：

| 顺序 | 图标 | 作用 | 说明 |
|------|------|------|------|
| 1 | ⚙ | 推流设置 | 桌面端设置常驻侧栏，此图标隐藏 |
| 2 | 四角外扩 / 内收 | 全屏 / 退出全屏 | 激活时换成内收图标并高亮，顶栏同时显示「全屏中」标签 |
| 3 | 窗口 + 上箭头 | 前台 | 把 Chrome 窗口提回最前 |
| 4 | 电源 | 重启 Chrome | 请求中禁用并旋转 |
| 5 | 双向循环箭头 | 刷新重连 | 丢弃画面缓存 + 关旧 WS + 立即新建连接，不等 1.5s 退避 |

图标为内联 SVG（`stroke: currentColor`，18px），不依赖字体与外部资源；每个都有 `title` / `aria-label`。

触控细节：所有操作按钮最小 54px 高（场景行 52px）、`-webkit-tap-highlight-color: transparent`、
`overscroll-behavior: none` 防下拉刷新、`env(safe-area-inset-*)` 适配刘海屏、输入框 16px 字号防 iOS 自动缩放、
按钮按下 `navigator.vibrate(8)` 给触感反馈。

---

## 8. 性能实测（本机 M 系列 / 2560×1600 Retina / 逻辑 1440×900）

| 指标 | 实测 |
|------|------|
| 首帧（冷） | 100-500ms |
| 服务端单帧开销 | 只有 base64 解码 + 一次 WS 写，不碰像素 |
| 单帧体积 @780 宽 q60 | 纯色页 8KB；真实桌面 34KB |
| 单帧体积 @1400 宽 q60 | 21KB |
| 静止页面 | 1.5 秒内 1 帧（Chrome 不推重复帧） |
| 面板切换生效 | ≤500ms（轮询周期） |
| 空闲 CPU | ≈0（`streaming` false 时连 screencast 都不开） |

降码率只剩一个旋钮：`quality 60→45`。分辨率由面板决定，不再手动调。

### 8.1 降采样：captureScreenshot 走不通，screencast 可以

想降分辨率只有两种办法，实测结论截然相反：

| 方案 | 结果 |
|------|------|
| `Page.captureScreenshot` + `clip.scale ≠ 1` | ❌ Chrome 把显示器上正在渲染的 surface 一起按 scale 重新栅格化，窗口内容瞬时缩小、露白、再还原。8fps 下就是每秒 8 次闪白：亮度极差 **31.4** / 标准差 **14.20**（基线 0.6 / 0.18） |
| `Page.startScreencast` + `maxWidth/maxHeight` | ✅ 复制一份 frame 再缩，不动显示面。screencast 运行中拍 1:1 截图，尺寸、体积、清晰度与基线 **100% 一致** |

所以现在的采集**只走 screencast**，`Screenshot`（`captureScreenshot`）已从推流路径移除。

回归：

- `go run ./cmd/screencast` —— 直接跑生产代码路径，检查帧尺寸、静止去重、面板宽度切换。
- `go run ./cmd/verify -k <token> -scene <id>` —— 采样被控窗口亮度，极差明显大于基线说明又在动显示面。

---

## 9. 已知限制

| 项 | 说明 |
|----|------|
| macOS 全屏 | CDP 的 `fullscreen` 状态会被 Chrome 忽略，实际以 `maximized` 落地。肉眼效果是铺满屏幕，但系统层面不是独立全屏空间（少数情况下菜单栏仍在） |
| 只控制 Chrome | 按需求设计。桌面其他窗口不接收任何输入 |
| 单屏 | 多显示器只接管 Chrome 所在窗口，暂不支持切换显示器 |
| 无键盘输入 | 只做鼠标 / 滚轮 / 手势；搜索框输入需要另加 `Input.insertText` |
| 新标签页 | 已用注入脚本把 `_blank` 拦回当前页；若页面用非常规方式开新窗口，会新开一个带 UI 的窗口（已知降级） |
| 权限 | 无需任何系统授权；但如果用户误把 profile 指向日常 Chrome，会污染其会话 |
| 按钮识别上限 | 单页最多列 60 个（`total` 仍是真实数量）；只扫主文档，**iframe 内的元素扫不到** |
| 动态内容 | 滚动加载 / 异步渲染出来的新按钮不会自动出现，点「刷新」或等下一次页面跳转 |
| 帧体积 | 按客户端面板宽度下发（780 宽 q60 约 8-34KB/帧）。屏幕越大帧越大，调低 `quality` 即可 |
| 帧率不可控 | screencast 由页面变化驱动 + ack 节流，没有帧率参数。需要「即使静止也保持固定帧率」的场景不适用 |
| 反自动化 | 部分站点（如百度搜索结果）识别到 CDP 点击会弹验证码。这是站点策略，不是程序缺陷，人工点也一样会弹 |
| 按钮文本 | 纯图形按钮（无 alt / aria-label / 文本）会显示成 `(标签名)`，点得动但看不出是什么 |

---

## 10. 后续可做（按价值排序）

1. **键盘输入** — `Input.insertText` + 虚拟键盘，让搜索框真正可用
2. **场景热加载** — fsnotify 监听 yaml，改配置不用重启
3. **WebP / 区域增量** — 静态页面下带宽还能降一个数量级
4. **多屏切换** — 按 `displayId` 选目标窗口
5. **Windows 适配** — `windowState=fullscreen` 在 Windows 上是 F11 式，比 macOS 干净，只需验证 DPI
