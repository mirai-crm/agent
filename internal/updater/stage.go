package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	defaultMaxChecksumsBytes = 1 << 20
	defaultMaxArchiveBytes   = 1 << 30
	defaultMaxExpandedBytes  = 1 << 30
)

var ErrHTTPClientRequired = errors.New("http client is required")

type Stager struct {
	MaxChecksumsBytes int64
	MaxArchiveBytes   int64
	MaxExpandedBytes  int64
}

type StageOptions struct {
	TargetPath    string
	StagingParent string
}

type StageResult struct {
	Dir        string
	BinaryPath string
	LibUSBPath string
}

func (s Stager) Stage(ctx context.Context, client *http.Client, release Release, opts StageOptions) (*StageResult, error) {
	if client == nil {
		return nil, ErrHTTPClientRequired
	}
	assetURL, err := parseURL(release.AssetURL)
	if err != nil {
		return nil, fmt.Errorf("release asset %q: %w", release.AssetName, err)
	}
	checksumsURL, err := parseURL(release.ChecksumsURL)
	if err != nil {
		return nil, fmt.Errorf("release asset %q: %w", "checksums.txt", err)
	}
	plan, err := archivePlanForRelease(release.AssetName)
	if err != nil {
		return nil, err
	}
	stageParent, err := stageParentDir(opts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stageParent, 0o755); err != nil {
		return nil, fmt.Errorf("create staging parent: %w", err)
	}
	stageDir, err := os.MkdirTemp(stageParent, ".mirai-agent-stage-*")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(stageDir)
		}
	}()

	checksumsBody, err := s.downloadSmall(ctx, client, checksumsURL.String(), s.maxChecksumsBytes(), "checksums.txt")
	if err != nil {
		return nil, err
	}
	want, err := parseChecksumsFile(checksumsBody, release.AssetName)
	if err != nil {
		return nil, err
	}

	archivePath := filepath.Join(stageDir, "archive"+plan.ext)
	got, err := s.downloadArchive(ctx, client, assetURL.String(), archivePath)
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, fmt.Errorf("archive checksum mismatch")
	}

	result, err := s.extractArchive(archivePath, stageDir, plan)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(archivePath); err != nil {
		return nil, fmt.Errorf("remove downloaded archive: %w", err)
	}
	ok = true
	return result, nil
}

type archivePlan struct {
	ext           string
	base          string
	binaryName    string
	stageBinary   string
	requireLibUSB bool
	allowedExtras map[string]bool
}

func archivePlanForRelease(assetName string) (archivePlan, error) {
	switch {
	case strings.HasSuffix(assetName, ".tar.gz"):
		base := strings.TrimSuffix(assetName, ".tar.gz")
		return archivePlan{
			ext:         ".tar.gz",
			base:        base,
			binaryName:  "mirai-agent",
			stageBinary: "mirai-agent",
			allowedExtras: map[string]bool{
				"README.md":           true,
				"AGENTS.md":           true,
				"config.example.toml": true,
			},
		}, nil
	case strings.HasSuffix(assetName, ".zip"):
		base := strings.TrimSuffix(assetName, ".zip")
		return archivePlan{
			ext:           ".zip",
			base:          base,
			binaryName:    "mirai-agent.exe",
			stageBinary:   "mirai-agent.exe",
			requireLibUSB: true,
			allowedExtras: map[string]bool{
				"README.md":           true,
				"AGENTS.md":           true,
				"config.example.toml": true,
			},
		}, nil
	default:
		return archivePlan{}, fmt.Errorf("unsupported release asset %q", assetName)
	}
}

func stageParentDir(opts StageOptions) (string, error) {
	if strings.TrimSpace(opts.TargetPath) != "" {
		return filepath.Dir(opts.TargetPath), nil
	}
	if strings.TrimSpace(opts.StagingParent) != "" {
		return opts.StagingParent, nil
	}
	return "", fmt.Errorf("target path or staging parent is required")
}

func (s Stager) maxChecksumsBytes() int64 {
	if s.MaxChecksumsBytes > 0 {
		return s.MaxChecksumsBytes
	}
	return defaultMaxChecksumsBytes
}

func (s Stager) maxArchiveBytes() int64 {
	if s.MaxArchiveBytes > 0 {
		return s.MaxArchiveBytes
	}
	return defaultMaxArchiveBytes
}

func (s Stager) maxExpandedBytes() int64 {
	if s.MaxExpandedBytes > 0 {
		return s.MaxExpandedBytes
	}
	return defaultMaxExpandedBytes
}

func (s Stager) downloadSmall(ctx context.Context, client *http.Client, rawURL string, maxBytes int64, label string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", label, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", label, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s request failed: status %d", label, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", label, err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("%s response too large", label)
	}
	return body, nil
}

func (s Stager) downloadArchive(ctx context.Context, client *http.Client, rawURL, dstPath string) ([32]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return [32]byte{}, fmt.Errorf("build archive request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return [32]byte{}, fmt.Errorf("request archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return [32]byte{}, fmt.Errorf("archive request failed: status %d", resp.StatusCode)
	}

	file, err := os.Create(dstPath)
	if err != nil {
		return [32]byte{}, fmt.Errorf("create archive file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	hash := sha256.New()
	limited := &io.LimitedReader{R: resp.Body, N: s.maxArchiveBytes() + 1}
	written, err := io.Copy(io.MultiWriter(file, hash), limited)
	if err != nil {
		return [32]byte{}, fmt.Errorf("download archive: %w", err)
	}
	if written > s.maxArchiveBytes() {
		return [32]byte{}, fmt.Errorf("archive response too large")
	}
	var sum [32]byte
	copy(sum[:], hash.Sum(nil))
	return sum, nil
}

func parseChecksumsFile(body []byte, wantName string) ([32]byte, error) {
	text := strings.TrimSuffix(string(body), "\n")
	if text == "" {
		return [32]byte{}, fmt.Errorf("checksum for %q missing", wantName)
	}
	var (
		found bool
		sum   [32]byte
	)
	for _, line := range strings.Split(text, "\n") {
		got, name, err := parseChecksumLine(strings.TrimSuffix(line, "\r"))
		if err != nil {
			return [32]byte{}, err
		}
		if name != wantName {
			continue
		}
		if found {
			return [32]byte{}, fmt.Errorf("checksum for %q duplicated", wantName)
		}
		found = true
		sum = got
	}
	if !found {
		return [32]byte{}, fmt.Errorf("checksum for %q missing", wantName)
	}
	return sum, nil
}

func parseChecksumLine(line string) ([32]byte, string, error) {
	if len(line) < 67 {
		return [32]byte{}, "", fmt.Errorf("malformed checksum line")
	}
	if line[64] != ' ' || (line[65] != ' ' && line[65] != '*') {
		return [32]byte{}, "", fmt.Errorf("malformed checksum line")
	}
	name := line[66:]
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return [32]byte{}, "", fmt.Errorf("malformed checksum line")
	}
	raw, err := hex.DecodeString(line[:64])
	if err != nil {
		return [32]byte{}, "", fmt.Errorf("malformed checksum line")
	}
	var sum [32]byte
	copy(sum[:], raw)
	return sum, name, nil
}

func (s Stager) extractArchive(archivePath, stageDir string, plan archivePlan) (*StageResult, error) {
	switch plan.ext {
	case ".zip":
		return s.extractZIP(archivePath, stageDir, plan)
	case ".tar.gz":
		return s.extractTarGz(archivePath, stageDir, plan)
	default:
		return nil, fmt.Errorf("unsupported archive type")
	}
}

func (s Stager) extractZIP(archivePath, stageDir string, plan archivePlan) (*StageResult, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open zip archive: %w", err)
	}
	defer reader.Close()

	result := &StageResult{Dir: stageDir}
	var expanded int64
	for _, file := range reader.File {
		kind, err := classifyArchivePath(file.Name, plan)
		if err != nil {
			return nil, err
		}
		mode := file.Mode()
		if mode&os.ModeType != 0 && !mode.IsDir() {
			return nil, fmt.Errorf("archive entry %q must be a regular file", file.Name)
		}
		if kind == archiveEntryDir {
			continue
		}
		expanded, err = addExpanded(expanded, int64(file.UncompressedSize64), s.maxExpandedBytes())
		if err != nil {
			return nil, err
		}
		if kind == archiveEntryIgnored {
			continue
		}
		if err := extractZipFile(file, stageDir, plan, kind, result); err != nil {
			return nil, err
		}
	}
	return finalizeStageResult(result, plan)
}

func (s Stager) extractTarGz(archivePath, stageDir string, plan archivePlan) (*StageResult, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer file.Close()
	gzr, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("open gzip archive: %w", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)

	result := &StageResult{Dir: stageDir}
	var expanded int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		kind, err := classifyArchivePath(hdr.Name, plan)
		if err != nil {
			return nil, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if kind != archiveEntryDir {
				return nil, fmt.Errorf("archive entry %q has unexpected layout", hdr.Name)
			}
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return nil, fmt.Errorf("archive entry %q must be a regular file", hdr.Name)
		}
		expanded, err = addExpanded(expanded, hdr.Size, s.maxExpandedBytes())
		if err != nil {
			return nil, err
		}
		if kind == archiveEntryIgnored {
			continue
		}
		if err := extractTarFile(tr, hdr, stageDir, plan, kind, result); err != nil {
			return nil, err
		}
	}
	return finalizeStageResult(result, plan)
}

type archiveEntryKind int

const (
	archiveEntryIgnored archiveEntryKind = iota
	archiveEntryBinary
	archiveEntryLibUSB
	archiveEntryDir
)

func classifyArchivePath(name string, plan archivePlan) (archiveEntryKind, error) {
	if strings.Contains(name, "\\") {
		return archiveEntryIgnored, fmt.Errorf("archive entry %q has invalid path", name)
	}
	clean := path.Clean(name)
	if clean == "." || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return archiveEntryIgnored, fmt.Errorf("archive entry %q has invalid path", name)
	}
	if clean == plan.base {
		return archiveEntryDir, nil
	}
	prefix := plan.base + "/"
	if !strings.HasPrefix(clean, prefix) {
		return archiveEntryIgnored, fmt.Errorf("archive entry %q has unexpected layout", name)
	}
	rest := strings.TrimPrefix(clean, prefix)
	if rest == plan.binaryName {
		return archiveEntryBinary, nil
	}
	if plan.requireLibUSB && rest == "libusb-1.0.dll" {
		return archiveEntryLibUSB, nil
	}
	if strings.Contains(rest, "/") {
		return archiveEntryIgnored, fmt.Errorf("archive entry %q has unexpected layout", name)
	}
	if plan.allowedExtras[rest] {
		return archiveEntryIgnored, nil
	}
	return archiveEntryIgnored, fmt.Errorf("archive entry %q has unexpected layout", name)
}

func addExpanded(total, size, limit int64) (int64, error) {
	if size < 0 {
		return 0, fmt.Errorf("archive entry has invalid size")
	}
	next := total + size
	if next < total || next > limit {
		return 0, fmt.Errorf("archive expanded size too large")
	}
	return next, nil
}

func extractZipFile(file *zip.File, stageDir string, plan archivePlan, kind archiveEntryKind, result *StageResult) error {
	dstPath, err := reserveStagePath(stageDir, plan, kind, result)
	if err != nil {
		return err
	}
	reader, err := file.Open()
	if err != nil {
		return fmt.Errorf("open archive entry %q: %w", file.Name, err)
	}
	defer reader.Close()
	return writeStageFile(dstPath, reader, int64(file.UncompressedSize64), false)
}

func extractTarFile(tr *tar.Reader, hdr *tar.Header, stageDir string, plan archivePlan, kind archiveEntryKind, result *StageResult) error {
	dstPath, err := reserveStagePath(stageDir, plan, kind, result)
	if err != nil {
		return err
	}
	return writeStageFile(dstPath, io.LimitReader(tr, hdr.Size), hdr.Size, true)
}

func reserveStagePath(stageDir string, plan archivePlan, kind archiveEntryKind, result *StageResult) (string, error) {
	switch kind {
	case archiveEntryBinary:
		if result.BinaryPath != "" {
			return "", fmt.Errorf("archive contains duplicate binary")
		}
		result.BinaryPath = filepath.Join(stageDir, plan.stageBinary)
		return result.BinaryPath, nil
	case archiveEntryLibUSB:
		if result.LibUSBPath != "" {
			return "", fmt.Errorf("archive contains duplicate libusb dll")
		}
		result.LibUSBPath = filepath.Join(stageDir, "libusb-1.0.dll")
		return result.LibUSBPath, nil
	default:
		return "", fmt.Errorf("internal: unexpected archive entry")
	}
}

func writeStageFile(dstPath string, src io.Reader, size int64, executable bool) error {
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	file, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create staged file %q: %w", filepath.Base(dstPath), err)
	}
	n, copyErr := io.Copy(file, src)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("extract staged file %q: %w", filepath.Base(dstPath), copyErr)
	}
	if n != size {
		return fmt.Errorf("extract staged file %q: short read", filepath.Base(dstPath))
	}
	if closeErr != nil {
		return fmt.Errorf("close staged file %q: %w", filepath.Base(dstPath), closeErr)
	}
	return nil
}

func finalizeStageResult(result *StageResult, plan archivePlan) (*StageResult, error) {
	if result.BinaryPath == "" {
		return nil, fmt.Errorf("archive missing binary")
	}
	if plan.requireLibUSB && result.LibUSBPath == "" {
		return nil, fmt.Errorf("archive missing libusb dll")
	}
	return result, nil
}
