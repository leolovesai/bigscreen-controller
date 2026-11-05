# 公司大屏幕网页控制器

[English](README.en.md) · 中文

用手机遥控公司大屏。本地启动一个单文件服务，Chrome 全屏展示业务页面，手机浏览器里实时看到画面，直接点、拖、滑。

适合会议室大屏、值班室监控墙、展厅看板——大屏那台机器没人愿意走过去操作，而操作员手里永远有手机。

不截屏、不注入系统事件、不申请任何系统权限。所有能力走 Chrome DevTools Protocol，只作用于 Chrome 里的页面。

```
┌──────────────┐   HTTP/WS :7788    ┌────────────────┐   CDP(ws)   ┌────────┐
│  手机控制器   │ ◄───────────────► │ deskctl agent  │ ◄─────────► │ Chrome │
│ canvas + 按钮 │  JSON 指令/JPEG 帧 │  Go 单二进制    │             └────────┘
└──────────────┘                    └────────────────┘
```

## 功能

| 能力 | 说明 |
|------|------|
| 场景切换 | 配置里每个场景一个 URL，点一下就在大屏打开并全屏 |
| 实时画面 | CDP screencast 推流，页面静止时零流量、零 CPU |
| 画面直操 | 在缩略图上点击、拖动、滚轮，等同于在大屏上操作 |
| 左右滑手势 | 服务端合成 12 步拖拽，断线也不会卡在按下状态 |
| 页面按钮识别 | 自动扫描当前页可点元素列成清单，点一下等于在页面上点一下 |
| 全屏控制 | 进入 / 退出全屏、把 Chrome 提到前台、重启 Chrome |
| 移动优先界面 | 手机竖屏、横屏、平板、桌面四套布局，按钮最小 54px |
| 扫码即用 | 监听 0.0.0.0 时启动日志直接印局域网二维码，扫码免密进入 |
| 零依赖部署 | 单二进制，前端已 embed，目标机器只需要装 Chrome |

## 快速开始

需要 Go 1.24+ 和 Google Chrome。

```bash
git clone https://github.com/leolovesai/bigscreen-controller.git
cd bigscreen-controller

# 至少改掉 password，再把 scenes 换成你自己的页面
cp config.example.yaml config.yaml

go build -o deskctl .
./deskctl
```

启动后终端输出：

```
[deskctl] 本机 http://127.0.0.1:7788/?k=<token>
[deskctl] 手机/平板 http://192.168.x.x:7788/?k=<token>
         █▀▀▀▀▀█ ▄▀ █▀▀▀▀▀█     ← 扫码直接进入
         █ ███ █ ▀▄ █ ███ █
```

手机扫码或直接打开局域网地址即可。Chrome 不会预热，第一次点场景时才拉起（冷启动 3–4 秒）。

### 命令行参数

| 参数 | 默认 | 说明 |
|------|------|------|
| `-config` | `config.yaml` | 配置文件路径，不存在时按默认值运行 |
| `-addr` | 空 | 覆盖监听地址，如 `0.0.0.0:7788` |
| `-open` | false | 启动后自动打开控制器页面 |
| `-v` | false | 打印版本、平台、Chrome 路径 |

## 配置

```yaml
server:
  host: 0.0.0.0          # 局域网设备可访问；只本机用就改 127.0.0.1
  port: 7788
  password: change-me    # host 非回环时必填，否则启动直接报错
  autoOpen: false        # 服务器场景不该抢焦点

capture:
  quality: 60            # 下发 JPEG 质量，分辨率由客户端面板宽度决定

chrome:
  binary: ""             # 留空自动探测
  userDataDir: ~/.deskctl/chrome-profile
  windowSize: [1512, 945]
  appMode: true          # 无标签栏/地址栏，全屏后是纯网页
  extraArgs:
    - --disable-features=Translate,MediaRouter

scenes:
  - id: dashboard
    name: 数据看板
    url: https://example.com/dashboard
    fullscreen: true
    wait: load
    timeoutMs: 8000
```

新增场景加一段 yaml 即可，重启生效。

> `server.host` 设为 `0.0.0.0` 意味着同一网段内谁都能访问控制台。密码一定要改，或者干脆收回到 `127.0.0.1`。

## 控制器怎么用

**场景**：点一下 → Chrome 拉起、导航、按场景配置进全屏。状态实时回推到顶栏。

**页面按钮**：进入新页面自动扫描可点元素（链接、按钮、`role=button`、`onclick` 容器），按文档位置排序列成一列，点一下就等于在页面上点。滚动加载出来的新按钮需要点右上角刷新。

**画面区**：直接点、拖、滚轮。画面按 contain 居中，留边区域的点击会被丢弃。

**底部四键**（常驻，最高频）：左滑 / 右滑 / 上一页 / 下一页。

**顶栏图标**：推流设置、全屏、前台、重启 Chrome、刷新重连。

## 技术要点

**全部走 CDP，不碰系统 API。** 截图用 `Page.startScreencast`，鼠标用 `Input.dispatchMouseEvent`。代价为零：不需要屏幕录制和辅助功能授权，`CGO_ENABLED=0` 可交叉编译，误触也只可能发生在 Chrome 页面里。

**推流是变化驱动的。** Chrome 只在画面变化时推帧，服务端不解码、不缩放、不编码，收到就转发。没有帧率参数——页面不动就不推。分辨率跟随手机面板宽度，780px 宽就下发 780px，单帧 8–34KB。

**手势在服务端合成。** 12 步 `pressed → moved(easeOut) → released`，中途任何一步失败都用 defer 补一次 released，不会让页面卡在按下状态。

**鉴权三层。** 启动生成 32 位随机 token（`?k=` 免密入口）；访问后种 cookie，URL 变干净；局域网访问强制配密码。WS 校验 Origin 与 Host 同源，防其他网页 CSRF 打到本地端口。密码错误有 1s/IP 限流。

更多实现细节和踩坑记录见 [DESIGN.md](DESIGN.md)，包括 macOS 全屏被静默忽略、`captureScreenshot` 降采样导致闪白、app 窗口延迟出现在 target 列表等问题。

## 调试工具

`cmd/` 下四个独立探针，都直接跑生产代码路径：

```bash
go run ./cmd/probe                     # 冒烟测试：连 WS、跑场景、收帧
go run ./cmd/screencast                # 检查帧尺寸、静止去重、面板宽度切换
go run ./cmd/uiprobe                   # 页面按钮扫描与点击验证
go run ./cmd/verify -k <token> -scene <id>   # 采样被控窗口亮度，检测显示面是否被动过
```

`verify` 是推流方案选型的回归工具：亮度极差明显偏离基线，说明降采样路径又在动显示面。

## 已知限制

| 项 | 说明 |
|----|------|
| 只控制 Chrome | 桌面其他窗口不接收任何输入，这是设计边界 |
| macOS 全屏 | CDP 的 fullscreen 状态会被 Chrome 忽略，实际以 maximized 落地。肉眼铺满屏幕，但系统层面不是独立全屏空间 |
| 无键盘输入 | 只做鼠标 / 滚轮 / 手势，搜索框输入尚未支持 |
| 按钮识别 | 单页最多列 60 条，只扫主文档，iframe 内的元素扫不到 |
| 动态内容 | 滚动 / 异步渲染出的新按钮不会自动出现 |
| 帧率 | 由页面变化驱动，没有固定帧率参数 |
| 站点反自动化 | 部分站点（如搜索引擎结果页）识别到 CDP 点击会弹验证码，人工点也一样 |

## 目录结构

```
bigscreen-controller/
├── main.go                    # 启动、embed 前端、优雅退出、终端二维码
├── internal/
│   ├── config/config.go       # yaml 加载与校验
│   ├── chrome/
│   │   ├── cdp.go             # 极简 CDP 客户端
│   │   ├── elements.go        # 页面可点元素扫描与点击
│   │   └── manager.go         # 进程管理、导航、全屏、推流、鼠标
│   └── server/
│       ├── server.go          # 路由、token/cookie 鉴权
│       └── ws.go              # WS 协议、帧转发、手势合成
├── web/                       # 控制器前端（原生 JS，无构建）
├── cmd/                       # 四个调试探针
├── docs/                      # 界面截图
├── config.example.yaml
└── DESIGN.md                  # 架构与实现说明
```

## 依赖

- [gorilla/websocket](https://github.com/gorilla/websocket) — WebSocket
- [skip2/go-qrcode](https://github.com/skip2/go-qrcode) — 终端局域网二维码
- [gopkg.in/yaml.v3](https://gopkg.in/yaml.v3) — 配置解析

前端不引入任何框架和构建步骤，图标为内联 SVG。

## 许可证

[MIT](LICENSE) © 2026 leo
