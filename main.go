package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"

	"deskctl/internal/chrome"
	"deskctl/internal/config"
	"deskctl/internal/server"
)

//go:embed all:web
var embeddedWeb embed.FS

var version = "0.1.0"

func main() {
	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	addr := flag.String("addr", "", "覆盖监听地址，如 0.0.0.0:7788")
	openCtrl := flag.Bool("open", false, "启动后自动打开控制器页面（默认不打开）")
	verbose := flag.Bool("v", false, "输出详细启动信息")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		if os.IsNotExist(err) && *cfgPath == "config.yaml" {
			// 没有配置文件就按默认跑，少一步打扰
			cfg, err = config.Load("")
		}
		if err != nil {
			log.Fatalf("[deskctl] 配置错误: %v", err)
		}
	}
	if *addr != "" {
		host, port, splitErr := splitAddr(*addr)
		if splitErr != nil {
			log.Fatalf("[deskctl] -addr 非法: %v", splitErr)
		}
		cfg.Server.Host = host
		if port > 0 {
			cfg.Server.Port = port
		}
	}

	webFS, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		log.Fatalf("[deskctl] 前端资源加载失败: %v", err)
	}

	mgr := chrome.NewManager(cfg.Chrome)
	srv, err := server.New(cfg, mgr, webFS)
	if err != nil {
		log.Fatalf("[deskctl] 初始化失败: %v", err)
	}

	// 启动全程静默：不打日志、不弹浏览器、不预热 Chrome。
	// Chrome 由第一次操作（跑场景 / 全屏 / 扫描）按需拉起，没人在用时进程里没有 Chrome。
	httpSrv := &http.Server{
		Addr:              srv.Addr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	if *verbose {
		log.Printf("[deskctl] %s · %s · Chrome: %s", version, runtime.GOOS, mgr.Describe())
	}
	line := "[deskctl] 本机 " + srv.URL()
	log.Printf("%s", line)
	if cfg.Server.Host == "0.0.0.0" || cfg.Server.Host == "::" {
		lan := lanAddr(cfg.Server.Port)
		log.Printf("[deskctl] 手机/平板 http://%s（扫码直接进入，无需密码）", lan)
		printQR(os.Stderr, srv.URLWith(lan))
	}

	if *openCtrl || cfg.Server.AutoOpen {
		go func() {
			time.Sleep(300 * time.Millisecond)
			openBrowser(srv.URL())
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 盯页面 URL，新页面自动重扫可点元素
	go srv.WatchPages(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("[deskctl] 服务启动失败: %v", err)
		}
	case <-ctx.Done():
		log.Printf("[deskctl] 正在退出…")
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		mgr.Close()
	}
}

func splitAddr(s string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

// lanAddr 找出一个非回环 IPv4，方便手机直接访问。
// 优先 10/8、172.16/12、192.168/16 这类内网地址，避免选中 utun / docker 的公网段。
// 网卡枚举顺序不可靠：Windows 上 Hyper-V 的 vEthernet (Default Switch) 会排在真实网卡
// 前面，本身就是 172.28.0.1 这类私有地址，按顺序取第一个私有地址必然选中它，手机连不上。
// 所以先问内核默认路由的出口地址（UDP connect 不发包），拿不到再退回遍历。
func lanAddr(port int) string {
	if ip := egressV4(); ip != nil {
		return net.JoinHostPort(ip.String(), strconv.Itoa(port))
	}
	var fallback string
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || ipn.IP.To4() == nil {
				continue
			}
			hp := net.JoinHostPort(ipn.IP.String(), strconv.Itoa(port))
			if isPrivateV4(ipn.IP) {
				return hp
			}
			if fallback == "" {
				fallback = hp
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// egressV4 问内核：访问外网时源地址是谁。UDP connect 只选路由、不发包。
// 只接受私有 IPv4 —— 默认路由走到 VPN 时这里会拿到 tun 地址，那种情况交回给
// 原来的网卡遍历逻辑处理。
func egressV4() net.IP {
	for _, target := range []string{"8.8.8.8:80", "223.5.5.5:80"} {
		c, err := net.Dial("udp4", target)
		if err != nil {
			continue
		}
		ua, ok := c.LocalAddr().(*net.UDPAddr)
		c.Close()
		if ok && ua.IP != nil && ua.IP.To4() != nil && !ua.IP.IsLoopback() && isPrivateV4(ua.IP) {
			return ua.IP
		}
	}
	return nil
}

func isPrivateV4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 10 ||
		(v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31) ||
		(v4[0] == 192 && v4[1] == 168)
}

// printQR 把地址印成终端二维码：黑前景 + 白背景，跟终端主题无关；
// 半块字符一行顶两行模块，模块保持正方，41 列以内宽度不折行。
func printQR(w io.Writer, url string) {
	q, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "\n\x1b[30;47m%s\x1b[0m\n", q.ToSmallString(false))
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
