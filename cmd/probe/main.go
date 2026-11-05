package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

type frame struct {
	head []byte
	jpg  []byte
}

type probe struct {
	c      *websocket.Conn
	frames chan frame
}

func main() {
	token := flag.String("k", "", "token")
	scene := flag.String("scene", "", "场景 id")
	cx := flag.Float64("cx", -1, "点击 X，归一化 0..1")
	cy := flag.Float64("cy", -1, "点击 Y，归一化 0..1")
	swipe := flag.String("swipe", "", "left/right")
	fs := flag.String("fs", "", "on/off")
	press := flag.Int("press", -1, "点击扫描到的第 N 个页面按钮（0 起）")
	scan := flag.Bool("scan", false, "请求重新扫描页面按钮")
	listen := flag.Duration("listen", 0, "额外监听时长")
	flag.Parse()

	url := fmt.Sprintf("ws://127.0.0.1:7788/ws?k=%s", *token)
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(180 * time.Second))

	p := &probe{c: c, frames: make(chan frame, 4)}
	go p.readLoop()

	if *listen > 0 {
		time.Sleep(*listen)
	}

	if *scene != "" {
		_ = c.WriteJSON(map[string]any{"type": "scenes.run", "id": *scene})
		time.Sleep(5 * time.Second)
	}

	if *scan {
		fmt.Println("scan")
		_ = c.WriteJSON(map[string]any{"type": "page.scan"})
		time.Sleep(1500 * time.Millisecond)
	}

	if *press >= 0 {
		fmt.Println("press", *press)
		_ = c.WriteJSON(map[string]any{"type": "page.press", "index": *press})
		time.Sleep(4 * time.Second)
	}

	if *cx >= 0 && *cy >= 0 {
		fmt.Printf("click (%.3f, %.3f)\n", *cx, *cy)
		_ = c.WriteJSON(map[string]any{"type": "mouse", "action": "click", "x": *cx, "y": *cy})
		time.Sleep(2500 * time.Millisecond)
	}

	if *swipe != "" {
		fmt.Println("swipe", *swipe)
		_ = c.WriteJSON(map[string]any{"type": "gesture.swipe", "dir": *swipe, "distance": 0.35})
		time.Sleep(1500 * time.Millisecond)
	}

	if *fs != "" {
		fmt.Println("fullscreen", *fs)
		_ = c.WriteJSON(map[string]any{"type": "chrome.fullscreen", "on": *fs == "on"})
		time.Sleep(3 * time.Second)
	}

	_ = c.WriteJSON(map[string]any{"type": "stream.start", "fps": 8, "quality": 60})
	if p.readN(1, "/tmp/pre") {
		fmt.Println("ok")
	}
}

// readLoop 独占读，文本消息直接打印，帧丢进 channel。
func (p *probe) readLoop() {
	for {
		typ, data, err := p.c.ReadMessage()
		if err != nil {
			close(p.frames)
			return
		}
		if typ == websocket.TextMessage {
			fmt.Println("  text:", truncate(string(data), 240))
			if f, err := os.OpenFile("/tmp/probe-text.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
				f.Write(data)
				f.Write([]byte("\n"))
				f.Close()
			}
			continue
		}
		if len(data) < 8 {
			continue
		}
		hlen := int(binary.BigEndian.Uint32(data[:4]))
		if 4+hlen > len(data) {
			continue
		}
		select {
		case p.frames <- frame{data[4 : 4+hlen], data[4+hlen:]}:
		default:
		}
	}
}

func (p *probe) readN(n int, prefix string) bool {
	for i := 0; i < n; i++ {
		select {
		case f, ok := <-p.frames:
			if !ok {
				fmt.Println("read: closed")
				return false
			}
			fmt.Printf("  frame %s\n", string(f.head))
			_ = os.WriteFile(fmt.Sprintf("%s%d.jpg", prefix, i), f.jpg, 0o644)
		case <-time.After(12 * time.Second):
			fmt.Println("read: timeout")
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
