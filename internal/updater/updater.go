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
	defaultAPIBaseURL       = "https://api.github.com"
	defaultMaxResponseBytes = 1 << 20
	releaseRepo             = "mirai-crm/agent"
)

type Checker struct {
	APIBaseURL       string
	MaxResponseBytes int64
}

type Release struct {
	Version      string
	TagName      string
	AssetName    string
	AssetURL     string
	ChecksumsURL string
}

type semver struct {
	major int
	minor int
	patch int
	text  string
}

type releaseAPIResponse struct {
	TagName    string            `json:"tag_name"`
	Draft      bool              `json:"draft"`
	Prerelease bool              `json:"prerelease"`
	Assets     []releaseAPIAsset `json:"assets"`
}

type releaseAPIAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

func (c Checker) Check(ctx context.Context, client *http.Client, currentVersion, goos, goarch string) (*Release, error) {
	if currentVersion == "dev" {
		return nil, nil
	}
	current, err := parseStableSemver(currentVersion)
	if err != nil {
		return nil, fmt.Errorf("parse current version: %w", err)
	}
	resp, err := c.fetchLatestRelease(ctx, client)
	if err != nil {
		return nil, err
	}
	latest, err := releaseFromAPI(resp, goos, goarch)
	if err != nil {
		return nil, err
	}
	if compareSemver(latest, current) <= 0 {
		return nil, nil
	}
	assetName := releaseAssetName(latest.text, goos, goarch)
	return &Release{
		Version:      latest.text,
		TagName:      "v" + latest.text,
		AssetName:    assetName,
		AssetURL:     assetURLByName(resp.Assets, assetName),
		ChecksumsURL: assetURLByName(resp.Assets, "checksums.txt"),
	}, nil
}

func (c Checker) fetchLatestRelease(ctx context.Context, client *http.Client) (releaseAPIResponse, error) {
	if client == nil {
		return releaseAPIResponse{}, fmt.Errorf("http client is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.latestReleaseURL(), nil)
	if err != nil {
		return releaseAPIResponse{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return releaseAPIResponse{}, fmt.Errorf("request latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return releaseAPIResponse{}, fmt.Errorf("latest release request failed: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes()+1))
	if err != nil {
		return releaseAPIResponse{}, fmt.Errorf("read latest release response: %w", err)
	}
	if int64(len(body)) > c.maxResponseBytes() {
		return releaseAPIResponse{}, fmt.Errorf("latest release response too large")
	}
	var parsed releaseAPIResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return releaseAPIResponse{}, fmt.Errorf("decode latest release response: %w", err)
	}
	return parsed, nil
}

func (c Checker) latestReleaseURL() string {
	base := c.APIBaseURL
	if strings.TrimSpace(base) == "" {
		base = defaultAPIBaseURL
	}
	return strings.TrimRight(base, "/") + "/repos/" + releaseRepo + "/releases/latest"
}

func (c Checker) maxResponseBytes() int64 {
	if c.MaxResponseBytes > 0 {
		return c.MaxResponseBytes
	}
	return defaultMaxResponseBytes
}

func releaseFromAPI(resp releaseAPIResponse, goos, goarch string) (semver, error) {
	if resp.Draft {
		return semver{}, fmt.Errorf("latest release is a draft")
	}
	if resp.Prerelease {
		return semver{}, fmt.Errorf("latest release is a prerelease")
	}
	version, err := parseReleaseTag(resp.TagName)
	if err != nil {
		return semver{}, fmt.Errorf("parse release tag: %w", err)
	}
	assetName := releaseAssetName(version.text, goos, goarch)
	assetURL := assetURLByName(resp.Assets, assetName)
	if assetURL == "" {
		return semver{}, fmt.Errorf("release asset %q not found", assetName)
	}
	if _, err := parseURL(assetURL); err != nil {
		return semver{}, fmt.Errorf("release asset %q: %w", assetName, err)
	}
	checksumsURL := assetURLByName(resp.Assets, "checksums.txt")
	if checksumsURL == "" {
		return semver{}, fmt.Errorf("release asset %q not found", "checksums.txt")
	}
	if _, err := parseURL(checksumsURL); err != nil {
		return semver{}, fmt.Errorf("release asset %q: %w", "checksums.txt", err)
	}
	return version, nil
}

func parseReleaseTag(tag string) (semver, error) {
	if !strings.HasPrefix(tag, "v") {
		return semver{}, fmt.Errorf("must start with v")
	}
	return parseStableSemver(strings.TrimPrefix(tag, "v"))
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

func releaseAssetName(version, goos, goarch string) string {
	base := "mirai-agent_" + version + "_" + goos + "_" + goarch
	if goos == "windows" {
		return base + ".zip"
	}
	return base + ".tar.gz"
}

func assetURLByName(assets []releaseAPIAsset, want string) string {
	for _, asset := range assets {
		if asset.Name == want {
			return asset.URL
		}
	}
	return ""
}

func parseURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) != raw || raw == "" {
		return nil, fmt.Errorf("download url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid download url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid download url")
	}
	return u, nil
}
