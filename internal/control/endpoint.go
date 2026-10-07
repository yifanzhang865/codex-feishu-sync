package control

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// Endpoint is private runtime data, independent on each machine. Tokens are
// passed to Codex through an environment variable, never through argv.
type Endpoint struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func saveEndpoint(directory string, endpoint Endpoint) error {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(endpoint)
	if err != nil {
		return err
	}
	path := filepath.Join(directory, "control.json")
	temp, err := os.CreateTemp(directory, ".control-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

func LoadEndpoint(directory string) (Endpoint, error) {
	data, err := os.ReadFile(filepath.Join(directory, "control.json"))
	if err != nil {
		return Endpoint{}, errors.New("本机桥接服务尚未启动，请先运行 codex-feishu run")
	}
	var endpoint Endpoint
	if err := json.Unmarshal(data, &endpoint); err != nil {
		return Endpoint{}, err
	}
	u, err := url.Parse(endpoint.URL)
	if err != nil || u.Scheme != "ws" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || endpoint.Token == "" {
		return Endpoint{}, errors.New("本机控制入口配置无效，请重启 codex-feishu run")
	}
	return endpoint, nil
}

func removeEndpoint(directory string, endpoint Endpoint) error {
	if directory == "" {
		return nil
	}
	current, err := LoadEndpoint(directory)
	if err != nil || current.Token != endpoint.Token {
		return nil
	}
	return os.Remove(filepath.Join(directory, "control.json"))
}
