package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server  Server  `yaml:"server"`
	Capture Capture `yaml:"capture"`
	Chrome  Chrome  `yaml:"chrome"`
	Scenes  []Scene `yaml:"scenes"`
}

type Server struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Password string `yaml:"password"`
	AutoOpen bool   `yaml:"autoOpen"`
}

type Capture struct {
	Quality int `yaml:"quality" json:"quality"`
}

type Chrome struct {
	Binary      string   `yaml:"binary"`
	UserDataDir string   `yaml:"userDataDir"`
	WindowSize  []int    `yaml:"windowSize"`
	AppMode     bool     `yaml:"appMode"`
	ExtraArgs   []string `yaml:"extraArgs"`
}

type Scene struct {
	ID         string `yaml:"id" json:"id"`
	Name       string `yaml:"name" json:"name"`
	URL        string `yaml:"url" json:"url"`
	Fullscreen bool   `yaml:"fullscreen" json:"fullscreen"`
	Wait       string `yaml:"wait" json:"wait"`
	TimeoutMs  int    `yaml:"timeoutMs" json:"timeoutMs"`
}

func defaults() *Config {
	return &Config{
		Server:  Server{Host: "127.0.0.1", Port: 7788, AutoOpen: false},
		Capture: Capture{Quality: 60},
		Chrome: Chrome{
			UserDataDir: "~/.deskctl/chrome-profile",
			WindowSize:  []int{1512, 945},
		},
	}
}

// Load 读取配置并填默认值。path 为空时只返回默认配置。
func Load(path string) (*Config, error) {
	cfg := defaults()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("解析 %s: %w", path, err)
		}
	}
	if cfg.Chrome.Binary == "" {
		cfg.Chrome.Binary = DetectChrome()
	}
	cfg.Chrome.UserDataDir = expand(cfg.Chrome.UserDataDir)
	if len(cfg.Chrome.WindowSize) != 2 {
		cfg.Chrome.WindowSize = []int{1512, 945}
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port 非法: %d", c.Server.Port)
	}
	// 暴露到局域网必须设密码，否则等于把浏览器控制权交给整个内网。
	if !isLoopback(c.Server.Host) && c.Server.Password == "" {
		return fmt.Errorf("server.host=%s 不是回环地址，必须设置 server.password", c.Server.Host)
	}
	if c.Capture.Quality < 20 || c.Capture.Quality > 95 {
		c.Capture.Quality = 60
	}
	seen := map[string]bool{}
	for i := range c.Scenes {
		s := &c.Scenes[i]
		if s.ID == "" {
			s.ID = fmt.Sprintf("scene-%d", i+1)
		}
		if seen[s.ID] {
			return fmt.Errorf("scenes 中 id 重复: %s", s.ID)
		}
		seen[s.ID] = true
		if s.Name == "" {
			s.Name = s.ID
		}
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("场景 %s 的 url 必须是 http/https: %q", s.ID, s.URL)
		}
		if s.TimeoutMs <= 0 {
			s.TimeoutMs = 8000
		}
		if s.Wait == "" {
			s.Wait = "load"
		}
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func expand(p string) string {
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// DetectChrome 按平台常见安装路径找 Chrome。
func DetectChrome() string {
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	case "windows":
		pf := os.Getenv("ProgramFiles")
		pf86 := os.Getenv("ProgramFiles(x86)")
		local := os.Getenv("LOCALAPPDATA")
		candidates = []string{
			filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(local, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
		}
	default:
		for _, n := range []string{"google-chrome", "chromium", "chromium-browser"} {
			if p, err := filepath.Abs(n); err == nil {
				candidates = append(candidates, p)
			}
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}
