package paymentjournal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
)

// Entry is one durable payment journal record keyed by device/task pair.
type Entry struct {
	DeviceID    int64                  `json:"deviceId"`
	TaskID      int64                  `json:"taskId"`
	Input       map[string]interface{} `json:"input"`
	RequestSent bool                   `json:"requestSent"`
	Data        map[string]interface{} `json:"data"`
}

// Journal persists payment task progress across restarts.
type Journal struct {
	path string

	mu      sync.Mutex
	entries map[journalKey]Entry
	syncDir func(string) error
}

type journalKey struct {
	DeviceID int64
	TaskID   int64
}

type persistedJournal struct {
	Entries []persistedEntry `json:"entries"`
}

type persistedEntry struct {
	DeviceID    int64                  `json:"deviceId"`
	TaskID      int64                  `json:"taskId"`
	Input       map[string]interface{} `json:"input"`
	RequestSent bool                   `json:"requestSent"`
	Data        map[string]interface{} `json:"data"`
}

// Open loads an existing journal or creates an empty in-memory one.
func Open(path string) (*Journal, error) {
	j := &Journal{
		path:    path,
		entries: make(map[journalKey]Entry),
		syncDir: syncDir,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return j, nil
		}
		return nil, fmt.Errorf("read payment journal %s: %w", path, err)
	}

	var stored persistedJournal
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode payment journal %s: %w", path, err)
	}
	var trailing interface{}
	switch err := decoder.Decode(&trailing); {
	case err == nil:
		return nil, fmt.Errorf("decode payment journal %s: trailing JSON value", path)
	case err != io.EOF:
		return nil, fmt.Errorf("decode payment journal %s: trailing data: %w", path, err)
	}

	for _, entry := range stored.Entries {
		key := journalKey{DeviceID: entry.DeviceID, TaskID: entry.TaskID}
		if _, exists := j.entries[key]; exists {
			return nil, fmt.Errorf("decode payment journal %s: duplicate entry for device %d task %d", path, entry.DeviceID, entry.TaskID)
		}
		j.entries[key] = Entry{
			DeviceID:    entry.DeviceID,
			TaskID:      entry.TaskID,
			Input:       cloneMap(entry.Input),
			RequestSent: entry.RequestSent,
			Data:        cloneMap(entry.Data),
		}
	}

	needsRecovery := false
	recovered := cloneEntries(j.entries)
	for key, entry := range recovered {
		if entry.Data != nil {
			continue
		}
		entry.Data = recoveryData(entry.Input, entry.RequestSent)
		recovered[key] = entry
		needsRecovery = true
	}
	if !needsRecovery {
		return j, nil
	}
	committed, err := persist(path, recovered, j.syncDir)
	if committed {
		j.entries = recovered
	}
	if err != nil {
		return nil, err
	}
	return j, nil
}

// Begin records intent before any terminal I/O starts. Repeated calls preserve
// the original entry.
func (j *Journal) Begin(deviceID, taskID int64, input map[string]interface{}) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	key := journalKey{DeviceID: deviceID, TaskID: taskID}
	if _, exists := j.entries[key]; exists {
		return nil
	}

	normalizedInput, err := normalizeMap(input)
	if err != nil {
		return fmt.Errorf("normalize payment journal input: %w", err)
	}

	next := cloneEntries(j.entries)
	next[key] = Entry{
		DeviceID: deviceID,
		TaskID:   taskID,
		Input:    normalizedInput,
	}
	return j.commit(next)
}

// MarkSent records that the terminal request was written.
func (j *Journal) MarkSent(deviceID, taskID int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	key := journalKey{DeviceID: deviceID, TaskID: taskID}
	entry, exists := j.entries[key]
	if !exists {
		return fmt.Errorf("payment journal entry not found for device %d task %d", deviceID, taskID)
	}
	if entry.RequestSent {
		return nil
	}

	next := cloneEntries(j.entries)
	entry = next[key]
	entry.RequestSent = true
	next[key] = entry
	return j.commit(next)
}

// Complete stores the exact finalize payload.
func (j *Journal) Complete(deviceID, taskID int64, data map[string]interface{}) error {
	if data == nil {
		return fmt.Errorf("payment journal completion data is required")
	}
	normalizedData, err := normalizeMap(data)
	if err != nil {
		return fmt.Errorf("normalize payment journal completion data: %w", err)
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	key := journalKey{DeviceID: deviceID, TaskID: taskID}
	entry, exists := j.entries[key]
	if !exists {
		return fmt.Errorf("payment journal entry not found for device %d task %d", deviceID, taskID)
	}

	next := cloneEntries(j.entries)
	entry = next[key]
	entry.Data = normalizedData
	next[key] = entry
	return j.commit(next)
}

// Get returns one entry by key.
func (j *Journal) Get(deviceID, taskID int64) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()

	entry, ok := j.entries[journalKey{DeviceID: deviceID, TaskID: taskID}]
	if !ok {
		return Entry{}, false
	}
	return cloneEntry(entry), true
}

// Pending returns all journal entries for one device in deterministic task-ID
// order.
func (j *Journal) Pending(deviceID int64) []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()

	pending := make([]Entry, 0, len(j.entries))
	for _, entry := range j.entries {
		if entry.DeviceID != deviceID {
			continue
		}
		pending = append(pending, cloneEntry(entry))
	}
	sort.Slice(pending, func(i, k int) bool {
		return pending[i].TaskID < pending[k].TaskID
	})
	return pending
}

// Remove deletes a finalized journal entry after CRM acknowledgement.
func (j *Journal) Remove(deviceID, taskID int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	key := journalKey{DeviceID: deviceID, TaskID: taskID}
	if _, exists := j.entries[key]; !exists {
		return nil
	}

	next := cloneEntries(j.entries)
	delete(next, key)
	return j.commit(next)
}

func recoveryData(input map[string]interface{}, requestSent bool) map[string]interface{} {
	data := cloneMap(input)
	if data == nil {
		data = make(map[string]interface{})
	}
	data["payment"] = map[string]interface{}{
		"status":           "unknown",
		"requestSent":      requestSent,
		"stage":            "recovered_after_restart",
		"errorDescription": "agent restarted before the terminal result was persisted",
	}
	return data
}

func (j *Journal) commit(next map[journalKey]Entry) error {
	committed, err := persist(j.path, next, j.syncDir)
	if committed {
		j.entries = next
	}
	return err
}

func persist(path string, entries map[journalKey]Entry, syncDirectory func(string) error) (bool, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("create payment journal dir %s: %w", dir, err)
	}

	state := persistedJournal{
		Entries: make([]persistedEntry, 0, len(entries)),
	}
	for _, key := range sortedKeys(entries) {
		entry := entries[key]
		state.Entries = append(state.Entries, persistedEntry{
			DeviceID:    entry.DeviceID,
			TaskID:      entry.TaskID,
			Input:       cloneMap(entry.Input),
			RequestSent: entry.RequestSent,
			Data:        cloneMap(entry.Data),
		})
	}

	data, err := json.Marshal(state)
	if err != nil {
		return false, fmt.Errorf("encode payment journal %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("create temp payment journal %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return false, fmt.Errorf("chmod temp payment journal %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write temp payment journal %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("sync temp payment journal %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close temp payment journal %s: %w", tmpPath, err)
	}
	// os.Rename replaces an existing destination on supported Go versions,
	// including Windows via MoveFileEx with MOVEFILE_REPLACE_EXISTING.
	if err := os.Rename(tmpPath, path); err != nil {
		return false, fmt.Errorf("rename payment journal %s: %w", path, err)
	}
	removeTmp = false
	if runtime.GOOS != "windows" {
		if err := syncDirectory(dir); err != nil {
			return true, err
		}
	}
	return true, nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open payment journal dir %s: %w", dir, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync payment journal dir %s: %w", dir, err)
	}
	return nil
}

func sortedKeys(entries map[journalKey]Entry) []journalKey {
	keys := make([]journalKey, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].DeviceID != keys[j].DeviceID {
			return keys[i].DeviceID < keys[j].DeviceID
		}
		return keys[i].TaskID < keys[j].TaskID
	})
	return keys
}

func cloneEntries(entries map[journalKey]Entry) map[journalKey]Entry {
	cloned := make(map[journalKey]Entry, len(entries))
	for key, entry := range entries {
		cloned[key] = cloneEntry(entry)
	}
	return cloned
}

func cloneEntry(entry Entry) Entry {
	entry.Input = cloneMap(entry.Input)
	entry.Data = cloneMap(entry.Data)
	return entry
}

func cloneMap(src map[string]interface{}) map[string]interface{} {
	if src == nil {
		return nil
	}
	dst := make(map[string]interface{}, len(src))
	for key, value := range src {
		dst[key] = cloneValue(value)
	}
	return dst
}

func cloneSlice(src []interface{}) []interface{} {
	if src == nil {
		return nil
	}
	dst := make([]interface{}, len(src))
	for i, value := range src {
		dst[i] = cloneValue(value)
	}
	return dst
}

func cloneValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		return cloneMap(typed)
	case []interface{}:
		return cloneSlice(typed)
	default:
		return typed
	}
}

func normalizeMap(src map[string]interface{}) (map[string]interface{}, error) {
	if src == nil {
		return nil, nil
	}
	data, err := json.Marshal(src)
	if err != nil {
		return nil, err
	}
	var normalized map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}
