package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML []byte

const maxLogLines = 3000

type Dashboard struct {
	mu         sync.RWMutex
	process    *exec.Cmd
	running    bool
	startTime  time.Time
	logs       []string
	binaryPath string
	configPath string
	cancel     context.CancelFunc
	healthPort int
}

type StatusResponse struct {
	GatewayRunning bool        `json:"gateway_running"`
	GatewayPID     int         `json:"gateway_pid"`
	Uptime         string      `json:"uptime"`
	ConfigExists   bool        `json:"config_exists"`
	BinaryExists   bool        `json:"binary_exists"`
	BinaryPath     string      `json:"binary_path"`
	ConfigPath     string      `json:"config_path"`
	HealthPort     int         `json:"health_port"`
	Health         *HealthInfo `json:"health"`
}

type HealthInfo struct {
	Status string `json:"status"`
	Uptime string `json:"uptime"`
}

type APIResponse struct {
	OK      bool        `json:"ok"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

func main() {
	port := 18080

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port", "-p":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &port)
				i++
			}
		}
	}

	binaryPath := findBinary()
	configPath := getConfigPath()

	d := &Dashboard{
		binaryPath: binaryPath,
		configPath: configPath,
		logs:       make([]string, 0, maxLogLines),
		healthPort: 18790,
	}

	if cfg, err := d.readConfigMap(); err == nil {
		if gw, ok := cfg["gateway"].(map[string]interface{}); ok {
			if p, ok := gw["port"].(float64); ok {
				d.healthPort = int(p)
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", d.handleIndex)
	mux.HandleFunc("/api/status", d.handleStatus)
	mux.HandleFunc("/api/config", d.handleConfig)
	mux.HandleFunc("/api/validate", d.handleValidate)
	mux.HandleFunc("/api/start", d.handleStart)
	mux.HandleFunc("/api/stop", d.handleStop)
	mux.HandleFunc("/api/restart", d.handleRestart)
	mux.HandleFunc("/api/logs", d.handleLogs)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	d.addLog("Dashboard started on port %d", port)
	d.addLog("Binary: %s (exists: %v)", binaryPath, fileExists(binaryPath))
	d.addLog("Config: %s (exists: %v)", configPath, fileExists(configPath))

	go func() {
		time.Sleep(600 * time.Millisecond)
		openBrowser(fmt.Sprintf("http://localhost:%d", port))
	}()

	fmt.Printf("PicoClaw Dashboard: http://localhost:%d\n", port)
	fmt.Println("Press Ctrl+C to stop")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		d.stopGateway()
		server.Shutdown(context.Background())
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

// ── HTTP Handlers ───────────────────────────────────────

func (d *Dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func (d *Dashboard) handleStatus(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	running := d.running
	var pid int
	var uptime string
	if running && d.process != nil && d.process.Process != nil {
		pid = d.process.Process.Pid
		uptime = time.Since(d.startTime).Truncate(time.Second).String()
	}
	d.mu.RUnlock()

	status := StatusResponse{
		GatewayRunning: running,
		GatewayPID:     pid,
		Uptime:         uptime,
		ConfigExists:   fileExists(d.configPath),
		BinaryExists:   fileExists(d.binaryPath),
		BinaryPath:     d.binaryPath,
		ConfigPath:     d.configPath,
		HealthPort:     d.healthPort,
	}

	if running {
		status.Health = d.checkHealth()
	}

	jsonResp(w, APIResponse{OK: true, Data: status})
}

func (d *Dashboard) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(d.configPath)
		if err != nil {
			if os.IsNotExist(err) {
				jsonResp(w, APIResponse{OK: false, Message: "Config not found. Use 'Create Default' to initialize."})
			} else {
				jsonResp(w, APIResponse{OK: false, Message: err.Error()})
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)

	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			jsonResp(w, APIResponse{OK: false, Message: "Failed to read body"})
			return
		}

		var parsed interface{}
		if err := json.Unmarshal(body, &parsed); err != nil {
			jsonResp(w, APIResponse{OK: false, Message: fmt.Sprintf("Invalid JSON: %v", err)})
			return
		}

		pretty, _ := json.MarshalIndent(parsed, "", "  ")

		if fileExists(d.configPath) {
			os.Rename(d.configPath, d.configPath+".bak")
		}
		os.MkdirAll(filepath.Dir(d.configPath), 0755)

		if err := os.WriteFile(d.configPath, append(pretty, '\n'), 0644); err != nil {
			jsonResp(w, APIResponse{OK: false, Message: fmt.Sprintf("Failed to save: %v", err)})
			return
		}

		d.addLog("Config saved to %s", d.configPath)
		jsonResp(w, APIResponse{OK: true, Message: "Configuration saved"})

	case http.MethodPost:
		if !fileExists(d.binaryPath) {
			jsonResp(w, APIResponse{OK: false, Message: "PicoClaw binary not found"})
			return
		}
		cmd := exec.Command(d.binaryPath, "onboard")
		cmd.Stdin = strings.NewReader("y\n")
		output, err := cmd.CombinedOutput()
		if err != nil {
			jsonResp(w, APIResponse{OK: false, Message: fmt.Sprintf("Onboard failed: %v\n%s", err, string(output))})
			return
		}
		d.addLog("Onboard completed: %s", strings.TrimSpace(string(output)))
		jsonResp(w, APIResponse{OK: true, Message: "Default configuration created"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (d *Dashboard) handleValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonResp(w, APIResponse{OK: false, Message: "Failed to read body"})
		return
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		jsonResp(w, APIResponse{OK: false, Message: fmt.Sprintf("Invalid JSON: %v", err)})
		return
	}

	warnings := validateConfig(parsed)

	if len(warnings) > 0 {
		jsonResp(w, APIResponse{OK: true, Message: "Valid JSON with warnings", Data: warnings})
	} else {
		jsonResp(w, APIResponse{OK: true, Message: "Configuration looks good!"})
	}
}

func (d *Dashboard) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := d.startGateway(); err != nil {
		jsonResp(w, APIResponse{OK: false, Message: err.Error()})
		return
	}
	jsonResp(w, APIResponse{OK: true, Message: "Gateway started"})
}

func (d *Dashboard) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := d.stopGateway(); err != nil {
		jsonResp(w, APIResponse{OK: false, Message: err.Error()})
		return
	}
	jsonResp(w, APIResponse{OK: true, Message: "Gateway stop signal sent"})
}

func (d *Dashboard) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d.stopGateway()
	time.Sleep(1500 * time.Millisecond)
	if err := d.startGateway(); err != nil {
		jsonResp(w, APIResponse{OK: false, Message: err.Error()})
		return
	}
	jsonResp(w, APIResponse{OK: true, Message: "Gateway restarted"})
}

func (d *Dashboard) handleLogs(w http.ResponseWriter, r *http.Request) {
	d.mu.RLock()
	logs := make([]string, len(d.logs))
	copy(logs, d.logs)
	d.mu.RUnlock()
	jsonResp(w, APIResponse{OK: true, Data: logs})
}

// ── Process Management ──────────────────────────────────

func (d *Dashboard) startGateway() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.running {
		return fmt.Errorf("gateway is already running")
	}
	if !fileExists(d.binaryPath) {
		return fmt.Errorf("binary not found: %s", d.binaryPath)
	}
	if !fileExists(d.configPath) {
		return fmt.Errorf("config not found — create default config first")
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel

	cmd := exec.CommandContext(ctx, d.binaryPath, "gateway")

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("failed to start: %v", err)
	}

	d.process = cmd
	d.running = true
	d.startTime = time.Now()
	d.addLogLocked("Gateway started (PID: %d)", cmd.Process.Pid)

	go d.captureOutput(stdout)
	go d.captureOutput(stderr)

	go func() {
		err := cmd.Wait()
		d.mu.Lock()
		d.running = false
		d.process = nil
		d.cancel = nil
		d.mu.Unlock()
		if err != nil {
			d.addLog("Gateway exited: %v", err)
		} else {
			d.addLog("Gateway stopped")
		}
	}()

	return nil
}

func (d *Dashboard) stopGateway() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.running || d.process == nil {
		return fmt.Errorf("gateway is not running")
	}

	d.addLogLocked("Stopping gateway...")

	if d.cancel != nil {
		d.cancel()
	}

	if runtime.GOOS == "windows" {
		d.process.Process.Kill()
	} else {
		d.process.Process.Signal(os.Interrupt)
		go func(p *os.Process) {
			time.Sleep(5 * time.Second)
			p.Kill()
		}(d.process.Process)
	}

	return nil
}

func (d *Dashboard) captureOutput(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		d.addLog("[gateway] %s", scanner.Text())
	}
}

// ── Logging ─────────────────────────────────────────────

func (d *Dashboard) addLog(format string, args ...interface{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addLogLocked(format, args...)
}

func (d *Dashboard) addLogLocked(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	line := fmt.Sprintf("[%s] %s", ts, fmt.Sprintf(format, args...))
	d.logs = append(d.logs, line)
	if len(d.logs) > maxLogLines {
		d.logs = d.logs[len(d.logs)-maxLogLines:]
	}
}

// ── Config ──────────────────────────────────────────────

func (d *Dashboard) readConfigMap() (map[string]interface{}, error) {
	data, err := os.ReadFile(d.configPath)
	if err != nil {
		return nil, err
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (d *Dashboard) checkHealth() *HealthInfo {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/health", d.healthPort))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var h HealthInfo
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil
	}
	return &h
}

func validateConfig(cfg map[string]interface{}) []string {
	var w []string

	agents, _ := cfg["agents"].(map[string]interface{})
	if agents == nil {
		w = append(w, "Missing 'agents' section")
	} else if defaults, _ := agents["defaults"].(map[string]interface{}); defaults == nil {
		w = append(w, "Missing 'agents.defaults' section")
	} else {
		if m, _ := defaults["model"].(string); m == "" {
			w = append(w, "'agents.defaults.model' is empty")
		}
	}

	placeholders := map[string]bool{
		"": true, "YOUR_TELEGRAM_BOT_TOKEN": true, "YOUR_DISCORD_BOT_TOKEN": true,
		"YOUR_ZHIPU_API_KEY": true, "sk-or-v1-xxx": true, "sk-xxx": true,
		"nvapi-xxx": true, "gsk_xxx": true, "pplx-xxx": true,
		"YOUR_CLIENT_ID": true, "YOUR_CLIENT_SECRET": true,
		"YOUR_LINE_CHANNEL_SECRET": true, "YOUR_LINE_CHANNEL_ACCESS_TOKEN": true,
		"YOUR_BRAVE_API_KEY": true, "xoxb-YOUR-BOT-TOKEN": true, "xapp-YOUR-APP-TOKEN": true,
	}

	providers, _ := cfg["providers"].(map[string]interface{})
	if providers == nil {
		w = append(w, "Missing 'providers' section")
	} else {
		hasKey := false
		for _, v := range providers {
			if p, ok := v.(map[string]interface{}); ok {
				if key, _ := p["api_key"].(string); !placeholders[key] {
					hasKey = true
					break
				}
				if base, _ := p["api_base"].(string); base != "" {
					hasKey = true
					break
				}
			}
		}
		if !hasKey {
			w = append(w, "No API keys configured — set at least one provider key")
		}
	}

	return w
}

// ── Helpers ─────────────────────────────────────────────

func jsonResp(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func getConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".picoclaw", "config.json")
}

func findBinary() string {
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}

	candidates := []string{
		filepath.Join("build", "picoclaw"+ext),
		"picoclaw" + ext,
	}
	for _, c := range candidates {
		if abs, err := filepath.Abs(c); err == nil && fileExists(abs) {
			return abs
		}
	}

	if p, err := exec.LookPath("picoclaw"); err == nil {
		return p
	}

	abs, _ := filepath.Abs(filepath.Join("build", "picoclaw"+ext))
	return abs
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}
