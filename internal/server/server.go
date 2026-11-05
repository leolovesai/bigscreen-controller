package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"deskctl/internal/chrome"
	"deskctl/internal/config"
)

type Server struct {
	cfg   *config.Config
	ch    *chrome.Manager
	token string
	web   fs.FS

	mu     sync.Mutex
	logins map[string]time.Time // 失败登录记录，防爆破

	subMu  sync.Mutex
	subs   map[int]func(any) error
	subSeq int

	scanMu    sync.Mutex
	lastURL   string
	emptyHits int
}

func New(cfg *config.Config, ch *chrome.Manager, web fs.FS) (*Server, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &Server{
		cfg:    cfg,
		ch:     ch,
		token:  hex.EncodeToString(b),
		web:    web,
		logins: make(map[string]time.Time),
		subs:   make(map[int]func(any) error),
	}, nil
}

// addSub 注册一个消息推送出口（每条 WS 连接一个），返回取消函数。
func (s *Server) addSub(fn func(any) error) int {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	s.subSeq++
	id := s.subSeq
	s.subs[id] = fn
	return id
}

func (s *Server) removeSub(id int) {
	s.subMu.Lock()
	delete(s.subs, id)
	s.subMu.Unlock()
}

func (s *Server) subCount() int {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return len(s.subs)
}

// broadcast 推给所有在线控制器，写失败的自动摘掉。
func (s *Server) broadcast(v any) {
	s.subMu.Lock()
	fns := make([]func(any) error, 0, len(s.subs))
	ids := make([]int, 0, len(s.subs))
	for id, fn := range s.subs {
		ids = append(ids, id)
		fns = append(fns, fn)
	}
	s.subMu.Unlock()

	for i, fn := range fns {
		if err := fn(v); err != nil {
			s.removeSub(ids[i])
		}
	}
}

// scanPage 扫描当前页面并记录状态（不广播）。
// 扫到空结果时 emptyHits 累积，让 WatchPages 知道该重试而不是认为"这页本来就没按钮"。
func (s *Server) scanPage(ctx context.Context) (*chrome.ButtonScan, error) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	scan, err := s.ch.ScanButtons(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if scan.URL != s.lastURL {
		s.emptyHits = 0
	}
	s.lastURL = scan.URL
	if len(scan.Items) == 0 {
		s.emptyHits++
	} else {
		s.emptyHits = 0
	}
	s.mu.Unlock()
	return scan, nil
}

// scanAndBroadcast 扫描并向所有控制器广播。
// 全局串行：多个控制器同时刷新时不重复扫同一页面。
func (s *Server) scanAndBroadcast(ctx context.Context) (*chrome.ButtonScan, error) {
	scan, err := s.scanPage(ctx)
	if err != nil {
		return nil, err
	}
	s.broadcast(map[string]any{"type": "page.buttons", "scan": scan})
	return scan, nil
}

// rescanUntilReady 点击后页面可能还在跳转，DOM 没就绪时扫不到东西。
// 反复扫直到拿到非空结果或超时，最后只广播一次，避免列表闪空。
func (s *Server) rescanUntilReady(ctx context.Context, dur time.Duration) {
	deadline := time.Now().Add(dur)
	var last *chrome.ButtonScan
	for {
		sctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		sc, err := s.scanPage(sctx)
		cancel()
		if err == nil && sc != nil {
			last = sc
			if len(sc.Items) > 0 {
				break
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(700 * time.Millisecond):
		}
		if ctx.Err() != nil {
			break
		}
	}
	if last != nil {
		s.broadcast(map[string]any{"type": "page.buttons", "scan": last})
	}
}

// WatchPages 盯着页面 URL，新页面自动重扫按钮。
// 没有控制器在线时完全不做事（不订阅不截图那套原则）。
func (s *Server) WatchPages(ctx context.Context) {
	tick := time.NewTicker(1200 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if s.subCount() == 0 || !s.ch.Status().Ready {
			continue
		}

		s.ch.RefreshMetrics(ctx)
		url := s.ch.Status().URL
		if url == "" {
			continue
		}
		s.mu.Lock()
		need := url != s.lastURL || (s.emptyHits > 0 && s.emptyHits < 6)
		s.mu.Unlock()
		if !need {
			continue
		}

		// 页面可能还在加载/跳转，等一拍再扫，否则扫到半成品 DOM
		select {
		case <-ctx.Done():
			return
		case <-time.After(700 * time.Millisecond):
		}

		// 等待期间可能已被别的扫描（点击重扫 / 场景完成）更新过，复核一次
		cur := s.ch.Status().URL
		s.mu.Lock()
		same := cur == s.lastURL && !(s.emptyHits > 0 && s.emptyHits < 6)
		s.mu.Unlock()
		if same {
			continue
		}

		sctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		_, err := s.scanAndBroadcast(sctx)
		cancel()
		if err != nil {
			// 扫失败就保持 lastURL 不变，下一轮重试
			continue
		}
	}
}

func (s *Server) Addr() string {
	return net.JoinHostPort(s.cfg.Server.Host, strconv.Itoa(s.cfg.Server.Port))
}

// URL 带 token，本机首次打开免密登录。
func (s *Server) URL() string {
	host := s.cfg.Server.Host
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(s.cfg.Server.Port)) + "/?k=" + s.token
}

// URLWith 拼指定 host:port 的带 token 地址，局域网二维码用。
func (s *Server) URLWith(hostport string) string {
	return "http://" + hostport + "/?k=" + s.token
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		st := s.ch.Status()
		writeJSON(w, 200, map[string]any{"ok": true, "chrome": st})
	})
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("GET /api/state", s.guard(s.handleState))
	mux.HandleFunc("POST /api/chrome/restart", s.guard(s.handleRestart))
	mux.HandleFunc("GET /ws", s.handleWS)

	fileServer := http.FileServerFS(s.web)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// 带 ?k= 的首次访问：种 cookie 后跳到干净 URL
		if k := r.URL.Query().Get("k"); k != "" {
			if subtle.ConstantTimeCompare([]byte(k), []byte(s.token)) == 1 {
				s.setCookie(w)
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
		}
		if !s.authorized(r) {
			// 未登录也返回页面，前端会显示密码框
			r = r.Clone(r.Context())
		}
		fileServer.ServeHTTP(w, r)
	})

	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) setCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "dk",
		Value:    s.token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7,
	})
}

func (s *Server) authorized(r *http.Request) bool {
	if k := r.URL.Query().Get("k"); k != "" {
		if subtle.ConstantTimeCompare([]byte(k), []byte(s.token)) == 1 {
			return true
		}
	}
	c, err := r.Cookie("dk")
	if err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) == 1 {
		return true
	}
	return false
}

func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeJSON(w, 401, map[string]any{"error": "未登录", "needPassword": true})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求格式错误"})
		return
	}

	ip := clientIP(r)
	s.mu.Lock()
	if t, ok := s.logins[ip]; ok && time.Since(t) < time.Second {
		s.mu.Unlock()
		writeJSON(w, 429, map[string]any{"error": "尝试过于频繁"})
		return
	}
	s.mu.Unlock()

	want := s.cfg.Server.Password
	if want == "" {
		// 本机模式：没有密码时不允许远程登录，但仍给本机回环放行
		if !isLoopbackHost(ip) {
			writeJSON(w, 403, map[string]any{"error": "未设置密码，仅允许本机访问"})
			return
		}
		s.setCookie(w)
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}

	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(want)) != 1 {
		s.mu.Lock()
		s.logins[ip] = time.Now()
		s.mu.Unlock()
		time.Sleep(600 * time.Millisecond)
		writeJSON(w, 401, map[string]any{"error": "密码错误"})
		return
	}

	s.mu.Lock()
	delete(s.logins, ip)
	s.mu.Unlock()
	s.setCookie(w)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"chrome":   s.ch.Status(),
		"scenes":   s.cfg.Scenes,
		"capture":  s.cfg.Capture,
		"platform": s.cfg.Server.Host,
	})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeoutSeconds(r, 20)
	defer cancel()
	if err := s.ch.Restart(ctx); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// bindAddr 返回可打印的访问地址（0.0.0.0 时补一个本机局域网 IP）。
func bindAddr(host string, port int) string {
	if host != "0.0.0.0" && host != "::" {
		return net.JoinHostPort(host, strconv.Itoa(port))
	}
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				return net.JoinHostPort(ipn.IP.String(), strconv.Itoa(port))
			}
		}
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}
