package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type req struct {
	ID     int64          `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

type msg struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type page struct {
	WS string `json:"webSocketDebuggerUrl"`
}

func main() {
	port := flag.Int("port", 9333, "debug port")
	token := flag.String("k", "", "cookie token")
	w := flag.Int("w", 390, "viewport width")
	h := flag.Int("h", 844, "viewport height")
	out := flag.String("out", "", "screenshot path")
	runJS := flag.String("js", "", "extra expression to eval before screenshot")
	after := flag.Duration("after", 600*time.Millisecond, "wait after -js before screenshot")
	flag.Parse()

	base := fmt.Sprintf("http://127.0.0.1:%d", *port)
	target := fmt.Sprintf("http://127.0.0.1:7788/?k=%s", *token)

	r, err := http.NewRequest("PUT", base+"/json/new?"+url.QueryEscape(target), nil)
	check(err)
	resp, err := http.DefaultClient.Do(r)
	check(err)
	var p page
	check(json.NewDecoder(resp.Body).Decode(&p))
	resp.Body.Close()

	conn, _, err := websocket.DefaultDialer.Dial(p.WS, nil)
	check(err)
	defer conn.Close()

	var mu sync.Mutex
	var seq int64
	waiters := map[int64]chan *msg{}

	call := func(method string, params map[string]any) json.RawMessage {
		mu.Lock()
		seq++
		id := seq
		ch := make(chan *msg, 1)
		waiters[id] = ch
		mu.Unlock()
		mu.Lock()
		conn.WriteJSON(req{ID: id, Method: method, Params: params})
		mu.Unlock()
		select {
		case m := <-ch:
			if m.Error != nil {
				fmt.Fprintf(os.Stderr, "%s: %s\n", method, m.Error.Message)
			}
			return m.Result
		case <-time.After(15 * time.Second):
			return nil
		}
	}

	go func() {
		for {
			var m msg
			if err := conn.ReadJSON(&m); err != nil {
				return
			}
			if m.ID == 0 {
				continue
			}
			mu.Lock()
			ch := waiters[m.ID]
			delete(waiters, m.ID)
			mu.Unlock()
			if ch != nil {
				ch <- &m
			}
		}
	}()

	call("Emulation.setDeviceMetricsOverride", map[string]any{
		"width": *w, "height": *h, "deviceScaleFactor": 1, "mobile": true,
	})
	call("Page.navigate", map[string]any{"url": target})
	time.Sleep(3 * time.Second)

	js := `(function(){
  const iw = innerWidth, ih = innerHeight;
  const bad = [];
  document.querySelectorAll('*').forEach(function(el){
    const r = el.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) return;
    if (r.right > iw + 0.5 || r.left < -0.5) {
      bad.push(el.tagName.toLowerCase() + (el.id?'#'+el.id:'') + (el.className && typeof el.className==='string' ? '.'+el.className.trim().split(/\s+/).join('.') : '') + ' right=' + Math.round(r.right) + ' w=' + Math.round(r.width));
    }
  });
  return JSON.stringify({
    iw: iw, ih: ih,
    docW: document.documentElement.scrollWidth,
    bodyW: document.body.scrollWidth,
    padW: Math.round((document.querySelector('.pad')||{getBoundingClientRect:()=>({width:0})}).getBoundingClientRect().width),
    stageH: Math.round((document.querySelector('.stage')||{getBoundingClientRect:()=>({height:0})}).getBoundingClientRect().height),
    overflow: bad.slice(0, 12)
  }, null, 1);
})()`

	var eval struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	raw := call("Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true})
	json.Unmarshal(raw, &eval)
	fmt.Println(eval.Result.Value)

	if *runJS != "" {
		call("Runtime.evaluate", map[string]any{"expression": *runJS, "returnByValue": true})
		time.Sleep(*after)
	}

	if *out != "" {
		var shot struct {
			Data string `json:"data"`
		}
		r := call("Page.captureScreenshot", map[string]any{"format": "png"})
		json.Unmarshal(r, &shot)
		b, _ := base64.StdEncoding.DecodeString(shot.Data)
		os.WriteFile(*out, b, 0o644)
		fmt.Println("screenshot:", *out, len(b), "bytes")
	}
	call("Target.closeTarget", nil)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
