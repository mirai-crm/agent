package updater

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

const defaultMaxDownloadBytes = 1 << 30

type StageResult struct {
	Dir        string
	BinaryPath string
}

func stageRelease(ctx context.Context, client *http.Client, release Release, targetPath string) (*StageResult, error) {
	if client == nil {
		return nil, fmt.Errorf("http client is required")
	}
	if !filepath.IsAbs(targetPath) {
		return nil, fmt.Errorf("target path must be absolute")
	}
	stageDir, err := os.MkdirTemp(filepath.Dir(targetPath), ".mirai-agent-stage-*")
	if err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(stageDir)
		}
	}()

	binaryName := "mirai-agent-update"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(stageDir, binaryName)
	if err := downloadFile(ctx, client, release.BinaryURL, binaryPath, true); err != nil {
		return nil, fmt.Errorf("download binary: %w", err)
	}
	if release.LibUSBURL != "" {
		dllPath := filepath.Join(stageDir, "libusb-1.0.dll")
		if err := downloadFile(ctx, client, release.LibUSBURL, dllPath, false); err != nil {
			return nil, fmt.Errorf("download libusb: %w", err)
		}
	}

	ok = true
	return &StageResult{Dir: stageDir, BinaryPath: binaryPath}, nil
}

func downloadFile(ctx context.Context, client *http.Client, rawURL, dstPath string, executable bool) error {
	if _, err := parseURL(rawURL); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("request failed: status %d", resp.StatusCode)
	}

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create staged file: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = dst.Close()
		}
	}()

	n, copyErr := io.Copy(dst, io.LimitReader(resp.Body, defaultMaxDownloadBytes+1))
	if copyErr != nil {
		return fmt.Errorf("write staged file: %w", copyErr)
	}
	if n > defaultMaxDownloadBytes {
		return fmt.Errorf("download exceeds %d bytes", defaultMaxDownloadBytes)
	}
	if executable && runtime.GOOS != "windows" {
		if err := dst.Chmod(0o755); err != nil {
			return fmt.Errorf("chmod staged binary: %w", err)
		}
	}
	if err := dst.Sync(); err != nil {
		return fmt.Errorf("sync staged file: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close staged file: %w", err)
	}
	closed = true
	return nil
}
