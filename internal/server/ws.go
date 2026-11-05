package server

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"deskctl/internal/chrome"
	"deskctl/internal/config"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 1 << 17,
	CheckOrigin:     checkOrigin,
}

// 只允许同源页面连 WS，防止其他网站 CSRF 到本地端口。
func checkOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

type wsCommand struct {
	Type     string  `json:"type"`
	ID       string  `json:"id"`
	URL      string  `json:"url"`
	On       bool    `json:"on"`
	Quality  int     `json:"quality"`
	Action   string  `json:"action"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Buttons  int     `json:"buttons"`
	Dir      string  `json:"dir"`
	Distance float64 `json:"distance"`
	DX       float64 `json:"dx"`
	DY       float64 `json:"dy"`
	Index    int     `json:"index"`
	T        int64   `json:"t"`
	// W 是客户端画面区域的物理像素宽（Canvas 尺寸），服务端按它决定下发分辨率
	W int `json:"w"`
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, 401, map[string]any{"error": "未登录"})
		return
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wmu sync.Mutex
	writeText := func(v any) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return c.WriteJSON(v)
	}
	writeFrame := func(head []byte, jpg []byte) error {
		buf := make([]byte, 4+len(head)+len(jpg))
		binary.BigEndian.PutUint32(buf, uint32(len(head)))
		copy(buf[4:], head)
		copy(buf[4+len(head):], jpg)
		wmu.Lock()
		defer wmu.Unlock()
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return c.WriteMessage(websocket.BinaryMessage, buf)
	}

	_ = c.SetReadDeadline(time.Now().Add(90 * time.Second))

	subID := s.addSub(writeText)
	defer s.removeSub(subID)

	var streaming atomic.Bool
	quality := int32(s.cfg.Capture.Quality)
	// panelW = 客户端画面的物理像素宽；0 表示还没上报，先按原尺寸下发
	var panelW int32
	var paramsMu sync.Mutex

	st := s.ch.Status()
	_ = writeText(map[string]any{
		"type":    "hello",
		"version": "0.1.0",
		"chrome":  st,
		"scenes":  s.cfg.Scenes,
		"capture": s.cfg.Capture,
		"needPwd": s.cfg.Server.Password != "",
	})

	go s.streamLoop(ctx, &streaming, &quality, &panelW, &paramsMu, writeFrame, writeText)

	// 连上就扫一次，省得等 URL 变化
	if st.Ready {
		go func() {
			sctx, scancel := context.WithTimeout(ctx, 8*time.Second)
			defer scancel()
			_, _ = s.scanAndBroadcast(sctx)
		}()
	}

	var sceneRunning atomic.Bool
	var gestureMu sync.Mutex

	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			break
		}
		_ = c.SetReadDeadline(time.Now().Add(90 * time.Second))

		var cmd wsCommand
		if err := json.Unmarshal(data, &cmd); err != nil {
			continue
		}

		switch cmd.Type {
		case "ping":
			_ = writeText(map[string]any{"type": "pong", "t": cmd.T})

		case "stream.start":
			paramsMu.Lock()
			if cmd.Quality > 0 {
				quality = int32(clampInt(cmd.Quality, 20, 95))
			}
			if cmd.W > 0 {
				panelW = int32(clampInt(cmd.W, 320, 4096))
			}
			paramsMu.Unlock()
			streaming.Store(true)

		case "panel":
			// 面板尺寸变了（窗口拉伸、转屏）只改下发宽度，不动流状态
			paramsMu.Lock()
			if cmd.W > 0 {
				panelW = int32(clampInt(cmd.W, 320, 4096))
			}
			paramsMu.Unlock()

		case "stream.stop":
			streaming.Store(false)

		case "mouse":
			go s.dispatchMouse(cmd)

		case "gesture.swipe":
			if gestureMu.TryLock() {
				go func(g wsCommand) {
					defer gestureMu.Unlock()
					s.swipe(ctx, g.Dir, g.Distance)
				}(cmd)
			}

		case "wheel":
			go s.dispatchWheel(cmd)

		case "scenes.run":
			if !sceneRunning.CompareAndSwap(false, true) {
				continue
			}
			go func(id string) {
				defer sceneRunning.Store(false)
				s.runScene(ctx, id, writeText)
			}(cmd.ID)

		case "chrome.fullscreen":
			go func(on bool) {
				gctx, gcancel := context.WithTimeout(ctx, 8*time.Second)
				defer gcancel()
				err := s.ch.SetFullscreen(gctx, on)
				msg := map[string]any{"type": "chrome.state", "fullscreen": on}
				if err != nil {
					msg["error"] = err.Error()
				}
				_ = writeText(msg)
			}(cmd.On)

		case "chrome.navigate":
			u, err := url.Parse(cmd.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				_ = writeText(map[string]any{"type": "error", "code": "E_BAD_URL", "msg": "只允许 http/https"})
				continue
			}
			go func(raw string) {
				gctx, gcancel := context.WithTimeout(ctx, 20*time.Second)
				defer gcancel()
				if err := s.ch.Navigate(gctx, raw, "load", 12000); err != nil {
					_ = writeText(map[string]any{"type": "error", "code": "E_NAV", "msg": err.Error()})
					return
				}
				_ = writeText(map[string]any{"type": "chrome.state", "url": raw})
			}(cmd.URL)

		case "chrome.focus":
			go func() {
				gctx, gcancel := context.WithTimeout(ctx, 5*time.Second)
				defer gcancel()
				_ = s.ch.Focus(gctx)
			}()

		case "page.scan":
			// Chrome 还没起来（静默启动的常态）就没得扫，静默跳过，不弹错
			if !s.ch.Status().Running {
				continue
			}
			go func() {
				gctx, gcancel := context.WithTimeout(ctx, 8*time.Second)
				defer gcancel()
				if _, err := s.scanAndBroadcast(gctx); err != nil {
					_ = writeText(map[string]any{"type": "error", "code": "E_SCAN", "msg": err.Error()})
				}
			}()

		case "page.press":
			go func(idx int) {
				gctx, gcancel := context.WithTimeout(ctx, 10*time.Second)
				defer gcancel()
				err := s.ch.PressButton(gctx, idx)
				msg := map[string]any{"type": "page.pressed", "index": idx}
				if err != nil {
					msg["error"] = err.Error()
				}
				_ = writeText(msg)
				// 点完页面可能变了（展开菜单、跳转），或者元素已随页面失效，
				// 两种情况都要重扫，重扫到有新内容为止
				select {
				case <-ctx.Done():
					return
				case <-time.After(600 * time.Millisecond):
				}
				s.rescanUntilReady(ctx, 9*time.Second)
			}(cmd.Index)
		}
	}
	cancel()
}

func (s *Server) runScene(ctx context.Context, id string, writeText func(any) error) {
	var target *config.Scene
	for i := range s.cfg.Scenes {
		if s.cfg.Scenes[i].ID == id {
			target = &s.cfg.Scenes[i]
			break
		}
	}
	if target == nil {
		_ = writeText(map[string]any{"type": "scenes.state", "id": id, "phase": "error", "msg": "场景不存在"})
		return
	}

	_ = writeText(map[string]any{"type": "scenes.state", "id": id, "phase": "running"})

	total := time.Duration(target.TimeoutMs+20000) * time.Millisecond
	gctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	// Ensure 是惰性的：Chrome 没起就在这里现拉（冷启动约 3-4s），拉完直接导航
	if err := s.ch.Ensure(gctx); err != nil {
		_ = writeText(map[string]any{"type": "scenes.state", "id": id, "phase": "error", "msg": err.Error()})
		return
	}
	if err := s.ch.Navigate(gctx, target.URL, target.Wait, target.TimeoutMs); err != nil {
		_ = writeText(map[string]any{"type": "scenes.state", "id": id, "phase": "error", "msg": err.Error()})
		return
	}
	if target.Fullscreen {
		if err := s.ch.SetFullscreen(gctx, true); err != nil {
			_ = writeText(map[string]any{"type": "chrome.state", "fullscreen": false, "error": "全屏失败: " + err.Error()})
			_ = writeText(map[string]any{"type": "scenes.state", "id": id, "phase": "ready", "msg": "页面已打开，全屏失败: " + err.Error()})
			return
		}
	}
	// 把真实窗口状态回推给控制器：冷启动时前端没走过全屏按钮，不同步的话
	// 「全屏中」角标和顶栏图标还是旧的，看起来像没进全屏。
	st := s.ch.Status()
	_ = writeText(map[string]any{"type": "chrome.state", "fullscreen": st.Fullscreen, "windowState": st.WindowState})
	_ = writeText(map[string]any{"type": "scenes.state", "id": id, "phase": "ready"})

	// 场景切完立刻扫一次，别等 URL 轮询发现
	sctx, scancel := context.WithTimeout(ctx, 8*time.Second)
	defer scancel()
	_, _ = s.scanAndBroadcast(sctx)
}

// dispatchMouse 把归一化坐标换成 CSS 像素后派发给页面。
func (s *Server) dispatchMouse(cmd wsCommand) {
	cw, ch := s.ch.Viewport()
	if cw <= 0 || ch <= 0 {
		return
	}
	x := clamp01(cmd.X) * float64(cw)
	y := clamp01(cmd.Y) * float64(ch)

	typ := "mouseMoved"
	switch cmd.Action {
	case "down":
		typ = "mousePressed"
	case "up":
		typ = "mouseReleased"
	case "move":
		typ = "mouseMoved"
	case "click":
		// 先 hover 定位再按下抬起，很多页面依赖 mousemove 建立 hover 态
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_ = s.ch.Mouse(ctx, "mouseMoved", x, y, 0)
		time.Sleep(12 * time.Millisecond)
		_ = s.ch.Mouse(ctx, "mousePressed", x, y, 1)
		time.Sleep(12 * time.Millisecond)
		_ = s.ch.Mouse(ctx, "mouseReleased", x, y, 0)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	buttons := cmd.Buttons
	if cmd.Action == "up" {
		buttons = 0
	}
	_ = s.ch.Mouse(ctx, typ, x, y, buttons)
}

func (s *Server) dispatchWheel(cmd wsCommand) {
	cw, ch := s.ch.Viewport()
	if cw <= 0 || ch <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_ = s.ch.Wheel(ctx, clamp01(cmd.X)*float64(cw), clamp01(cmd.Y)*float64(ch), cmd.DX, cmd.DY)
}

// swipe 在服务端合成完整手势：即使网页卡顿，位移也是完整的。
func (s *Server) swipe(ctx context.Context, dir string, distance float64) {
	cw, ch := s.ch.Viewport()
	if cw <= 0 || ch <= 0 {
		return
	}
	if distance <= 0 || distance > 0.9 {
		distance = 0.35
	}
	cx, cy := float64(cw)/2, float64(ch)/2
	span := distance * float64(cw)

	x0, y0 := cx, cy
	x1, y1 := cx, cy
	switch dir {
	case "left":
		x0, x1 = cx+span/2, cx-span/2
	case "right":
		x0, x1 = cx-span/2, cx+span/2
	case "up":
		y0, y1 = cy+span/2, cy-span/2
	case "down":
		y0, y1 = cy-span/2, cy+span/2
	default:
		return
	}

	gctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	if err := s.ch.Mouse(gctx, "mousePressed", x0, y0, 1); err != nil {
		return
	}
	steps := 12
	for i := 1; i <= steps; i++ {
		t := easeOutCubic(float64(i) / float64(steps))
		if err := s.ch.Mouse(gctx, "mouseMoved", x0+(x1-x0)*t, y0+(y1-y0)*t, 1); err != nil {
			break
		}
		select {
		case <-gctx.Done():
			break
		case <-time.After(8 * time.Millisecond):
		}
	}
	// 无论如何都要补 released，否则页面可能停在拖拽态
	_ = s.ch.Mouse(gctx, "mouseReleased", x1, y1, 0)
}

// streamLoop 消费 Chrome 推来的帧，原样转发给客户端。
//
// 帧率不由这里控制：screencast 是变化驱动 + ack 节流，页面不动就一帧都不推，
// 缩放也在 Chrome 内部完成，服务端不碰像素。
func (s *Server) streamLoop(
	ctx context.Context,
	streaming *atomic.Bool,
	quality *int32, panelW *int32,
	paramsMu *sync.Mutex,
	writeFrame func([]byte, []byte) error,
	writeText func(any) error,
) {
	var frames <-chan chrome.Frame
	var runW, runQ int
	var seq int64
	var lastPing time.Time
	var fails int
	defer s.ch.StopScreencast(context.Background())

	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if !streaming.Load() {
			if frames != nil {
				s.ch.StopScreencast(ctx)
				frames, runW, runQ = nil, 0, 0
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}

		paramsMu.Lock()
		q, w := int(*quality), int(*panelW)
		paramsMu.Unlock()

		cssW, cssH := s.ch.Viewport()
		maxW, maxH := castSize(w, cssW, cssH)

		// 面板尺寸或清晰度变了要重启：Chrome 只在 start 时读 maxWidth/quality
		if frames == nil || maxW != runW || q != runQ {
			if frames != nil {
				s.ch.StopScreencast(ctx)
				frames = nil
			}
			f, err := s.ch.StartScreencast(ctx, maxW, maxH, q)
			if err != nil {
				// Chrome 没起来或正在导航，单次失败很常见，别把流掐死
				fails++
				if fails == 1 || fails%10 == 0 {
					_ = writeText(map[string]any{"type": "error", "code": "E_SHOT", "msg": err.Error()})
				}
				if fails >= 40 {
					fails = 0
					streaming.Store(false)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(300 * time.Millisecond):
				}
				continue
			}
			fails = 0
			frames, runW, runQ = f, maxW, q
		}

		select {
		case <-ctx.Done():
			return
		case f := <-frames:
			seq++
			head := fmt.Sprintf(`{"seq":%d,"ts":%d,"cw":%d,"ch":%d,"bytes":%d}`,
				seq, time.Now().UnixMilli(), f.CSSW, f.CSSH, len(f.JPEG))
			if err := writeFrame([]byte(head), f.JPEG); err != nil {
				return
			}
		case <-poll.C:
			// 静止时没有帧可等，靠心跳发现 Chrome 掉线
			if time.Since(lastPing) < 3*time.Second {
				continue
			}
			lastPing = time.Now()
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := s.ch.Ping(pctx)
			cancel()
			if err != nil {
				streaming.Store(false)
				_ = writeText(map[string]any{"type": "error", "code": "E_SHOT", "msg": err.Error()})
			}
		}
	}
}

// castSize 把客户端面板宽度换算成 screencast 的降采样目标框。
// 还没收到面板尺寸时按 1x 下发（CSS 宽），够看且不会突然一张 4K。
func castSize(targetW, cssW, cssH int) (int, int) {
	if cssW <= 0 {
		cssW = 1
	}
	if targetW <= 0 {
		targetW = cssW
	}
	maxH := targetW * cssH / cssW
	if maxH < 1 {
		maxH = targetW
	}
	return targetW, maxH
}

func easeOutCubic(t float64) float64 {
	return 1 - math.Pow(1-t, 3)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func contextWithTimeoutSeconds(r *http.Request, sec int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), time.Duration(sec)*time.Second)
}
