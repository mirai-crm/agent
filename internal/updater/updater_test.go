package updater

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckerCheckFindsNewerStableRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/mirai-crm/agent/releases/latest" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		fmt.Fprint(w, `{
			"tag_name":"v1.2.3",
			"draft":false,
			"prerelease":false,
			"assets":[
				{"name":"mirai-agent_1.2.3_linux_amd64.tar.gz","browser_download_url":"https://example.com/mirai-agent_1.2.3_linux_amd64.tar.gz"},
				{"name":"checksums.txt","browser_download_url":"https://example.com/checksums.txt"}
			]
		}`)
	}))
	defer server.Close()

	release, err := Checker{APIBaseURL: server.URL}.Check(context.Background(), server.Client(), "1.2.2", "linux", "amd64")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if release == nil {
		t.Fatal("Check() release = nil, want update")
	}
	if release.Version != "1.2.3" {
		t.Fatalf("Version = %q, want 1.2.3", release.Version)
	}
	if release.AssetName != "mirai-agent_1.2.3_linux_amd64.tar.gz" {
		t.Fatalf("AssetName = %q", release.AssetName)
	}
	if release.ChecksumsURL != "https://example.com/checksums.txt" {
		t.Fatalf("ChecksumsURL = %q", release.ChecksumsURL)
	}
}

func TestCheckerCheckReturnsNilForEqualOrOlderRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"tag_name":"v1.2.3",
			"draft":false,
			"prerelease":false,
			"assets":[
				{"name":"mirai-agent_1.2.3_linux_amd64.tar.gz","browser_download_url":"https://example.com/mirai-agent_1.2.3_linux_amd64.tar.gz"},
				{"name":"checksums.txt","browser_download_url":"https://example.com/checksums.txt"}
			]
		}`)
	}))
	defer server.Close()

	checker := Checker{APIBaseURL: server.URL}
	for _, currentVersion := range []string{"1.2.3", "1.2.4"} {
		release, err := checker.Check(context.Background(), server.Client(), currentVersion, "linux", "amd64")
		if err != nil {
			t.Fatalf("Check(%q) error = %v", currentVersion, err)
		}
		if release != nil {
			t.Fatalf("Check(%q) release = %#v, want nil", currentVersion, release)
		}
	}
}

func TestCheckerCheckRejectsMalformedVersionOrMetadata(t *testing.T) {
	cases := map[string]string{
		"bad version": `{
			"tag_name":"v1.2",
			"draft":false,
			"prerelease":false,
			"assets":[
				{"name":"mirai-agent_1.2_linux_amd64.tar.gz","browser_download_url":"https://example.com/mirai-agent_1.2_linux_amd64.tar.gz"},
				{"name":"checksums.txt","browser_download_url":"https://example.com/checksums.txt"}
			]
		}`,
		"missing asset url": `{
			"tag_name":"v1.2.3",
			"draft":false,
			"prerelease":false,
			"assets":[
				{"name":"mirai-agent_1.2.3_linux_amd64.tar.gz","browser_download_url":""},
				{"name":"checksums.txt","browser_download_url":"https://example.com/checksums.txt"}
			]
		}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer server.Close()

			_, err := Checker{APIBaseURL: server.URL}.Check(context.Background(), server.Client(), "1.2.2", "linux", "amd64")
			if err == nil {
				t.Fatal("Check() error = nil, want error")
			}
		})
	}
}

func TestCheckerCheckRejectsPrereleaseOrDraft(t *testing.T) {
	cases := map[string]string{
		"prerelease": `{
			"tag_name":"v1.2.3",
			"draft":false,
			"prerelease":true,
			"assets":[
				{"name":"mirai-agent_1.2.3_linux_amd64.tar.gz","browser_download_url":"https://example.com/mirai-agent_1.2.3_linux_amd64.tar.gz"},
				{"name":"checksums.txt","browser_download_url":"https://example.com/checksums.txt"}
			]
		}`,
		"draft": `{
			"tag_name":"v1.2.3",
			"draft":true,
			"prerelease":false,
			"assets":[
				{"name":"mirai-agent_1.2.3_linux_amd64.tar.gz","browser_download_url":"https://example.com/mirai-agent_1.2.3_linux_amd64.tar.gz"},
				{"name":"checksums.txt","browser_download_url":"https://example.com/checksums.txt"}
			]
		}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer server.Close()

			_, err := Checker{APIBaseURL: server.URL}.Check(context.Background(), server.Client(), "1.2.2", "linux", "amd64")
			if err == nil {
				t.Fatal("Check() error = nil, want error")
			}
		})
	}
}

func TestCheckerCheckRejectsUnsupportedPlatform(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"tag_name":"v1.2.3",
			"draft":false,
			"prerelease":false,
			"assets":[
				{"name":"mirai-agent_1.2.3_linux_amd64.tar.gz","browser_download_url":"https://example.com/mirai-agent_1.2.3_linux_amd64.tar.gz"},
				{"name":"checksums.txt","browser_download_url":"https://example.com/checksums.txt"}
			]
		}`)
	}))
	defer server.Close()

	_, err := Checker{APIBaseURL: server.URL}.Check(context.Background(), server.Client(), "1.2.2", "darwin", "arm64")
	if err == nil {
		t.Fatal("Check() error = nil, want error")
	}
}

func TestCheckerCheckRequiresChecksumsAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"tag_name":"v1.2.3",
			"draft":false,
			"prerelease":false,
			"assets":[
				{"name":"mirai-agent_1.2.3_linux_amd64.tar.gz","browser_download_url":"https://example.com/mirai-agent_1.2.3_linux_amd64.tar.gz"}
			]
		}`)
	}))
	defer server.Close()

	_, err := Checker{APIBaseURL: server.URL}.Check(context.Background(), server.Client(), "1.2.2", "linux", "amd64")
	if err == nil {
		t.Fatal("Check() error = nil, want error")
	}
}

func TestCheckerCheckHandlesNon2xxResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := Checker{APIBaseURL: server.URL}.Check(context.Background(), server.Client(), "1.2.2", "linux", "amd64")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("Check() error = %v, want HTTP status error", err)
	}
}

func TestCheckerCheckRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 64))
	}))
	defer server.Close()

	_, err := Checker{
		APIBaseURL:       server.URL,
		MaxResponseBytes: 32,
	}.Check(context.Background(), server.Client(), "1.2.2", "linux", "amd64")
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("Check() error = %v, want size error", err)
	}
}

func TestCheckerCheckSkipsDevVersionWithoutNetwork(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	release, err := Checker{APIBaseURL: server.URL}.Check(context.Background(), server.Client(), "dev", "linux", "amd64")
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if release != nil {
		t.Fatalf("Check() release = %#v, want nil", release)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}
