package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const defaultURL = "http://127.0.0.1:9527"

// Config is persisted to ~/.config/omarchy-honk/config.json (mode 0600).
type Config struct {
	URL         string `json:"url"`
	Secret      string `json:"secret"`
	ShowTraffic bool   `json:"showTraffic"`
	GroupsView  string `json:"groupsView"`
	IPProvider  string `json:"ipProvider"`
	STUNServer  string `json:"stunServer"`
	// RecordFlows keeps honk's flow recorder pinned "on" (see watch.go).
	RecordFlows bool `json:"recordFlows"`
}

func configPath() string {
	if p := os.Getenv("HONK_PLUGIN_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "omarchy-honk", "config.json")
}

func defaultConfig() Config {
	return Config{
		URL: defaultURL, ShowTraffic: true, GroupsView: "card",
		IPProvider: defaultIPProvider, STUNServer: defaultSTUNServer, RecordFlows: true,
	}
}

func loadConfig() Config {
	cfg := defaultConfig()
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	var saved map[string]any
	if json.Unmarshal(data, &saved) != nil {
		return cfg
	}
	if v, ok := saved["url"].(string); ok && v != "" {
		cfg.URL = v
	}
	if v, ok := saved["secret"].(string); ok {
		cfg.Secret = v
	}
	if v, ok := saved["showTraffic"].(bool); ok {
		cfg.ShowTraffic = v
	}
	if v, ok := saved["recordFlows"].(bool); ok {
		cfg.RecordFlows = v
	}
	for key := range prefKeys {
		if v, ok := saved[key].(string); ok {
			_ = setPref(&cfg, key, v)
		}
	}
	return cfg
}

// saveConfig writes atomically with private permissions.
func saveConfig(cfg Config) error {
	path := configPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "config.json.tmp.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// resolveEndpoint picks the API URL and secret: flag, then environment (as
// set by the QML side), then the saved config.
func resolveEndpoint(flagURL string) (string, string) {
	cfg := loadConfig()
	u := flagURL
	if u == "" {
		u = os.Getenv("HONK_API_URL")
	}
	if u == "" {
		u = cfg.URL
	}
	secret, ok := os.LookupEnv("HONK_API_SECRET")
	if !ok || secret == "" {
		secret = cfg.Secret
	}
	return u, secret
}

// prefKeys lists the UI preferences set-pref accepts, with their validators.
var prefKeys = map[string]func(string) error{
	"groupsView": func(v string) error {
		if v != "card" && v != "list" {
			return errors.New("groupsView must be card or list")
		}
		return nil
	},
	"ipProvider": func(v string) error {
		if _, ok := ipProviders[v]; !ok {
			return fmt.Errorf("ipProvider must be one of ip.sb, ipwho.is, ipapi.is")
		}
		return nil
	},
	"stunServer": func(v string) error {
		if _, _, err := net.SplitHostPort(v); err != nil {
			return fmt.Errorf("stunServer must be host:port: %v", err)
		}
		return nil
	},
}

func setPref(cfg *Config, key, value string) error {
	validate, ok := prefKeys[key]
	if !ok {
		return fmt.Errorf("unknown preference %q", key)
	}
	value = strings.TrimSpace(value)
	if err := validate(value); err != nil {
		return err
	}
	switch key {
	case "groupsView":
		cfg.GroupsView = value
	case "ipProvider":
		cfg.IPProvider = value
	case "stunServer":
		cfg.STUNServer = value
	}
	return nil
}
