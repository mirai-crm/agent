package updater

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type ApplyRequest struct {
	TargetPath              string `json:"targetPath"`
	StagedBinaryPath        string `json:"stagedBinaryPath"`
	StagedLibUSBPath        string `json:"stagedLibUSBPath,omitempty"`
	ConfigPath              string `json:"configPath"`
	ParentPID               int    `json:"parentPid"`
	Version                 string `json:"version"`
	Nonce                   string `json:"nonce"`
	ParentExitTimeoutMillis int    `json:"parentExitTimeoutMillis"`
	HealthTimeoutMillis     int    `json:"healthTimeoutMillis"`
}

func (r ApplyRequest) Validate() error {
	switch {
	case !filepath.IsAbs(r.TargetPath):
		return fmt.Errorf("targetPath must be absolute")
	case !filepath.IsAbs(r.StagedBinaryPath):
		return fmt.Errorf("stagedBinaryPath must be absolute")
	case r.StagedLibUSBPath != "" && !filepath.IsAbs(r.StagedLibUSBPath):
		return fmt.Errorf("stagedLibUSBPath must be absolute")
	case !filepath.IsAbs(r.ConfigPath):
		return fmt.Errorf("configPath must be absolute")
	case r.ParentPID <= 0:
		return fmt.Errorf("parentPid must be positive")
	case strings.TrimSpace(r.Version) == "":
		return fmt.Errorf("version is required")
	case !validNonce(r.Nonce):
		return fmt.Errorf("nonce must be 16-128 lowercase hexadecimal characters")
	case r.ParentExitTimeoutMillis <= 0:
		return fmt.Errorf("parentExitTimeoutMillis must be positive")
	case r.HealthTimeoutMillis <= 0:
		return fmt.Errorf("healthTimeoutMillis must be positive")
	case r.TargetPath == r.StagedBinaryPath:
		return fmt.Errorf("targetPath and stagedBinaryPath must differ")
	default:
		return nil
	}
}

func validNonce(nonce string) bool {
	if len(nonce) < 16 || len(nonce) > 128 {
		return false
	}
	for _, char := range nonce {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func LoadApplyRequest(path string) (ApplyRequest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return ApplyRequest{}, fmt.Errorf("read apply request: %w", err)
	}
	var req ApplyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return ApplyRequest{}, fmt.Errorf("decode apply request: %w", err)
	}
	if err := req.Validate(); err != nil {
		return ApplyRequest{}, fmt.Errorf("validate apply request: %w", err)
	}
	return req, nil
}

func WriteApplyRequest(dir string, req ApplyRequest) (string, error) {
	if err := req.Validate(); err != nil {
		return "", fmt.Errorf("validate apply request: %w", err)
	}
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("request dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create request dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".mirai-agent-apply-request-*.json")
	if err != nil {
		return "", fmt.Errorf("create apply request: %w", err)
	}
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return "", fmt.Errorf("chmod apply request: %w", err)
	}
	if err := json.NewEncoder(tmp).Encode(req); err != nil {
		return "", fmt.Errorf("write apply request: %w", err)
	}
	return tmp.Name(), nil
}
