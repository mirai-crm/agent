package updater

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	healthStatePending = "pending"
	healthStateHealthy = "healthy"
)

type healthMarker struct {
	Nonce   string `json:"nonce"`
	Version string `json:"version"`
	State   string `json:"state"`
}

func markerPath(configPath string) string {
	return configPath + ".update-health.json"
}

func writePendingHealthMarker(req ApplyRequest) error {
	return writeHealthMarker(markerPath(req.ConfigPath), healthMarker{
		Nonce:   req.Nonce,
		Version: req.Version,
		State:   healthStatePending,
	})
}

func MarkHealthy(configPath, currentVersion string) error {
	path := markerPath(configPath)
	marker, ok, err := tryLoadHealthMarker(path)
	if err != nil {
		return fmt.Errorf("inspect health marker: %w", err)
	}
	if !ok {
		return nil
	}
	if marker.Version != currentVersion {
		return nil
	}
	marker.State = healthStateHealthy
	return writeHealthMarker(path, marker)
}

func writeHealthMarker(path string, marker healthMarker) error {
	if strings.TrimSpace(marker.Nonce) == "" {
		return fmt.Errorf("health marker nonce is required")
	}
	if strings.TrimSpace(marker.Version) == "" {
		return fmt.Errorf("health marker version is required")
	}
	if marker.State != healthStatePending && marker.State != healthStateHealthy {
		return fmt.Errorf("health marker state %q is invalid", marker.State)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create health marker dir: %w", err)
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode health marker: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create health marker temp file: %w", err)
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		_ = tmp.Close()
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("chmod health marker temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write health marker temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync health marker temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close health marker temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename health marker: %w", err)
	}
	removeTmp = false
	return nil
}

func loadHealthMarker(path string) (healthMarker, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return healthMarker{}, err
	}
	var marker healthMarker
	if err := json.Unmarshal(body, &marker); err != nil {
		return healthMarker{}, fmt.Errorf("decode health marker: %w", err)
	}
	return marker, nil
}

func tryLoadHealthMarker(path string) (healthMarker, bool, error) {
	marker, err := loadHealthMarker(path)
	if err != nil {
		if os.IsNotExist(err) {
			return healthMarker{}, false, nil
		}
		return healthMarker{}, false, err
	}
	return marker, true, nil
}
