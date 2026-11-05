package main

// verify：端到端验证「推流时被控窗口是否闪烁」。
// 1) 从 DevToolsActivePort 找到 deskctl 启动的 Chrome，拿窗口在屏幕上的位置
// 2) 连 deskctl 的 WS 开推流
// 3) 边收帧边用系统截屏采样窗口内部区域的平均亮度
// 不闪烁时亮度应恒定；闪烁时会出现明显的亮度尖峰（留白）。
import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/png"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

func devtoolsPort(userDataDir string) (int, string) {
	b, err := os.ReadFile(userDataDir + "/DevToolsActivePort")
	if err != nil {
		return 0, ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 {
		return 0, ""
	}
	var port int
	fmt.Sscanf(lines[0], "%d", &port)
	return port, lines[1]
}

var (
	cdp     *websocket.Conn
	cdpID   int64
	browser struct {
		Left, Top, Width, Height int
	}
)

func cdpCall(method string, params any, out any) error {
	cdpID++
	id := cdpID
	m := map[string]any{"id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	b, _ := json.Marshal(m)
	if err := cdp.WriteMessage(websocket.TextMessage, b); err != nil {
		return err
	}
	for {
		_, d, err := cdp.ReadMessage()
		if err != nil {
			return err
		}
		var r struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(d, &r)
		if r.ID != id {
			continue
		}
		if out != nil && len(r.Result) > 0 {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	}
}

// meanBrightness 截取屏幕指定区域并计算平均亮度（0-255）
func meanBrightness(region string) float64 {
	tmp := "/tmp/verify_shot.png"
	_ = exec.Command("screencapture", "-x", "-R", region, "-t", "png", tmp).Run()
	f, err := os.Open(tmp)
	if err != nil {
		return -1
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return -1
	}
	b := img.Bounds()
	var sum, n float64
	for y := b.Min.Y; y < b.Max.Y; y += 3 {
		for x := b.Min.X; x < b.Max.X; x += 3 {
			r, g, bb, _ := img.At(x, y).RGBA()
			sum += float64(r+g+bb) / 3 / 257
			n++
		}
	}
	if n == 0 {
		return -1
	}
	return sum / n
}

type sample struct {
	v float64
	t time.Time
}

func stats(name string, xs []float64) {
	if len(xs) == 0 {
		fmt.Printf("%s: 无样本\n", name)
		return
	}
	lo, hi, sum := xs[0], xs[0], 0.0
	for _, v := range xs {
		sum += v
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	mean := sum / float64(len(xs))
	var sd float64
	for _, v := range xs {
		sd += (v - mean) * (v - mean)
	}
	sd = math.Sqrt(sd / float64(len(xs)))
	fmt.Printf("%-10s 样本=%2d  均值=%6.2f  最小=%6.2f  最大=%6.2f  极差=%5.2f  标准差=%5.2f\n",
		name, len(xs), mean, lo, hi, hi-lo, sd)
}

func main() {
	token := flag.String("k", "", "登录口令")
	profile := flag.String("profile", "/tmp/dctest/profile", "Chrome user-data-dir")
	scene := flag.String("scene", "", "先执行的场景 id")
	wsURL := flag.String("ws", "127.0.0.1:7788", "deskctl 地址")
	flag.Parse()

	// Chrome 是懒启动的：先通过 WS 触发场景，等 DevToolsActivePort 落地
	if *scene != "" {
		if _, err := os.Stat(*profile + "/DevToolsActivePort"); err != nil {
			pre, _, e := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/ws?k=%s", *wsURL, *token), nil)
			if e == nil {
				_ = pre.WriteJSON(map[string]any{"type": "scenes.run", "id": *scene})
				time.Sleep(5 * time.Second)
				pre.Close()
			}
		}
	}

	// --- 定位 Chrome 窗口 ---
	port, browserPath := devtoolsPort(*profile)
	if port == 0 {
		fmt.Println("读取 DevToolsActivePort 失败")
		os.Exit(1)
	}
	// 优先用 /json/version 给的地址，DevToolsActivePort 的 browser 路径有时握手被拒
	wsEndpoint := fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser/%s", port, browserPath)
	if resp, e := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port)); e == nil {
		var v struct {
			W string `json:"webSocketDebuggerUrl"`
		}
		if json.NewDecoder(resp.Body).Decode(&v) == nil && v.W != "" {
			wsEndpoint = v.W
		}
		resp.Body.Close()
	}
	var err error
	cdp, _, err = websocket.DefaultDialer.Dial(wsEndpoint, nil)
	if err != nil {
		fmt.Println("CDP dial:", err)
		os.Exit(1)
	}
	cdp.SetReadLimit(64 << 20)

	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := cdpCall("Target.getTargets", nil, &targets); err != nil {
		fmt.Println("getTargets:", err)
		os.Exit(1)
	}
	tid := ""
	for _, t := range targets.TargetInfos {
		if t.Type == "page" {
			tid = t.TargetID
		}
	}
	var win struct {
		Bounds struct{ Left, Top, Width, Height int }
	}
	if err := cdpCall("Browser.getWindowForTarget", map[string]any{"targetId": tid}, &win); err != nil {
		fmt.Println("getWindow:", err)
		os.Exit(1)
	}
	browser = win.Bounds

	// 采样窗口内部：避开车窗圆角与标题栏，取中间一块纯内容区
	region := fmt.Sprintf("%d,%d,%d,%d",
		browser.Left+120, browser.Top+160, browser.Width-240, browser.Height-280)
	fmt.Printf("窗口=%+v\n采样区=%s\n", browser, region)

	// --- 连 deskctl ---
	u := fmt.Sprintf("ws://%s/ws?k=%s", *wsURL, *token)
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		fmt.Println("ws dial:", err)
		os.Exit(1)
	}
	defer c.Close()
	c.SetReadLimit(32 << 20)

	var frames int64
	var bytes int64
	var lastHead struct {
		IW, IH, CW, CH, MS, Bytes int
	}
	go func() {
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt != websocket.BinaryMessage || len(data) < 4 {
				continue
			}
			n := binary.BigEndian.Uint32(data)
			if int(n)+4 > len(data) {
				continue
			}
			var h struct {
				IW, IH, CW, CH, MS, Bytes int
			}
			if json.Unmarshal(data[4:4+n], &h) == nil {
				lastHead.IW, lastHead.IH, lastHead.CW, lastHead.CH = h.IW, h.IH, h.CW, h.CH
				lastHead.MS, lastHead.Bytes = h.MS, h.Bytes
			}
			atomic.AddInt64(&frames, 1)
			atomic.AddInt64(&bytes, int64(len(data)))
		}
	}()

	if *scene != "" {
		_ = c.WriteJSON(map[string]any{"type": "scenes.run", "id": *scene})
		time.Sleep(4 * time.Second)
	}
	time.Sleep(1500 * time.Millisecond)

	// --- 基线（未开推流） ---
	var base []float64
	for i := 0; i < 10; i++ {
		base = append(base, meanBrightness(region))
	}
	stats("基线", base)

	// --- 开推流 ---
	atomic.StoreInt64(&frames, 0)
	atomic.StoreInt64(&bytes, 0)
	if err := c.WriteJSON(map[string]any{"type": "stream.start", "fps": 8, "quality": 60}); err != nil {
		fmt.Println("start:", err)
		os.Exit(1)
	}

	var during []float64
	start := time.Now()
	for time.Since(start) < 6*time.Second {
		during = append(during, meanBrightness(region))
	}
	el := time.Since(start)
	n := atomic.LoadInt64(&frames)
	stats("推流中", during)
	fmt.Printf("推流 %.1fs: 帧数=%d (%.1f fps) 头=%dx%d CSS=%dx%d 编码=%dms 单帧=%.0fKB\n",
		el.Seconds(), n, float64(n)/el.Seconds(),
		lastHead.IW, lastHead.IH, lastHead.CW, lastHead.CH, lastHead.MS, float64(lastHead.Bytes)/1024)

	_ = c.WriteJSON(map[string]any{"type": "stream.stop"})
}
