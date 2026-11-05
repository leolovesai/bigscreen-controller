package chrome

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"deskctl/internal/config"
)

var (
	ErrNotReady = errors.New("chrome 尚未就绪")
	ErrTimeout  = errors.New("等待页面加载超时")
)

type Status struct {
	Running     bool   `json:"running"`
	Ready       bool   `json:"ready"`
	Port        int    `json:"port"`
	PID         int    `json:"pid"`
	TargetID    string `json:"targetId"`
	URL         string `json:"url"`
	Fullscreen  bool   `json:"fullscreen"`
	WindowState string `json:"windowState"`
	ViewportW   int    `json:"viewportW"`
	ViewportH   int    `json:"viewportH"`
	Err         string `json:"err,omitempty"`
}

type Manager struct {
	cfg config.Chrome

	mu               sync.Mutex
	conn             *cdpConn
	session          string
	targetID         string
	port             int
	pid              int
	owned            bool
	fullscreen       bool
	winState         string
	cssW, cssH       int
	screenW, screenH int
	dpr              float64
	url              string
	lastErr          string

	castMu sync.Mutex
	castCh chan Frame
}

func NewManager(cfg config.Chrome) *Manager {
	return &Manager{cfg: cfg}
}

// Ensure 保证 Chrome 已启动且已 attach 到一个页面 target。
func (m *Manager) Ensure(ctx context.Context) error {
	m.mu.Lock()
	conn := m.conn
	m.mu.Unlock()
	if conn != nil {
		select {
		case <-conn.closed:
			// 连接断了，走重建流程
		default:
			return nil
		}
	}

	if err := m.launch(ctx); err != nil {
		m.setErr(err)
		return err
	}
	if err := m.connect(ctx); err != nil {
		m.setErr(err)
		return err
	}
	return nil
}

func (m *Manager) setErr(err error) {
	m.mu.Lock()
	if err != nil {
		m.lastErr = err.Error()
	}
	m.mu.Unlock()
}

func (m *Manager) launch(ctx context.Context) error {
	if m.cfg.Binary == "" {
		return errors.New("未找到 Chrome 可执行文件，请在 config.yaml 配置 chrome.binary")
	}
	if err := os.MkdirAll(m.cfg.UserDataDir, 0o755); err != nil {
		return fmt.Errorf("创建 profile 目录失败: %w", err)
	}

	portFile := filepath.Join(m.cfg.UserDataDir, "DevToolsActivePort")

	// 上次的实例还活着就直接复用，避免 profile 被锁。
	if p, ok := readActivePort(portFile); ok && endpointAlive(p) {
		m.mu.Lock()
		m.port, m.owned = p, false
		m.mu.Unlock()
		return nil
	}
	_ = os.Remove(portFile)

	args := []string{
		"--user-data-dir=" + m.cfg.UserDataDir,
		"--remote-debugging-port=0",
		fmt.Sprintf("--window-size=%d,%d", m.cfg.WindowSize[0], m.cfg.WindowSize[1]),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-session-crashed-bubble",
		"--hide-crash-restore-bubble",
	}
	args = append(args, m.cfg.ExtraArgs...)
	// app 模式没有标签栏和地址栏，全屏后才是真正的纯网页。
	// 注意 about:blank 会被 Chrome 当成普通标签页忽略，必须给一个真实可打开的 URL。
	// 这个 data 页就是被控窗口的「待机画面」，场景一导航它就被替换掉，所以文案要能自解释。
	if m.cfg.AppMode {
		args = append(args, "--app=data:text/html;base64,"+starterPageB64)
	} else {
		args = append(args, "about:blank")
	}

	cmd := exec.Command(m.cfg.Binary, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 Chrome 失败: %w", err)
	}

	m.mu.Lock()
	m.pid, m.owned = cmd.Process.Pid, true
	m.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		m.conn = nil
		m.session = ""
		m.owned = false
		m.mu.Unlock()
	}()

	// 等 Chrome 写出调试端口
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if p, ok := readActivePort(portFile); ok && endpointAlive(p) {
			m.mu.Lock()
			m.port = p
			m.mu.Unlock()
			return nil
		}
		time.Sleep(120 * time.Millisecond)
	}
	return errors.New("等待 Chrome 调试端口超时（profile 可能被占用）")
}

func readActivePort(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 0 {
		return 0, false
	}
	p, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || p <= 0 {
		return 0, false
	}
	return p, true
}

func endpointAlive(port int) bool {
	c := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == 200
}

func (m *Manager) connect(ctx context.Context) error {
	m.mu.Lock()
	port := m.port
	m.mu.Unlock()
	if port == 0 {
		return errors.New("调试端口未知")
	}

	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return fmt.Errorf("读取 CDP 版本信息失败: %w", err)
	}
	var ver struct {
		Browser              string `json:"Browser"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	err = json.NewDecoder(resp.Body).Decode(&ver)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("解析 CDP 版本信息失败: %w", err)
	}

	conn, err := dialCDP(ver.WebSocketDebuggerURL)
	if err != nil {
		return err
	}

	// --app=data: 窗口从进程启动到出现在 CDP target 列表里要几百毫秒。
	// 这期间 getTargets 可能是空的，若立刻 createTarget 会额外开一个窗口：
	// 业务页跑到新窗口里，原来的 app 窗口就变成一块黑屏留在桌面上。
	// 所以这里先等自己的页面出现，只有真的等不到才自己建。
	var targetID string
	deadline := time.Now().Add(6 * time.Second)
	for {
		var targets struct {
			TargetInfos []pageTarget `json:"targetInfos"`
		}
		if err := conn.call(ctx, "", "Target.getTargets", nil, &targets); err != nil {
			conn.Close()
			return err
		}
		targetID = pickPageTarget(targets.TargetInfos)
		if targetID != "" || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			conn.Close()
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	if targetID == "" {
		var created struct {
			TargetID string `json:"targetId"`
		}
		if err := conn.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
			conn.Close()
			return err
		}
		targetID = created.TargetID
	}

	var attach struct {
		SessionID string `json:"sessionId"`
	}
	if err := conn.call(ctx, "", "Target.attachToTarget", map[string]any{
		"targetId": targetID,
		"flatten":  true,
	}, &attach); err != nil {
		conn.Close()
		return err
	}

	_ = conn.call(ctx, attach.SessionID, "Page.enable", nil, nil)
	_ = conn.call(ctx, attach.SessionID, "Runtime.enable", nil, nil)
	m.installLinkIntercept(ctx, conn, attach.SessionID)

	m.mu.Lock()
	m.conn = conn
	m.session = attach.SessionID
	m.targetID = targetID
	m.lastErr = ""
	m.mu.Unlock()

	// 监听新开标签页：点 target=_blank 的链接时把控制权交给新页面
	conn.SetOnEvent(m.handleEvent)
	_ = conn.call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil)

	m.refreshMetrics(ctx)
	return nil
}

// starterPageB64 是 --app 的待机画面：deskctl / 等待控制器选择场景。
// 用 base64 是为了避免中文和引号在命令行参数里被转义坏。
const starterPageB64 = "PCFET0NUWVBFIGh0bWw+PGh0bWw+PGhlYWQ+PG1ldGEgY2hhcnNldD0idXRmLTgiPjx0aXRsZT5kZXNrY3RsPC90aXRsZT4KPHN0eWxlPmh0bWwsYm9keXttYXJnaW46MDtoZWlnaHQ6MTAwJTtiYWNrZ3JvdW5kOiMwZDExMTc7ZGlzcGxheTpmbGV4O2FsaWduLWl0ZW1zOmNlbnRlcjsKanVzdGlmeS1jb250ZW50OmNlbnRlcjtmb250OjE0cHgvMS43IC1hcHBsZS1zeXN0ZW0sQmxpbmtNYWNTeXN0ZW1Gb250LCJQaW5nRmFuZyBTQyIsc2Fucy1zZXJpZjtjb2xvcjojNmU3NjgxfQouYnt0ZXh0LWFsaWduOmNlbnRlcn0udHtmb250LXNpemU6MTZweDtjb2xvcjojYWRiYWM3O2xldHRlci1zcGFjaW5nOi4wOGVtfS5ze21hcmdpbi10b3A6MTJweDtmb250LXNpemU6MTNweH08L3N0eWxlPgo8L2hlYWQ+PGJvZHk+PGRpdiBjbGFzcz0iYiI+PGRpdiBjbGFzcz0idCI+ZGVza2N0bDwvZGl2PjxkaXYgY2xhc3M9InMiPuetieW+heaOp+WItuWZqOmAieaLqeWcuuaZrzwvZGl2PjwvZGl2PjwvYm9keT48L2h0bWw+"

type pageTarget struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	URL      string `json:"url"`
}

// pickPageTarget 在多个页面里挑一个真正该被控制的窗口。
// 优先级：自己的 app 启动页 → 已加载的真实页面 → about:blank → chrome:// 之类内置页。
// 关键是别把 chrome://newtab 当成业务页，否则会出现「业务页在一个窗口、黑屏在另一个窗口」。
func pickPageTarget(infos []pageTarget) string {
	var starter, real, blank, chromePage string
	for _, t := range infos {
		if t.Type != "page" || t.TargetID == "" {
			continue
		}
		switch {
		case strings.HasPrefix(t.URL, "data:text/html"):
			if starter == "" {
				starter = t.TargetID
			}
		case t.URL == "" || t.URL == "about:blank":
			if blank == "" {
				blank = t.TargetID
			}
		case strings.HasPrefix(t.URL, "chrome://"):
			if chromePage == "" {
				chromePage = t.TargetID
			}
		default:
			if real == "" {
				real = t.TargetID
			}
		}
	}
	for _, id := range []string{starter, real, blank, chromePage} {
		if id != "" {
			return id
		}
	}
	return ""
}

// linkInterceptJS 注入到每个新文档：把 target=_blank 的打开方式改成当前页导航。
// 否则 app 模式会新开一个带 UI 的普通窗口，全屏画面直接退化。
const linkInterceptJS = `(function(){
  if (window.__deskctl_links__) return;
  window.__deskctl_links__ = 1;
  document.addEventListener('click', function(e){
    var el = e.target;
    while (el && el.nodeName !== 'A') el = el.parentElement;
    if (el && (el.target === '_blank' || el.target === '_new')) {
      el.target = '_self';
    }
  }, true);
  var rawOpen = window.open;
  window.open = function(u){
    if (typeof u === 'string' && u) { location.href = u; return null; }
    return rawOpen.apply(window, arguments);
  };
})();`

func (m *Manager) installLinkIntercept(ctx context.Context, conn *cdpConn, session string) {
	if err := conn.call(ctx, session, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": linkInterceptJS,
	}, nil); err != nil {
		log.Printf("[deskctl] 注入链接拦截失败: %v", err)
	}
}

func (m *Manager) handleEvent(session, method string, params json.RawMessage) {
	if method == "Page.screencastFrame" {
		m.onScreencastFrame(session, params)
		return
	}
	if method != "Target.targetCreated" {
		return
	}
	var p struct {
		TargetInfo struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
			OpenerID string `json:"openerId"`
		} `json:"targetInfo"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	ti := p.TargetInfo
	if ti.Type != "page" || ti.TargetID == "" || ti.URL == "" || ti.URL == "about:blank" {
		return
	}

	m.mu.Lock()
	cur := m.targetID
	m.mu.Unlock()

	// 只接管由当前页面打开的子页面，不去抢用户在其他窗口的操作
	if ti.OpenerID != cur {
		return
	}
	m.adoptTarget(ti.TargetID)
}

// adoptTarget 把控制器切到另一个页面 target。
func (m *Manager) adoptTarget(targetID string) {
	m.mu.Lock()
	conn := m.conn
	m.mu.Unlock()
	if conn == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	var at struct {
		SessionID string `json:"sessionId"`
	}
	if err := conn.call(ctx, "", "Target.attachToTarget", map[string]any{
		"targetId": targetID,
		"flatten":  true,
	}, &at); err != nil {
		return
	}
	_ = conn.call(ctx, at.SessionID, "Page.enable", nil, nil)
	_ = conn.call(ctx, at.SessionID, "Runtime.enable", nil, nil)
	m.installLinkIntercept(ctx, conn, at.SessionID)
	_ = conn.call(ctx, "", "Target.activateTarget", map[string]any{"targetId": targetID}, nil)

	m.mu.Lock()
	m.session = at.SessionID
	m.targetID = targetID
	m.mu.Unlock()

	select {
	case <-ctx.Done():
	case <-time.After(400 * time.Millisecond):
	}
	m.refreshMetrics(ctx)
}

func (m *Manager) sess() (*cdpConn, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn == nil || m.session == "" {
		return nil, "", ErrNotReady
	}
	return m.conn, m.session, nil
}

// Navigate 打开 URL 并等加载完成。
func (m *Manager) Navigate(ctx context.Context, rawURL, wait string, timeoutMs int) error {
	if err := m.Ensure(ctx); err != nil {
		return err
	}
	conn, session, err := m.sess()
	if err != nil {
		return err
	}

	// 让当前页所在的窗口/标签前置，避免命中后台标签。
	m.mu.Lock()
	targetID := m.targetID
	m.mu.Unlock()
	_ = conn.call(ctx, "", "Target.activateTarget", map[string]any{"targetId": targetID}, nil)

	if err := conn.call(ctx, session, "Page.navigate", map[string]any{"url": rawURL}, nil); err != nil {
		return err
	}
	if wait == "none" {
		return nil
	}
	// 超时不等于失败：SPA（如 #/index 先跳登录页）常有一个长期 pending 的资源，
	// readyState 卡在 interactive，但页面其实已经能看能点了。此时继续走全屏和扫描。
	if err := m.waitLoaded(ctx, timeoutMs); err != nil && !errors.Is(err, ErrTimeout) {
		return err
	}
	m.refreshMetrics(ctx)
	return nil
}

func (m *Manager) waitLoaded(ctx context.Context, timeoutMs int) error {
	conn, session, err := m.sess()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		var r struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := conn.call(callCtx, session, "Runtime.evaluate", map[string]any{
			"expression":    "document.readyState",
			"returnByValue": true,
		}, &r)
		cancel()
		if err == nil && r.Result.Value == "complete" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return ErrTimeout
}

// SetFullscreen 切换全屏。
// 先请求 CDP 的原生全屏；macOS 上 Chrome（尤其 app 窗口）会静默忽略这个状态，
// 此时退回 maximized。判定依据是读回的真实 windowState，不是调用是否报错。
func (m *Manager) SetFullscreen(ctx context.Context, on bool) error {
	if err := m.Ensure(ctx); err != nil {
		return err
	}
	conn, _, err := m.sess()
	if err != nil {
		return err
	}
	m.mu.Lock()
	targetID := m.targetID
	m.mu.Unlock()

	// 冷启动刚拉起的窗口可能还没在窗口管理器里就绪，getWindowForTarget 会短暂失败，
	// 这里重试几次，别让「点场景自动拉起 + 全屏」在这种时序上掉链子。
	var windowID int
	for i := 0; i < 4 && windowID == 0; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(250 * time.Millisecond):
			}
		}
		var w struct {
			WindowID int `json:"windowId"`
		}
		if err := conn.call(ctx, "", "Browser.getWindowForTarget", map[string]any{
			"targetId": targetID,
		}, &w); err == nil {
			windowID = w.WindowID
		}
	}
	if windowID == 0 {
		return errors.New("拿不到窗口 id")
	}

	apply := func(state string) string {
		_ = conn.call(ctx, "", "Browser.setWindowBounds", map[string]any{
			"windowId": windowID,
			"bounds":   map[string]any{"windowState": state},
		}, nil)
		select {
		case <-ctx.Done():
		case <-time.After(900 * time.Millisecond):
		}
		var w struct {
			Bounds struct {
				WindowState string `json:"windowState"`
			} `json:"bounds"`
		}
		_ = conn.call(ctx, "", "Browser.getWindowForTarget", map[string]any{
			"targetId": targetID,
		}, &w)
		return w.Bounds.WindowState
	}

	if on {
		state := apply("fullscreen")
		if state != "fullscreen" {
			// macOS 上会走到这里：原生全屏被忽略，退到最大化
			state = apply("maximized")
		}
		m.mu.Lock()
		m.fullscreen = state == "fullscreen" || state == "maximized"
		m.winState = state
		m.mu.Unlock()
		if !m.fullscreen {
			return fmt.Errorf("窗口仍是 %q，全屏未生效", state)
		}
	} else {
		state := apply("normal")
		m.mu.Lock()
		m.fullscreen = false
		m.winState = state
		m.mu.Unlock()
	}

	m.refreshMetrics(ctx)
	return nil
}

func (m *Manager) Focus(ctx context.Context) error {
	conn, _, err := m.sess()
	if err != nil {
		return err
	}
	m.mu.Lock()
	targetID := m.targetID
	m.mu.Unlock()
	return conn.call(ctx, "", "Target.activateTarget", map[string]any{"targetId": targetID}, nil)
}

// refreshMetrics 缓存 CSS 视口尺寸、屏幕尺寸和当前 URL。
// 鼠标坐标换算依赖 cssW/cssH；全屏判定依赖 screenW/screenH。
func (m *Manager) refreshMetrics(ctx context.Context) {
	conn, session, err := m.sess()
	if err != nil {
		return
	}
	var r struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	expr := `JSON.stringify({iw:innerWidth,ih:innerHeight,sw:screen.width,sh:screen.height,href:location.href,dpr:devicePixelRatio})`
	if err := conn.call(ctx, session, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
	}, &r); err != nil {
		return
	}
	var v struct {
		IW, IH, SW, SH float64
		Href           string
		DPR            float64
	}
	if err := json.Unmarshal([]byte(r.Result.Value), &v); err != nil {
		return
	}
	if v.IW <= 0 || v.IH <= 0 {
		return
	}
	m.mu.Lock()
	m.cssW, m.cssH = int(v.IW), int(v.IH)
	if v.SW > 0 && v.SH > 0 {
		m.screenW, m.screenH = int(v.SW), int(v.SH)
	}
	if v.DPR > 0 {
		m.dpr = v.DPR
	}
	if v.Href != "" {
		m.url = v.Href
	}
	m.mu.Unlock()
}

// Viewport 返回 CSS 视口尺寸，鼠标坐标换算依赖它。
func (m *Manager) Viewport() (int, int) {
	return m.viewport()
}

// RefreshMetrics 供推流循环定时校准 URL 与视口尺寸（页面内跳转不会通知我们）。
func (m *Manager) RefreshMetrics(ctx context.Context) {
	m.refreshMetrics(ctx)
}

func (m *Manager) viewport() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cssW, m.cssH
}

// Frame 是一帧 screencast 画面。Chrome 已经按 maxWidth/maxHeight 降采样过，
// 服务端不需要解码，直接转发。
type Frame struct {
	JPEG       []byte
	CSSW, CSSH int // 页面 CSS 视口，前端换算坐标用
}

// StartScreencast 让 Chrome 开始推帧。maxW/maxH 是降采样的目标框，缩放由 Chrome 内部完成。
// 只有页面内容变化时才推帧，静止时不推——去重也是 Chrome 自己做的。
//
// 注意 screencast 跟 captureScreenshot 的 clip.scale 不是一个东西：clip.scale 会把屏幕上
// 正在渲染的 surface 一起重新栅格化（肉眼看到闪白），screencast 是复制一份 frame 再缩，
// 显示面不受影响。cmd/screencast 里做过清晰度对比验证。
func (m *Manager) StartScreencast(ctx context.Context, maxW, maxH, quality int) (<-chan Frame, error) {
	conn, session, err := m.sess()
	if err != nil {
		return nil, err
	}
	ch := make(chan Frame, 2)
	m.castMu.Lock()
	m.castCh = ch
	m.castMu.Unlock()

	err = conn.call(ctx, session, "Page.startScreencast", map[string]any{
		"format":        "jpeg",
		"quality":       quality,
		"maxWidth":      maxW,
		"maxHeight":     maxH,
		"everyNthFrame": 1,
	}, nil)
	if err != nil {
		m.castMu.Lock()
		m.castCh = nil
		m.castMu.Unlock()
		return nil, err
	}
	return ch, nil
}

// StopScreencast 停流。通道不关闭，消费者靠 ctx 退出，避免写入已关闭通道。
func (m *Manager) StopScreencast(ctx context.Context) {
	m.castMu.Lock()
	m.castCh = nil
	m.castMu.Unlock()
	if conn, session, err := m.sess(); err == nil {
		_ = conn.call(ctx, session, "Page.stopScreencast", nil, nil)
	}
}

// Ping 做一次极轻的 CDP 往返，用来发现 Chrome 掉线——screencast 静止时不推帧，
// 光靠「没有帧」判断不了死活。
func (m *Manager) Ping(ctx context.Context) error {
	conn, session, err := m.sess()
	if err != nil {
		return err
	}
	return conn.call(ctx, session, "Runtime.evaluate", map[string]any{"expression": "0"}, nil)
}

// onScreencastFrame 在 CDP 读循环里执行，任何阻塞都会把整个连接卡死。
func (m *Manager) onScreencastFrame(session string, params json.RawMessage) {
	var p struct {
		Data     string `json:"data"`
		Metadata struct {
			DeviceWidth  float64 `json:"deviceWidth"`
			DeviceHeight float64 `json:"deviceHeight"`
		} `json:"metadata"`
		SessionID int64 `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if conn, _, err := m.sess(); err == nil {
		// 不回 ack，Chrome 推两三帧就停
		conn.notify(session, "Page.screencastFrameAck", map[string]any{"sessionId": p.SessionID})
	}

	m.castMu.Lock()
	ch := m.castCh
	m.castMu.Unlock()
	if ch == nil {
		return
	}
	jpg, err := base64.StdEncoding.DecodeString(p.Data)
	if err != nil {
		return
	}
	f := Frame{JPEG: jpg, CSSW: int(p.Metadata.DeviceWidth), CSSH: int(p.Metadata.DeviceHeight)}
	select {
	case ch <- f:
	default: // 消费端跟不上就丢这一帧，绝不阻塞读循环
	}
}

// Mouse 派发鼠标事件。坐标是 CSS 像素，天然只作用在页面上。
func (m *Manager) Mouse(ctx context.Context, typ string, x, y float64, buttons int) error {
	conn, session, err := m.sess()
	if err != nil {
		return err
	}
	// mouseMoved 必须配 button=none，传 left 会让 Chrome 认为按键状态异常。
	button := "left"
	switch typ {
	case "mouseMoved":
		button = "none"
		buttons = 0
	case "mouseReleased":
		buttons = 0
	}
	return conn.call(ctx, session, "Input.dispatchMouseEvent", map[string]any{
		"type":        typ,
		"x":           x,
		"y":           y,
		"button":      button,
		"buttons":     buttons,
		"clickCount":  1,
		"pointerType": "mouse",
	}, nil)
}

func (m *Manager) Wheel(ctx context.Context, x, y, dx, dy float64) error {
	conn, session, err := m.sess()
	if err != nil {
		return err
	}
	return conn.call(ctx, session, "Input.dispatchMouseEvent", map[string]any{
		"type":   "mouseWheel",
		"x":      x,
		"y":      y,
		"deltaX": dx,
		"deltaY": dy,
	}, nil)
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		Running:     m.pid != 0 || m.conn != nil,
		Ready:       m.conn != nil && m.session != "",
		Port:        m.port,
		PID:         m.pid,
		TargetID:    m.targetID,
		URL:         m.url,
		Fullscreen:  m.fullscreen,
		WindowState: m.winState,
		ViewportW:   m.cssW,
		ViewportH:   m.cssH,
		Err:         m.lastErr,
	}
	return st
}

// Restart 只杀自己拉起的实例。
func (m *Manager) Restart(ctx context.Context) error {
	m.mu.Lock()
	conn, pid, owned := m.conn, m.pid, m.owned
	m.conn, m.session, m.targetID = nil, "", ""
	m.mu.Unlock()

	if conn != nil {
		conn.Close()
	}
	if owned && pid != 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(400 * time.Millisecond):
	}
	return m.Ensure(ctx)
}

func (m *Manager) Close() {
	m.mu.Lock()
	conn, pid, owned := m.conn, m.pid, m.owned
	m.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
	if owned && pid != 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(os.Interrupt)
		}
	}
}

// Describe 用于启动时打印。
func (m *Manager) Describe() string {
	return fmt.Sprintf("%s (%s)", filepath.Base(m.cfg.Binary), runtime.GOOS)
}
