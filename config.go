// boeyu-proxy (POC)：配置加载 / 解析 / 持久化。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Settings is the top level configuration file.
type Settings struct {
	DataDir            string            `json:"data_dir"`
	CertDays           int               `json:"cert_days"`
	CertDomains        []string          `json:"cert_domains"`
	AllowedHosts       []string          `json:"allowed_hosts"`
	AdminAddr          string            `json:"admin_addr"`
	AdminToken         string            `json:"admin_token"`
	Backends           map[string]string `json:"backends"`
	InsecureSkipVerify bool              `json:"insecure_skip_verify"`
	ReadTimeoutSec     int               `json:"read_timeout_sec"`
	IdleTimeoutSec     int               `json:"idle_timeout_sec"`
	Listeners          []ListenerConfig  `json:"listeners"`
}

// ListenerConfig describes one listener. When Proxy is true the listener is a
// classic HTTP proxy port (CONNECT tunnels + absolute-form requests). When
// false it is a direct TLS server. Rules are either inlined or loaded from
// RulesFile. EnableRules, when set, restricts which rules are switched on at
// startup (everything else stays present but disabled). ID is the stable
// programmatic identifier (English); Name is the display name (Chinese). When
// ID is empty it falls back to Name.
type ListenerConfig struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Addr        string   `json:"addr"`
	Offline     bool     `json:"offline"`
	Enabled     bool     `json:"enabled"`
	Proxy       bool     `json:"proxy"`
	RulesFile   string   `json:"rules_file"`
	EnableRules []string `json:"enable_rules"`
	Rules       []Rule   `json:"rules"`
}

// listenerID returns the programmatic identifier for a listener.
func listenerID(lc ListenerConfig) string {
	if lc.ID != "" {
		return lc.ID
	}
	return lc.Name
}

// LoadSettings reads and normalises the configuration file.
func LoadSettings(path string) (*Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	cfgDir := filepath.Dir(path)
	if s.DataDir == "" {
		s.DataDir = filepath.Join(cfgDir, "..", "data")
	}
	if s.CertDays <= 0 {
		s.CertDays = 36500
	}
	if s.AdminAddr == "" {
		s.AdminAddr = "127.0.0.1:8080"
	}
	if len(s.CertDomains) == 0 {
		s.CertDomains = []string{"*.boeyu.com", "s.boeyu.com", "logs.boeyu.com"}
	}
	if s.ReadTimeoutSec <= 0 {
		s.ReadTimeoutSec = 30
	}
	if s.IdleTimeoutSec <= 0 {
		s.IdleTimeoutSec = 120
	}
	if s.Backends == nil {
		s.Backends = map[string]string{}
	}

	for i := range s.Listeners {
		lc := &s.Listeners[i]
		if strings.TrimSpace(lc.Name) == "" {
			lc.Name = lc.Addr
		}
		if strings.TrimSpace(lc.ID) == "" {
			lc.ID = lc.Name
		}
		if lc.RulesFile != "" {
			rp := lc.RulesFile
			if !filepath.IsAbs(rp) {
				rp = filepath.Join(cfgDir, rp)
			}
			rules, err := LoadRulesFile(rp)
			if err != nil {
				return nil, fmt.Errorf("listener %s: %w", lc.Name, err)
			}
			lc.Rules = rules
		}
	}

	// Only whitelisted hosts may connect. cdn.boeyu.com and any other host
	// are explicitly forbidden; logs.boeyu.com is allowed but only answered
	// by the "logs全部200" rule (local 200 synthesis, never forwarded).
	if len(s.AllowedHosts) == 0 {
		s.AllowedHosts = []string{"s.boeyu.com"}
	}

	return &s, nil
}

// persist writes the current listener port/feature settings back to the config
// file, so changes made via the web UI survive a restart. Rule bodies stay in
// RulesFile and are not duplicated here.
func (s *Server) persist() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rt := range s.runtimes {
		st := rt.snapshotCfg()
		for i := range s.settings.Listeners {
			if listenerID(s.settings.Listeners[i]) == listenerID(st) {
				s.settings.Listeners[i] = st
			}
		}
	}
	data, err := json.MarshalIndent(s.settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.configPath, append(data, '\n'), 0o644)
}
