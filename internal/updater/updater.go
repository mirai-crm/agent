package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	latestManifestURL      = "https://github.com/mirai-crm/agent/releases/latest/download/latest.json"
	defaultMaxManifestSize = 1 << 20
)

type Release struct {
	Version   string
	BinaryURL string
	LibUSBURL string
}

type updateManifest struct {
	Version   string                            `json:"version"`
	Platforms map[string]updateManifestPlatform `json:"platforms"`
}

type updateManifestPlatform struct {
	BinaryURL string `json:"binary_url"`
	LibUSBURL string `json:"libusb_url,omitempty"`
}

type semver struct {
	major int
	minor int
	patch int
	text  string
}

func checkRelease(ctx context.Context, client *http.Client, currentVersion, goos, goarch string) (*Release, error) {
	if currentVersion == "dev" {
		return nil, nil
	}
	current, err := parseStableSemver(currentVersion)
	if err != nil {
		return nil, fmt.Errorf("parse current version: %w", err)
	}
	manifest, err := fetchManifest(ctx, client)
	if err != nil {
		return nil, err
	}
	latest, err := parseStableSemver(manifest.Version)
	if err != nil {
		return nil, fmt.Errorf("parse manifest version: %w", err)
	}
	if compareSemver(latest, current) <= 0 {
		return nil, nil
	}

	platform, ok := manifest.Platforms[goos+"/"+goarch]
	if !ok {
		return nil, fmt.Errorf("platform %s/%s is not available", goos, goarch)
	}
	if _, err := parseURL(platform.BinaryURL); err != nil {
		return nil, fmt.Errorf("binary URL: %w", err)
	}
	release := &Release{
		Version:   latest.text,
		BinaryURL: platform.BinaryURL,
	}
	if goos == "windows" {
		if _, err := parseURL(platform.LibUSBURL); err != nil {
			return nil, fmt.Errorf("libusb URL: %w", err)
		}
		release.LibUSBURL = platform.LibUSBURL
	}
	return release, nil
}

func fetchManifest(ctx context.Context, client *http.Client) (updateManifest, error) {
	if client == nil {
		return updateManifest{}, fmt.Errorf("http client is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestManifestURL, nil)
	if err != nil {
		return updateManifest{}, fmt.Errorf("build manifest request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return updateManifest{}, fmt.Errorf("request update manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return updateManifest{}, fmt.Errorf("manifest request failed: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, defaultMaxManifestSize+1))
	if err != nil {
		return updateManifest{}, fmt.Errorf("read update manifest: %w", err)
	}
	if len(body) > defaultMaxManifestSize {
		return updateManifest{}, fmt.Errorf("update manifest is too large")
	}
	var manifest updateManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return updateManifest{}, fmt.Errorf("decode update manifest: %w", err)
	}
	if strings.TrimSpace(manifest.Version) == "" {
		return updateManifest{}, fmt.Errorf("update manifest version is required")
	}
	if manifest.Platforms == nil {
		return updateManifest{}, fmt.Errorf("update manifest platforms are required")
	}
	return manifest, nil
}

func parseStableSemver(s string) (semver, error) {
	if strings.TrimSpace(s) != s || s == "" {
		return semver{}, fmt.Errorf("must be major.minor.patch")
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("must be major.minor.patch")
	}
	values := [3]int{}
	for i, part := range parts {
		if part == "" {
			return semver{}, fmt.Errorf("must be major.minor.patch")
		}
		if len(part) > 1 && part[0] == '0' {
			return semver{}, fmt.Errorf("must not contain zero-padded components")
		}
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				return semver{}, fmt.Errorf("must be major.minor.patch")
			}
			n = n*10 + int(r-'0')
		}
		values[i] = n
	}
	return semver{major: values[0], minor: values[1], patch: values[2], text: s}, nil
}

func compareSemver(a, b semver) int {
	switch {
	case a.major != b.major:
		return compareInt(a.major, b.major)
	case a.minor != b.minor:
		return compareInt(a.minor, b.minor)
	default:
		return compareInt(a.patch, b.patch)
	}
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func parseURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) != raw || raw == "" {
		return nil, fmt.Errorf("download URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid download URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("download URL must use HTTPS")
	}
	return u, nil
}
