// Package mserv talks to an Ampio M-SERV: settings, and request/reply over its MQTT broker.
package mserv

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Settings say where the M-SERV's three services live and how to log in.
type Settings struct {
	MQTT     string // mqtt://host:1883, the config and module API
	Designer string // http://host, where the Designer web app (and its codec) is served
	NodeRED  string // http://host:1880

	User, Password               string // M-SERV admin login (same as Designer)
	NodeREDUser, NodeREDPassword string // default to the admin login
}

// LoadSettings reads the environment, after loading dir/.env for any variable
// not already set. Offline commands work without settings; Check says whether
// they are complete.
func LoadSettings(dir string) (Settings, error) {
	if err := loadEnvFile(filepath.Join(dir, ".env")); err != nil {
		return Settings{}, err
	}
	host := os.Getenv("AMPIO_HOST")
	or := func(name, fallback string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return fallback
	}
	s := Settings{
		MQTT:     or("AMPIO_MQTT_URL", "mqtt://"+host+":1883"),
		Designer: or("AMPIO_DESIGNER_URL", "http://"+host),
		NodeRED:  or("AMPIO_NODERED_URL", "http://"+host+":1880"),
		User:     or("AMPIO_USER", "admin"),
		Password: os.Getenv("AMPIO_PASSWORD"),
	}
	s.NodeREDUser = or("NODERED_USER", s.User)
	s.NodeREDPassword = or("NODERED_PASSWORD", s.Password)
	return s, nil
}

// Check reports what is missing to log in to the service at address, one
// of the addresses in s.
func (s Settings) Check(address string) error {
	if p, err := url.Parse(address); err != nil || p.Hostname() == "" {
		return fmt.Errorf("no M-SERV address in %q: set AMPIO_HOST in the environment or in .env", address)
	}
	if s.Password == "" {
		return errors.New("set AMPIO_PASSWORD in the environment or in .env")
	}
	return nil
}

// loadEnvFile sets KEY=VALUE lines from path, skipping comments and variables already set.
// A missing file is fine.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}
