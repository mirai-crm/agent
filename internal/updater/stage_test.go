package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStagerStageTarGzSuccess(t *testing.T) {
	t.Parallel()

	archiveName := "mirai-agent_1.2.3_linux_amd64.tar.gz"
	payload := tarGzArchive(t, archiveFixture{
		base:   strings.TrimSuffix(archiveName, ".tar.gz"),
		binary: "linux-binary",
		docs:   true,
	})
	stage, parent := mustStageRelease(t, archiveName, payload, checksumFor(archiveName, payload), StageOptions{
		TargetPath: filepath.Join(t.TempDir(), "mirai-agent"),
	})

	if got := filepath.Dir(stage.Dir); got != parent {
		t.Fatalf("stage dir parent = %q, want %q", got, parent)
	}
	if stage.BinaryPath == "" {
		t.Fatal("BinaryPath = empty")
	}
	if stage.LibUSBPath != "" {
		t.Fatalf("LibUSBPath = %q, want empty", stage.LibUSBPath)
	}
	if got := readFile(t, stage.BinaryPath); got != "linux-binary" {
		t.Fatalf("binary contents = %q", got)
	}
	info, err := os.Stat(stage.BinaryPath)
	if err != nil {
		t.Fatalf("stat binary: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("binary mode = %v, want executable", info.Mode().Perm())
	}
	assertDirEntries(t, stage.Dir, "mirai-agent")
}

func TestStagerStageZipSuccessRequiresDLL(t *testing.T) {
	t.Parallel()

	archiveName := "mirai-agent_1.2.3_windows_amd64.zip"
	base := strings.TrimSuffix(archiveName, ".zip")
	payload := zipArchive(t, archiveFixture{
		base:   base,
		binary: "windows-binary",
		dll:    "libusb-dll",
		docs:   true,
		extra:  []zipEntry{{name: base + "/", mode: os.ModeDir | 0o755}},
	})
	stage, _ := mustStageRelease(t, archiveName, payload, checksumFor(archiveName, payload), StageOptions{
		StagingParent: t.TempDir(),
	})

	if got := readFile(t, stage.BinaryPath); got != "windows-binary" {
		t.Fatalf("binary contents = %q", got)
	}
	if got := readFile(t, stage.LibUSBPath); got != "libusb-dll" {
		t.Fatalf("dll contents = %q", got)
	}
	assertDirEntries(t, stage.Dir, "libusb-1.0.dll", "mirai-agent.exe")
}

func TestStagerStageRejectsChecksumProblems(t *testing.T) {
	t.Parallel()

	archiveName := "mirai-agent_1.2.3_linux_amd64.tar.gz"
	payload := tarGzArchive(t, archiveFixture{
		base:   strings.TrimSuffix(archiveName, ".tar.gz"),
		binary: "linux-binary",
	})

	tests := map[string]string{
		"missing":   checksumFor("other.tar.gz", payload),
		"duplicate": checksumFor(archiveName, payload) + checksumFor(archiveName, payload),
		"malformed": "not-a-sha  " + archiveName + "\n",
		"mismatch":  checksumFor(archiveName, []byte("wrong")) + checksumFor("other.tar.gz", payload),
	}

	for name, checksums := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := stageReleaseForTest(t, archiveName, payload, checksums, StageOptions{
				StagingParent: t.TempDir(),
			}, Stager{})
			if err == nil {
				t.Fatal("Stage() error = nil, want error")
			}
		})
	}
}

func TestStagerStageRejectsHTTPFailuresAndBodyLimits(t *testing.T) {
	t.Parallel()

	archiveName := "mirai-agent_1.2.3_linux_amd64.tar.gz"
	payload := tarGzArchive(t, archiveFixture{
		base:   strings.TrimSuffix(archiveName, ".tar.gz"),
		binary: "linux-binary",
	})
	checksums := checksumFor(archiveName, payload)

	t.Run("archive status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/checksums.txt" {
				fmt.Fprint(w, checksums)
				return
			}
			http.Error(w, "boom", http.StatusBadGateway)
		}))
		defer server.Close()

		_, err := Stager{}.Stage(context.Background(), server.Client(), Release{
			AssetName:    archiveName,
			AssetURL:     server.URL + "/archive",
			ChecksumsURL: server.URL + "/checksums.txt",
		}, StageOptions{StagingParent: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "502") {
			t.Fatalf("Stage() error = %v, want HTTP status error", err)
		}
	})

	t.Run("checksum status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/archive" {
				_, _ = w.Write(payload)
				return
			}
			http.Error(w, "boom", http.StatusServiceUnavailable)
		}))
		defer server.Close()

		_, err := Stager{}.Stage(context.Background(), server.Client(), Release{
			AssetName:    archiveName,
			AssetURL:     server.URL + "/archive",
			ChecksumsURL: server.URL + "/checksums.txt",
		}, StageOptions{StagingParent: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("Stage() error = %v, want HTTP status error", err)
		}
	})

	t.Run("archive too large", func(t *testing.T) {
		_, err := stageReleaseForTest(t, archiveName, payload, checksums, StageOptions{
			StagingParent: t.TempDir(),
		}, Stager{MaxArchiveBytes: int64(len(payload) - 1)})
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("Stage() error = %v, want size error", err)
		}
	})

	t.Run("checksums too large", func(t *testing.T) {
		_, err := stageReleaseForTest(t, archiveName, payload, checksums, StageOptions{
			StagingParent: t.TempDir(),
		}, Stager{MaxChecksumsBytes: int64(len(checksums) - 1)})
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("Stage() error = %v, want size error", err)
		}
	})

	t.Run("archive truncated", func(t *testing.T) {
		server := truncatedServer(t, "/archive", payload, "/checksums.txt", []byte(checksums))
		defer server.Close()

		_, err := Stager{}.Stage(context.Background(), server.Client(), Release{
			AssetName:    archiveName,
			AssetURL:     server.URL + "/archive",
			ChecksumsURL: server.URL + "/checksums.txt",
		}, StageOptions{StagingParent: t.TempDir()})
		if err == nil {
			t.Fatal("Stage() error = nil, want error")
		}
	})

	t.Run("checksums truncated", func(t *testing.T) {
		server := truncatedServer(t, "/checksums.txt", []byte(checksums), "/archive", payload)
		defer server.Close()

		_, err := Stager{}.Stage(context.Background(), server.Client(), Release{
			AssetName:    archiveName,
			AssetURL:     server.URL + "/archive",
			ChecksumsURL: server.URL + "/checksums.txt",
		}, StageOptions{StagingParent: t.TempDir()})
		if err == nil {
			t.Fatal("Stage() error = nil, want error")
		}
	})
}

func TestStagerStageRejectsUnsafeTarLayout(t *testing.T) {
	t.Parallel()

	archiveName := "mirai-agent_1.2.3_linux_amd64.tar.gz"
	base := strings.TrimSuffix(archiveName, ".tar.gz")

	tests := map[string][]tarEntry{
		"path traversal": {
			{name: base + "/../mirai-agent", body: []byte("bad"), mode: 0o644, typeflag: tar.TypeReg},
		},
		"symlink": {
			{name: base + "/mirai-agent", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "somewhere"},
		},
		"hardlink": {
			{name: base + "/mirai-agent", mode: 0o777, typeflag: tar.TypeLink, linkname: "somewhere"},
		},
		"device": {
			{name: base + "/mirai-agent", mode: 0o644, typeflag: tar.TypeChar},
		},
		"duplicate binary": {
			{name: base + "/mirai-agent", body: []byte("one"), mode: 0o644, typeflag: tar.TypeReg},
			{name: base + "/mirai-agent", body: []byte("two"), mode: 0o644, typeflag: tar.TypeReg},
		},
		"unexpected nesting": {
			{name: base + "/bin/mirai-agent", body: []byte("bad"), mode: 0o644, typeflag: tar.TypeReg},
		},
		"missing binary": {
			{name: base + "/README.md", body: []byte("docs"), mode: 0o644, typeflag: tar.TypeReg},
		},
	}

	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			payload := tarGzEntries(t, entries)
			_, err := stageReleaseForTest(t, archiveName, payload, checksumFor(archiveName, payload), StageOptions{
				StagingParent: t.TempDir(),
			}, Stager{})
			if err == nil {
				t.Fatal("Stage() error = nil, want error")
			}
		})
	}
}

func TestStagerStageRejectsUnsafeZipLayout(t *testing.T) {
	t.Parallel()

	archiveName := "mirai-agent_1.2.3_windows_amd64.zip"
	base := strings.TrimSuffix(archiveName, ".zip")

	tests := map[string]archiveFixture{
		"path traversal": {
			base:    base,
			binary:  "windows-binary",
			replace: map[string]string{base + "/../mirai-agent.exe": "windows-binary"},
			remove:  []string{base + "/mirai-agent.exe"},
		},
		"duplicate binary": {
			base:   base,
			binary: "one",
			extra: []zipEntry{
				{name: base + "/mirai-agent.exe", body: []byte("two")},
			},
		},
		"missing dll": {
			base:   base,
			binary: "windows-binary",
		},
		"unexpected nesting": {
			base: base,
			extra: []zipEntry{
				{name: base + "/bin/mirai-agent.exe", body: []byte("bad")},
			},
		},
		"missing binary": {
			base: base,
			dll:  "libusb-dll",
			remove: []string{
				base + "/mirai-agent.exe",
			},
		},
	}

	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			payload := zipArchive(t, fixture)
			_, err := stageReleaseForTest(t, archiveName, payload, checksumFor(archiveName, payload), StageOptions{
				StagingParent: t.TempDir(),
			}, Stager{})
			if err == nil {
				t.Fatal("Stage() error = nil, want error")
			}
		})
	}
}

func TestStagerStageRejectsTopLevelZipPathUnlessDirectory(t *testing.T) {
	t.Parallel()

	archiveName := "mirai-agent_1.2.3_windows_amd64.zip"
	base := strings.TrimSuffix(archiveName, ".zip")
	tests := map[string]zipEntry{
		"regular file at top level path": {name: base, body: []byte("not a directory")},
		"nested directory":               {name: base + "/bin/", mode: os.ModeDir | 0o755},
	}

	for name, entry := range tests {
		t.Run(name, func(t *testing.T) {
			payload := zipEntriesArchive(t, []zipEntry{
				entry,
				{name: base + "/mirai-agent.exe", body: []byte("windows-binary")},
				{name: base + "/libusb-1.0.dll", body: []byte("libusb-dll")},
			})
			_, err := stageReleaseForTest(t, archiveName, payload, checksumFor(archiveName, payload), StageOptions{
				StagingParent: t.TempDir(),
			}, Stager{})
			if err == nil {
				t.Fatal("Stage() error = nil, want layout error")
			}
		})
	}
}

func TestStagerZipExpandedLimitUsesActualBytesCopied(t *testing.T) {
	t.Parallel()

	archiveName := "mirai-agent_1.2.3_windows_amd64.zip"
	base := strings.TrimSuffix(archiveName, ".zip")
	validPayload := zipEntriesArchive(t, []zipEntry{
		{name: base + "/mirai-agent.exe", body: []byte(strings.Repeat("x", 32))},
		{name: base + "/libusb-1.0.dll", body: []byte("dll")},
	})
	payload := setZipCentralUncompressedSize(t, validPayload, base+"/mirai-agent.exe", 1)

	archivePath := filepath.Join(t.TempDir(), archiveName)
	if err := os.WriteFile(archivePath, payload, 0o600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	stageDir := t.TempDir()
	plan, err := archivePlanForRelease(archiveName)
	if err != nil {
		t.Fatalf("archivePlanForRelease(): %v", err)
	}

	_, err = (Stager{MaxExpandedBytes: 8}).extractZIP(archivePath, stageDir, plan)
	if err == nil {
		t.Fatal("extractZIP() error = nil, want size error")
	}
	info, statErr := os.Stat(filepath.Join(stageDir, "mirai-agent.exe"))
	if statErr != nil {
		t.Fatalf("Stat(): %v", statErr)
	}
	if info.Size() > 9 {
		t.Fatalf("staged bytes = %d, want at most limit plus detection byte", info.Size())
	}

	stagingParent := t.TempDir()
	_, err = stageReleaseForTest(t, archiveName, validPayload, checksumFor(archiveName, validPayload), StageOptions{
		StagingParent: stagingParent,
	}, Stager{MaxExpandedBytes: 8})
	if err == nil || !strings.Contains(err.Error(), "expanded") {
		t.Fatalf("Stage() error = %v, want expanded size error", err)
	}
	entries, readErr := os.ReadDir(stagingParent)
	if readErr != nil {
		t.Fatalf("ReadDir(): %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("staging parent entries = %d, want cleanup", len(entries))
	}
}

func TestWriteZipStageFileBoundsActualOutput(t *testing.T) {
	t.Parallel()

	dstPath := filepath.Join(t.TempDir(), "mirai-agent.exe")
	written, err := writeZipStageFile(dstPath, strings.NewReader(strings.Repeat("x", 32)), "mirai-agent.exe", 32, 8)
	if err == nil || !strings.Contains(err.Error(), "expanded") {
		t.Fatalf("writeZipStageFile() error = %v, want expanded size error", err)
	}
	if written != 9 {
		t.Fatalf("written = %d, want remaining allowance plus one detection byte", written)
	}
	info, statErr := os.Stat(dstPath)
	if statErr != nil {
		t.Fatalf("Stat(): %v", statErr)
	}
	if info.Size() != 9 {
		t.Fatalf("staged bytes = %d, want 9", info.Size())
	}
}

func TestStagerStageRejectsExpandedSizeOverflow(t *testing.T) {
	t.Parallel()

	archiveName := "mirai-agent_1.2.3_linux_amd64.tar.gz"
	payload := tarGzArchive(t, archiveFixture{
		base:   strings.TrimSuffix(archiveName, ".tar.gz"),
		binary: strings.Repeat("x", 32),
	})

	_, err := stageReleaseForTest(t, archiveName, payload, checksumFor(archiveName, payload), StageOptions{
		StagingParent: t.TempDir(),
	}, Stager{MaxExpandedBytes: 8})
	if err == nil || !strings.Contains(err.Error(), "expanded") {
		t.Fatalf("Stage() error = %v, want expanded size error", err)
	}
}

func TestStagerStageCleansUpOnFailure(t *testing.T) {
	t.Parallel()

	stagingParent := t.TempDir()
	archiveName := "mirai-agent_1.2.3_linux_amd64.tar.gz"
	payload := tarGzArchive(t, archiveFixture{
		base:   strings.TrimSuffix(archiveName, ".tar.gz"),
		binary: "linux-binary",
	})

	_, err := stageReleaseForTest(t, archiveName, payload, checksumFor(archiveName, []byte("wrong")), StageOptions{
		StagingParent: stagingParent,
	}, Stager{})
	if err == nil {
		t.Fatal("Stage() error = nil, want error")
	}

	entries, readErr := os.ReadDir(stagingParent)
	if readErr != nil {
		t.Fatalf("ReadDir(): %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("staging parent entries = %d, want 0", len(entries))
	}
}

type archiveFixture struct {
	base    string
	binary  string
	dll     string
	docs    bool
	extra   []zipEntry
	remove  []string
	replace map[string]string
}

type zipEntry struct {
	name string
	body []byte
	mode os.FileMode
}

type tarEntry struct {
	name     string
	body     []byte
	mode     int64
	typeflag byte
	linkname string
}

func mustStageRelease(t *testing.T, archiveName string, payload []byte, checksums string, opts StageOptions) (*StageResult, string) {
	t.Helper()

	stage, err := stageReleaseForTest(t, archiveName, payload, checksums, opts, Stager{})
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}
	parent := opts.StagingParent
	if opts.TargetPath != "" {
		parent = filepath.Dir(opts.TargetPath)
	}
	return stage, parent
}

func stageReleaseForTest(t *testing.T, archiveName string, payload []byte, checksums string, opts StageOptions, stager Stager) (*StageResult, error) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/archive":
			_, _ = w.Write(payload)
		case "/checksums.txt":
			_, _ = io.WriteString(w, checksums)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	return stager.Stage(context.Background(), server.Client(), Release{
		Version:      "1.2.3",
		TagName:      "v1.2.3",
		AssetName:    archiveName,
		AssetURL:     server.URL + "/archive",
		ChecksumsURL: server.URL + "/checksums.txt",
	}, opts)
}

func checksumFor(name string, payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]) + "  " + name + "\n"
}

func tarGzArchive(t *testing.T, fixture archiveFixture) []byte {
	t.Helper()

	entries := []tarEntry{
		{name: fixture.base + "/mirai-agent", body: []byte(fixture.binary), mode: 0o755, typeflag: tar.TypeReg},
	}
	if fixture.docs {
		entries = append(entries,
			tarEntry{name: fixture.base + "/README.md", body: []byte("docs"), mode: 0o644, typeflag: tar.TypeReg},
			tarEntry{name: fixture.base + "/AGENTS.md", body: []byte("agents"), mode: 0o644, typeflag: tar.TypeReg},
			tarEntry{name: fixture.base + "/config.example.toml", body: []byte("config"), mode: 0o644, typeflag: tar.TypeReg},
		)
	}
	return tarGzEntries(t, entries)
}

func tarGzEntries(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for _, entry := range entries {
		hdr := &tar.Header{
			Name:     entry.name,
			Mode:     entry.mode,
			Size:     int64(len(entry.body)),
			Typeflag: entry.typeflag,
			Linkname: entry.linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", entry.name, err)
		}
		if entry.typeflag == tar.TypeReg && len(entry.body) > 0 {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatalf("Write(%q): %v", entry.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func zipArchive(t *testing.T, fixture archiveFixture) []byte {
	t.Helper()

	entries := map[string][]byte{
		fixture.base + "/mirai-agent.exe": []byte(fixture.binary),
	}
	if fixture.dll != "" {
		entries[fixture.base+"/libusb-1.0.dll"] = []byte(fixture.dll)
	}
	if fixture.docs {
		entries[fixture.base+"/README.md"] = []byte("docs")
		entries[fixture.base+"/AGENTS.md"] = []byte("agents")
		entries[fixture.base+"/config.example.toml"] = []byte("config")
	}
	for _, name := range fixture.remove {
		delete(entries, name)
	}
	for name, body := range fixture.replace {
		entries[name] = []byte(body)
	}

	var entriesInOrder []zipEntry
	for name, body := range entries {
		entriesInOrder = append(entriesInOrder, zipEntry{name: name, body: body})
	}
	entriesInOrder = append(entriesInOrder, fixture.extra...)
	return zipEntriesArchive(t, entriesInOrder)
}

func zipEntriesArchive(t *testing.T, entries []zipEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatalf("Create(%q): %v", entry.name, err)
		}
		if _, err := w.Write(entry.body); err != nil {
			t.Fatalf("Write(%q): %v", entry.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func setZipCentralUncompressedSize(t *testing.T, payload []byte, name string, size uint32) []byte {
	t.Helper()

	payload = bytes.Clone(payload)
	for offset := 0; ; {
		index := bytes.Index(payload[offset:], []byte("PK\x01\x02"))
		if index < 0 {
			break
		}
		index += offset
		if len(payload)-index < 46 {
			t.Fatal("truncated central directory header")
		}
		nameLen := int(binary.LittleEndian.Uint16(payload[index+28 : index+30]))
		extraLen := int(binary.LittleEndian.Uint16(payload[index+30 : index+32]))
		commentLen := int(binary.LittleEndian.Uint16(payload[index+32 : index+34]))
		end := index + 46 + nameLen + extraLen + commentLen
		if end > len(payload) {
			t.Fatal("truncated central directory entry")
		}
		if string(payload[index+46:index+46+nameLen]) == name {
			binary.LittleEndian.PutUint32(payload[index+24:index+28], size)
			return payload
		}
		offset = end
	}
	t.Fatalf("central directory entry %q not found", name)
	return nil
}

func truncatedServer(t *testing.T, truncatedPath string, truncatedBody []byte, okPath string, okBody []byte) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case okPath:
			_, _ = w.Write(okBody)
		case truncatedPath:
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("response writer does not support hijack")
			}
			conn, buf, err := hj.Hijack()
			if err != nil {
				t.Fatalf("Hijack(): %v", err)
			}
			defer conn.Close()
			_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nContent-Type: application/octet-stream\r\nConnection: close\r\n\r\n", len(truncatedBody)+16)
			_, _ = buf.Write(truncatedBody[:len(truncatedBody)/2])
			_ = buf.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return string(data)
}

func assertDirEntries(t *testing.T, dir string, want ...string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	if len(entries) != len(want) {
		t.Fatalf("entry count = %d, want %d", len(entries), len(want))
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		seen[entry.Name()] = true
	}
	for _, name := range want {
		if !seen[name] {
			t.Fatalf("missing %q in staged dir", name)
		}
	}
}

func TestStageRequiresHTTPClient(t *testing.T) {
	t.Parallel()

	_, err := Stager{}.Stage(context.Background(), nil, Release{}, StageOptions{StagingParent: t.TempDir()})
	if !errors.Is(err, ErrHTTPClientRequired) {
		t.Fatalf("Stage() error = %v, want ErrHTTPClientRequired", err)
	}
}

var _ net.Conn
