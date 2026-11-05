// screencast 集成探针：直接跑 deskctl 生产代码路径（chrome.Manager），
// 验证 StartScreencast / StopScreencast / Ping 与面板宽度切换是否正常。
//
// 用法：go run ./cmd/screencast
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image/jpeg"
	"os"
	"path/filepath"
	"time"

	"deskctl/internal/chrome"
	"deskctl/internal/config"
)

// 页面 2.5 秒后自动开始动画：前 2.5s 测静止去重，之后测变化跟随。
const probeHTML = `<!doctype html><meta charset="utf-8">
<title>sc probe</title>
<style>
 body{margin:0;font:14px/1.6 -apple-system,"PingFang SC",sans-serif;background:#fff;color:#111}
 .bar{background:#0a84ff;color:#fff;padding:14px 20px;font-size:18px;font-weight:600}
 .grid{display:grid;grid-template-columns:repeat(4,1fr);gap:12px;padding:20px}
 .card{border:1px solid #e3e3e3;border-radius:10px;padding:12px}
 .card b{display:block;font-size:22px;color:#0a84ff}
 .txt{padding:0 20px;font-size:13px;color:#333}
 .flip{position:fixed;right:20px;bottom:20px;width:120px;height:60px;background:#34c759;border-radius:8px}
</style>
<div class="bar">startScreencast 探针</div>
<div class="grid">
 <div class="card"><b>1,284</b>设备在线</div><div class="card"><b>97.3%</b>可用率</div>
 <div class="card"><b>23 ms</b>平均响应</div><div class="card"><b>0</b>告警</div>
</div>
<div class="txt">这一行是 13px 的中文正文，用来判断降采样后文字是否还能辨认。</div>
<div class="flip" id="flip"></div>
<script>
 setTimeout(() => {
   let on = false;
   setInterval(() => {
     on = !on;
     document.getElementById('flip').style.background = on ? '#ff3b30' : '#34c759';
     document.querySelector('.bar').textContent = 'tick ' + Date.now();
   }, 200);
 }, 2500);
</script>`

func main() {
	flag.Parse()

	bin := config.DetectChrome()
	if bin == "" {
		fmt.Println("找不到 Chrome")
		os.Exit(1)
	}

	htmlPath := filepath.Join(os.TempDir(), "scprobe.html")
	if err := os.WriteFile(htmlPath, []byte(probeHTML), 0o644); err != nil {
		fmt.Println("写测试页失败:", err)
		os.Exit(1)
	}
	profile, _ := os.MkdirTemp("", "scprobe-prof-")
	defer os.RemoveAll(profile)

	m := chrome.NewManager(config.Chrome{
		Binary:      bin,
		UserDataDir: profile,
		WindowSize:  []int{1200, 800},
		AppMode:     true,
		ExtraArgs:   []string{"--disable-features=Translate,MediaRouter"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	defer m.Close()

	t0 := time.Now()
	if err := m.Ensure(ctx); err != nil {
		fmt.Println("启动 Chrome 失败:", err)
		os.Exit(1)
	}
	if err := m.Navigate(ctx, "file://"+htmlPath, "load", 8000); err != nil {
		fmt.Println("导航失败:", err)
		os.Exit(1)
	}
	time.Sleep(600 * time.Millisecond)
	cssW, cssH := m.Viewport()
	fmt.Printf("页面 CSS 视口 %dx%d，Chrome 就绪耗时 %d ms\n", cssW, cssH, time.Since(t0).Milliseconds())

	pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
	pt := time.Now()
	err := m.Ping(pctx)
	pcancel()
	fmt.Printf("Ping 往返 %d ms（err=%v）\n\n", time.Since(pt).Milliseconds(), err)

	run := func(maxW int, label string) {
		ch, serr := m.StartScreencast(ctx, maxW, maxW*cssH/cssW, 60)
		if serr != nil {
			fmt.Printf("%s StartScreencast 失败: %v\n", label, serr)
			return
		}
		var n, bytesSum, bytesMax int
		var dim string
		var firstAt, lastAt time.Time
		deadline := time.Now().Add(1500 * time.Millisecond)
		for time.Now().Before(deadline) {
			select {
			case f := <-ch:
				n++
				bytesSum += len(f.JPEG)
				if len(f.JPEG) > bytesMax {
					bytesMax = len(f.JPEG)
				}
				if dim == "" {
					if cfg, e := jpeg.DecodeConfig(bytes.NewReader(f.JPEG)); e == nil {
						dim = fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
					}
					firstAt = time.Now()
				}
				lastAt = time.Now()
			case <-time.After(60 * time.Millisecond):
			}
		}
		avg := 0
		if n > 0 {
			avg = bytesSum / n
		}
		var gap int64
		if n > 1 {
			gap = lastAt.Sub(firstAt).Milliseconds() / int64(n-1)
		}
		fmt.Printf("%-14s maxW=%-4d 帧%-3d %-10s 单帧 均%2d/最大%2d KB  间隔≈%d ms\n",
			label, maxW, n, dim, avg/1024, bytesMax/1024, gap)
		m.StopScreencast(ctx)
	}

	// A：页面静止（动画 2.5s 后才开始）
	run(780, "A 静止")
	// B：动画已启动
	time.Sleep(1200 * time.Millisecond)
	run(780, "B 变化中")
	// C/D：模拟面板尺寸变化，验证 maxW 切换生效
	run(1400, "C 面板放大")
	run(600, "D 面板缩小")

	fmt.Println()
	m.StopScreencast(ctx)
	fmt.Println("已停流，退出")
}
