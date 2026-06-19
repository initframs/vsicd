package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"

	vsic "github.com/initframs/vsic"
)

type Config struct {
	Name                 string `toml:"name"`
	Motd                 string `toml:"motd"`
	AllowPriviledgedPort bool   `toml:"allow_priviledged_port"`

	Moderation struct {
		Banlist string `toml:"banlist"`
		Modcmd  string `toml:"modcmd"`
	} `toml:"moderation"`

	MaxConnsPerIP int `toml:"max_conns_per_ip"`
	MaxMsgsPerSec int `toml:"max_msgs_per_sec"`
	MaxMsgSize    int `toml:"max_msg_size"`
	MaxKeepalive  int `toml:"max_keepalive_timeout"`

	Server struct {
		TCP struct {
			Enabled bool `toml:"enabled"`
			Port    int  `toml:"port"`
		} `toml:"tcp"`
		TLS struct {
			Enabled bool   `toml:"enabled"`
			Port    int    `toml:"port"`
			Cert    string `toml:"ssl_cert"`
			Key     string `toml:"ssl_key"`
		} `toml:"tls"`
	} `toml:"server"`
}

type Client struct {
	Conn     *vsic.Conn
	Send     chan string
	IP       string
	LastMsg  time.Time
	MsgCount int
}

type Server struct {
	cfg       Config
	clients   map[string]*Client
	ipCounts  map[string]int
	mu        sync.RWMutex
	startTime time.Time
	totalMsg  int64
}

var (
	baseDir  = filepath.Join(os.Getenv("HOME"), ".config/vsicd")
	pidFile  = filepath.Join(baseDir, "vsicd.pid")
	statFile = filepath.Join(baseDir, "vsicd.stats")
)

func nicepanic(s string) {
	fmt.Println(s)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: vsicd [start|stop|info]")
		return
	}

	switch os.Args[1] {
	case "start":
		start()
	case "stop":
		stop()
	case "info":
		info()
	}
}

func start() {
	if data, err := os.ReadFile(pidFile); err == nil {
		pidStr := strings.TrimSpace(string(data))
		pid, _ := strconv.Atoi(pidStr)
		if err := syscall.Kill(pid, 0); err == nil {
			fmt.Println("vsicd is already running, PID:", pid)
			return
		}
		os.Remove(pidFile)
	}

	if os.Getenv("_VSICD_DAEMON") != "1" {
		cmd := exec.Command(os.Args[0], "start")
		cmd.Env = append(os.Environ(), "_VSICD_DAEMON=1")

		logDir := filepath.Join(baseDir, "logs")
		os.MkdirAll(logDir, 0755)
		logFile, err := os.OpenFile(filepath.Join(logDir, "vsicd.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Println("failed to open log file:", err)
			return
		}

		cmd.Stdout = logFile
		cmd.Stderr = logFile

		if err := cmd.Start(); err != nil {
			fmt.Println("failed to start daemon:", err)
			return
		}

		fmt.Println("vsicd started in background, pid:", cmd.Process.Pid)
		return
	}

	cfg := loadConfig()
	writePID()

	s := &Server{
		cfg:       cfg,
		clients:   make(map[string]*Client),
		ipCounts:  make(map[string]int),
		startTime: time.Now(),
	}

	go s.writeStats()

	var ln net.Listener
	var err error

	if cfg.Server.TLS.Enabled {
		cert, err := tls.LoadX509KeyPair(expand(cfg.Server.TLS.Cert), expand(cfg.Server.TLS.Key))
		if err != nil {
			panic(err)
		}
		ln, err = tls.Listen("tcp", fmt.Sprintf(":%d", cfg.Server.TLS.Port),
			&tls.Config{Certificates: []tls.Certificate{cert}})
	} else {
		ln, err = net.Listen("tcp", fmt.Sprintf(":%d", cfg.Server.TCP.Port))
	}
	if err != nil {
		panic(err)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sig
		fmt.Println("shutting down...")
		ln.Close() // stop accepting new conns

		s.mu.Lock()
		for _, c := range s.clients {
			c.Conn.Close()
		}
		s.mu.Unlock()

		os.Remove(pidFile)
		fmt.Println("vsicd stopped")
		os.Exit(0)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(nc net.Conn) {
	ip := strings.Split(nc.RemoteAddr().String(), ":")[0]

	s.mu.Lock()
	if s.ipCounts[ip] >= s.cfg.MaxConnsPerIP {
		s.mu.Unlock()
		nc.Close()
		return
	}
	s.ipCounts[ip]++
	s.mu.Unlock()

	vconn := vsic.Wrap(nc, vsic.Config{
		MaxMsgSize: s.cfg.MaxMsgSize,
		TimeoutSec: s.cfg.MaxKeepalive,
	})

	client := &Client{
		Conn: vconn,
		Send: make(chan string, 16),
		IP:   ip,
	}

	go s.writeLoop(client)

	line, err := vconn.ReadLine()
	if err != nil {
		s.disconnect(client)
		return
	}

	cmd, arg := vsic.ParseCommand(line)
	if cmd != "HELLO" || !vsic.ValidNick(arg) {
		vconn.WriteLine("ERROR 100")
		s.disconnect(client)
		return
	}

	nick := s.uniqueNick(arg)
	vconn.Nick = nick
	client.Conn.WriteLine("HELLO " + nick)

	if s.cfg.Motd != "" {
		for _, line := range strings.Split(s.cfg.Motd, "\n") {
			if line != "" {
				client.Conn.WriteLine("MOTD " + line)
			}
		}
	}

	s.mu.Lock()
	s.clients[nick] = client
	s.mu.Unlock()

	for {
		line, err := vconn.ReadLine()
		if err != nil {
			break
		}

		cmd, arg := vsic.ParseCommand(line)

		switch cmd {

		case "MSG":
			if time.Since(client.LastMsg) < time.Second/time.Duration(max(1, s.cfg.MaxMsgsPerSec)) {
				continue
			}
			client.LastMsg = time.Now()
			s.broadcast("MSG " + nick + ": " + arg)

		case "PING":
			client.Conn.WriteLine("PONG")

		case "BYE":
			client.Conn.WriteLine("CYA")
			s.disconnect(client)
			return
		}
	}

	s.disconnect(client)
}

func (s *Server) writeLoop(c *Client) {
	for msg := range c.Send {
		c.Conn.WriteLine(msg)
	}
}

func (s *Server) broadcast(msg string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, c := range s.clients {
		select {
		case c.Send <- msg:
		default:
		}
	}
	s.totalMsg++
}

func (s *Server) disconnect(c *Client) {
	c.Conn.Close()
	close(c.Send)

	s.mu.Lock()
	delete(s.clients, c.Conn.Nick)
	s.ipCounts[c.IP]--
	s.mu.Unlock()
}

func (s *Server) uniqueNick(n string) string {
	s.mu.RLock()
	_, exists := s.clients[n]
	s.mu.RUnlock()

	if !exists {
		return n
	}
	return n + vsic.RandomSuffix()
}

func (s *Server) writeStats() {
	for {
		time.Sleep(5 * time.Second)
		m := runtime.MemStats{}
		runtime.ReadMemStats(&m)

		stats := map[string]interface{}{
			"clients":    len(s.clients),
			"goroutines": runtime.NumGoroutine(),
			"mem_mb":     m.Alloc / 1024 / 1024,
			"uptime_sec": int(time.Since(s.startTime).Seconds()),
			"messages":   s.totalMsg,
		}

		data, _ := json.MarshalIndent(stats, "", "  ")
		os.WriteFile(statFile, data, 0644)
	}
}

func loadConfig() Config {
	cfgPath := expand(filepath.Join(baseDir, "config.toml"))
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		nicepanic("config not found, try creating it at ~/.config/vsicd/config.toml")
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		panic(err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		nicepanic("invalid toml config")
	}

	// todo: more config validation

	if cfg.MaxMsgSize <= 0 {
		cfg.MaxMsgSize = 4096 // so that someone cant just nuke the entire server if MaxMsgSize isnt specified
	}

	if cfg.MaxConnsPerIP <= 0 {
		cfg.MaxConnsPerIP = 4
	}

	if cfg.MaxMsgsPerSec <= 0 {
		cfg.MaxMsgsPerSec = 1
	}

	if !cfg.AllowPriviledgedPort {
		cfg.AllowPriviledgedPort = false
	}

	if cfg.Server.TLS.Port == 0 && cfg.Server.TCP.Port == 0 {
		nicepanic("no tcp or tls port defined, nothing to do")
	}

	if cfg.Server.TLS.Enabled && (cfg.Server.TLS.Cert == "" || cfg.Server.TLS.Key == "") {
		nicepanic("tls enabled but cert or key not defined")
	}

	if ((cfg.Server.TLS.Port <= 1000 && cfg.Server.TLS.Enabled) || (cfg.Server.TCP.Port <= 1000 && cfg.Server.TCP.Enabled)) && (cfg.AllowPriviledgedPort == false) {
		fmt.Println(cfg.Server.TLS.Port)
		fmt.Println(cfg.Server.TCP.Port)
		nicepanic("it looks like you're trying to run vsicd on a priviledged port (<1000). this is disabled by default, but you can enable it by setting `allow_priviledged_port` at the root of your config")
	}

	return cfg
}

func writePID() {
	os.MkdirAll(baseDir, 0755)
	os.WriteFile(pidFile, []byte(fmt.Sprint(os.Getpid())), 0644)
}

func stop() {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		fmt.Println("vsicd not running")
		return
	}

	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		fmt.Println("invalid PID file, removing")
		os.Remove(pidFile)
		return
	}

	if err := syscall.Kill(pid, 0); err != nil {
		fmt.Println("process not running, removing stale PID file")
		os.Remove(pidFile)
		return
	}

	proc, _ := os.FindProcess(pid)
	fmt.Println("sending SIGTERM to process", pid)
	proc.Signal(syscall.SIGTERM)

	for i := 0; i < 10; i++ {
		time.Sleep(time.Second)
		if err := syscall.Kill(pid, 0); err != nil {
			os.Remove(pidFile)
			fmt.Println("vsicd stopped successfully")
			return
		}
	}

	fmt.Println("warning: vsicd may not have stopped!")
}

func info() {
	data, err := os.ReadFile(statFile)
	if err != nil {
		fmt.Println("no stats")
		return
	}
	os.Stdout.Write(data)
	fmt.Println()
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(os.Getenv("HOME"), p[2:])
	}
	return p
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
