package chrome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// 极简 CDP 客户端：只需要 call + 事件丢弃，不引 chromedp。
type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cdpMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpError       `json:"error,omitempty"`
}

type cdpConn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	waiters map[int64]chan *cdpMessage
	closed  chan struct{}
	once    sync.Once
	onEvent func(session, method string, params json.RawMessage)
}

func dialCDP(rawURL string) (*cdpConn, error) {
	ws, _, err := websocket.DefaultDialer.Dial(rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("连接 CDP 失败: %w", err)
	}
	// 截图 base64 会比较大，放宽读上限。
	ws.SetReadLimit(64 << 20)
	c := &cdpConn{
		ws:      ws,
		waiters: make(map[int64]chan *cdpMessage),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *cdpConn) readLoop() {
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			c.Close()
			return
		}
		var m cdpMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.ID == 0 {
			// 只关心 Target.*（新标签页）和 Page.*（screencast 帧）
			c.mu.Lock()
			cb := c.onEvent
			c.mu.Unlock()
			if cb != nil && (strings.HasPrefix(m.Method, "Target.") || strings.HasPrefix(m.Method, "Page.")) {
				cb(m.SessionID, m.Method, m.Params)
			}
			continue
		}
		c.mu.Lock()
		ch := c.waiters[m.ID]
		delete(c.waiters, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- &m
		}
	}
}

func (c *cdpConn) call(ctx context.Context, session, method string, params any, out any) error {
	select {
	case <-c.closed:
		return errors.New("cdp: 连接已断开")
	default:
	}

	id := atomic.AddInt64(&c.nextID, 1)
	ch := make(chan *cdpMessage, 1)

	c.mu.Lock()
	c.waiters[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
	}()

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	c.writeMu.Lock()
	err = c.ws.WriteMessage(websocket.TextMessage, b)
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("cdp 写入失败: %w", err)
	}

	select {
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return errors.New("cdp: 连接已断开")
	}
}

// notify 只发不等回包。事件回调跑在读循环里，在里面等回包会把自己卡死。
func (c *cdpConn) notify(session, method string, params any) {
	id := atomic.AddInt64(&c.nextID, 1)
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	c.writeMu.Lock()
	_ = c.ws.WriteMessage(websocket.TextMessage, b)
	c.writeMu.Unlock()
	// 这个 id 没人认领，回包到了 waiters 里查不到会自动丢掉
}

func (c *cdpConn) Close() {
	c.once.Do(func() {
		close(c.closed)
		_ = c.ws.Close()
	})
}

// SetOnEvent 注册 browser 级事件回调。
func (c *cdpConn) SetOnEvent(fn func(session, method string, params json.RawMessage)) {
	c.mu.Lock()
	c.onEvent = fn
	c.mu.Unlock()
}
