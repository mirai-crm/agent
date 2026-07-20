package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/mirai-agent/mirai-agent/internal/api"
	"github.com/mirai-agent/mirai-agent/internal/config"
	"github.com/mirai-agent/mirai-agent/internal/paymentjournal"
)

func TestManagerBeginDrainWaitsForActiveTask(t *testing.T) {
	manager := newTestManager(t)
	worker, client, executor := newTestWorker(manager, config.DeviceConfig{ID: 1, Name: "one"})
	executor.block = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan bool, 1)
	go func() {
		result <- worker.processTaskIfAllowed(ctx, testTask(1))
	}()

	call := <-executor.started
	if call.task.ID != 1 {
		t.Fatalf("started task = %d, want 1", call.task.ID)
	}

	drained := manager.BeginDrain()
	select {
	case <-drained:
		t.Fatal("drain completed while task still active")
	default:
	}
	if err := call.ctx.Err(); err != nil {
		t.Fatalf("active task context cancelled after BeginDrain(): %v", err)
	}

	close(executor.block)

	if ok := <-result; !ok {
		t.Fatal("processTaskIfAllowed() = false, want true")
	}
	<-drained
	if got := client.finalizedIDs(); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("finalized task ids = %v, want [1]", got)
	}
}

func TestManagerBeginDrainDeniesAdmissionAfterDrain(t *testing.T) {
	manager := newTestManager(t)
	worker, client, executor := newTestWorker(manager, config.DeviceConfig{ID: 2, Name: "two"})

	drained := manager.BeginDrain()
	if !manager.IsDraining() {
		t.Fatal("IsDraining() = false, want true")
	}
	<-drained

	if ok := worker.processTaskIfAllowed(context.Background(), testTask(2)); ok {
		t.Fatal("processTaskIfAllowed() = true after drain, want false")
	}
	select {
	case call := <-executor.started:
		t.Fatalf("unexpected task start: %d", call.task.ID)
	default:
	}
	if got := client.finalizedIDs(); len(got) != 0 {
		t.Fatalf("finalized task ids = %v, want none", got)
	}
}

func TestManagerBeginDrainWaitsForAdmittedPollAndDropsReturnedBatch(t *testing.T) {
	manager := newTestManager(t)
	worker, client, executor := newTestWorker(manager, config.DeviceConfig{ID: 3, Name: "three"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.pollLoop(ctx)
	}()

	<-client.pollStarted
	drained := manager.BeginDrain()
	select {
	case <-drained:
		t.Fatal("drain completed while admitted poll still active")
	default:
	}
	client.pollResults <- pollResult{tasks: []api.Task{testTask(31), testTask(32)}}

	<-done
	<-drained

	select {
	case call := <-executor.started:
		t.Fatalf("unexpected task start after drain: %d", call.task.ID)
	default:
	}
	if got := client.finalizedIDs(); len(got) != 0 {
		t.Fatalf("finalized task ids = %v, want none", got)
	}
}

func TestManagerBeginDrainPreventsNewPollAdmission(t *testing.T) {
	manager := newTestManager(t)
	worker, client, _ := newTestWorker(manager, config.DeviceConfig{ID: 30, Name: "thirty"})

	<-manager.BeginDrain()

	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.pollLoop(context.Background())
	}()
	<-done

	select {
	case <-client.pollStarted:
		t.Fatal("PollTasks() started after drain won admission")
	default:
	}
}

func TestManagerBeginDrainLeavesRemainingBatchTasksPending(t *testing.T) {
	manager := newTestManager(t)
	worker, client, executor := newTestWorker(manager, config.DeviceConfig{ID: 4, Name: "four"})
	executor.block = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.pollLoop(ctx)
	}()

	client.pollResults <- pollResult{tasks: []api.Task{testTask(41), testTask(42)}}

	call := <-executor.started
	if call.task.ID != 41 {
		t.Fatalf("started task = %d, want 41", call.task.ID)
	}

	drained := manager.BeginDrain()
	select {
	case <-drained:
		t.Fatal("drain completed while first batch task still active")
	default:
	}

	close(executor.block)

	<-done
	<-drained

	select {
	case extra := <-executor.started:
		t.Fatalf("unexpected second task start: %d", extra.task.ID)
	default:
	}
	if got := client.finalizedIDs(); !reflect.DeepEqual(got, []int64{41}) {
		t.Fatalf("finalized task ids = %v, want [41]", got)
	}
}

func TestManagerBeginDrainWaitsForMultipleWorkers(t *testing.T) {
	manager := newTestManager(t)
	workerOne, _, execOne := newTestWorker(manager, config.DeviceConfig{ID: 5, Name: "five"})
	workerTwo, _, execTwo := newTestWorker(manager, config.DeviceConfig{ID: 6, Name: "six"})
	execOne.block = make(chan struct{})
	execTwo.block = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make(chan bool, 2)
	go func() { results <- workerOne.processTaskIfAllowed(ctx, testTask(51)) }()
	go func() { results <- workerTwo.processTaskIfAllowed(ctx, testTask(61)) }()

	<-execOne.started
	<-execTwo.started

	drained := manager.BeginDrain()
	select {
	case <-drained:
		t.Fatal("drain completed while multiple tasks still active")
	default:
	}

	close(execOne.block)
	if ok := <-results; !ok {
		t.Fatal("first worker admission failed unexpectedly")
	}
	select {
	case <-drained:
		t.Fatal("drain completed before second worker finished")
	default:
	}

	close(execTwo.block)
	if ok := <-results; !ok {
		t.Fatal("second worker admission failed unexpectedly")
	}
	<-drained
}

func TestManagerBeginDrainIsIdempotent(t *testing.T) {
	manager := newTestManager(t)

	first := manager.BeginDrain()
	second := manager.BeginDrain()
	if first != second {
		t.Fatal("BeginDrain() returned different channels")
	}
	if !manager.IsDraining() {
		t.Fatal("IsDraining() = false, want true")
	}

	<-first
	<-second
}

func TestManagerBeginDrainWaitsForActiveReplay(t *testing.T) {
	manager := newTestManager(t)
	worker, client, _ := newTestWorker(manager, config.DeviceConfig{ID: 81, Name: "POS"})
	worker.isPOS = true
	journal := &stubReplayJournal{
		entries: []paymentjournal.Entry{{
			DeviceID: 81,
			TaskID:   1001,
			Data:     map[string]any{"payment": map[string]any{"status": "approved"}},
		}},
	}
	worker.journal = journal
	client.finalizeStarted = make(chan struct{}, 1)
	client.finalizeRelease = make(chan struct{})

	ctx := context.Background()
	replayed := make(chan bool, 1)
	go func() {
		replayed <- worker.replayIfAllowed(ctx)
	}()

	<-client.finalizeStarted
	drained := manager.BeginDrain()
	select {
	case <-drained:
		t.Fatal("drain completed while replay finalization still active")
	default:
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("replay context cancelled after BeginDrain(): %v", err)
	}

	close(client.finalizeRelease)
	if ok := <-replayed; !ok {
		t.Fatal("replayIfAllowed() = false, want true")
	}
	<-drained

	if got := journal.removedIDs(); !reflect.DeepEqual(got, []int64{1001}) {
		t.Fatalf("removed journal task ids = %v, want [1001]", got)
	}
}

func TestManagerBeginDrainSkipsReplayBeforeAdmission(t *testing.T) {
	manager := newTestManager(t)
	worker, client, _ := newTestWorker(manager, config.DeviceConfig{ID: 82, Name: "POS"})
	worker.isPOS = true
	journal := &stubReplayJournal{
		entries: []paymentjournal.Entry{{
			DeviceID: 82,
			TaskID:   1002,
			Data:     map[string]any{"payment": map[string]any{"status": "approved"}},
		}},
	}
	worker.journal = journal

	<-manager.BeginDrain()

	if ok := worker.replayIfAllowed(context.Background()); !ok {
		t.Fatal("replayIfAllowed() = false when drain skipped replay")
	}
	if journal.completeCalls != 0 || journal.removeCalls != 0 {
		t.Fatalf("journal mutations after drain = complete %d, remove %d", journal.completeCalls, journal.removeCalls)
	}
	if got := client.finalizedIDs(); len(got) != 0 {
		t.Fatalf("finalized task ids = %v, want none", got)
	}
}

func TestManagerBeginDrainDoesNotRewritePaymentJournalFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	journalPath := configPath + ".payments.json"
	initial := []byte(`{"entries":[{"deviceId":81,"taskId":1001,"input":{"amountMinor":12345,"tin":"1111111111"},"requestSent":true,"data":{"amountMinor":12345,"tin":"1111111111","payment":{"status":"approved"}}}]}`)
	if err := os.WriteFile(journalPath, initial, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg := config.Default()
	cfg.Devices = []config.DeviceConfig{{
		Token: "pos-token",
		ID:    81,
		Name:  "POS",
		Type:  api.DeviceTypePOSTerminal,
		POS: config.POSConfig{
			Address:                 "127.0.0.1:2000",
			ConnectTimeoutSeconds:   5,
			OperationTimeoutSeconds: 180,
			MerchantIDs:             map[string]string{"1111111111": "1"},
		},
	}}

	manager, err := NewManager(cfg, configPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	<-manager.BeginDrain()

	got, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != string(initial) {
		t.Fatalf("payment journal changed:\n got: %s\nwant: %s", got, initial)
	}
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := NewManager(config.Default(), filepath.Join(t.TempDir(), "config.toml"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	return manager
}

func newTestWorker(manager *Manager, dev config.DeviceConfig) (*deviceWorker, *stubWorkerClient, *stubWorkerExecutor) {
	cfg := config.Default()
	cfg.Retry.MaxAttempts = 1

	client := &stubWorkerClient{
		pollStarted: make(chan struct{}, 10),
		pollResults: make(chan pollResult, 10),
	}
	executor := &stubWorkerExecutor{
		started: make(chan executeCall, 10),
	}
	return &deviceWorker{
		cfg:      cfg,
		dev:      dev,
		client:   client,
		executor: executor,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		gate:     manager.gate,
	}, client, executor
}

func testTask(id int64) api.Task {
	data, _ := json.Marshal(map[string]any{"ok": true})
	return api.Task{ID: id, Name: api.TaskPrintCheck, Data: data}
}

type pollResult struct {
	tasks []api.Task
	err   error
}

type stubWorkerClient struct {
	pollStarted chan struct{}
	pollResults chan pollResult

	finalizeStarted chan struct{}
	finalizeRelease chan struct{}

	mu        sync.Mutex
	finalized []int64
}

func (c *stubWorkerClient) PollTasks(ctx context.Context, _ int, _ int) ([]api.Task, error) {
	select {
	case c.pollStarted <- struct{}{}:
	default:
	}
	select {
	case res := <-c.pollResults:
		return res.tasks, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *stubWorkerClient) Finalize(_ context.Context, items []api.FinalizeItem) (api.FinalizeResponse, error) {
	if c.finalizeStarted != nil {
		c.finalizeStarted <- struct{}{}
	}
	if c.finalizeRelease != nil {
		<-c.finalizeRelease
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	resp := api.FinalizeResponse{Finalized: make([]int64, 0, len(items))}
	for _, item := range items {
		c.finalized = append(c.finalized, item.ID)
		resp.Finalized = append(resp.Finalized, item.ID)
	}
	return resp, nil
}

func (c *stubWorkerClient) Ping(context.Context) (api.PingResponse, error) {
	return api.PingResponse{}, nil
}

func (c *stubWorkerClient) FetchPNG(context.Context, string, int, int) ([]byte, error) {
	return nil, nil
}

func (c *stubWorkerClient) finalizedIDs() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.finalized...)
}

type executeCall struct {
	ctx  context.Context
	task api.Task
}

type stubWorkerExecutor struct {
	started chan executeCall
	block   chan struct{}
}

func (e *stubWorkerExecutor) Execute(ctx context.Context, task api.Task) (map[string]interface{}, error) {
	e.started <- executeCall{ctx: ctx, task: task}
	if e.block != nil {
		<-e.block
	}
	return nil, nil
}

func (e *stubWorkerExecutor) Close() error { return nil }

type stubReplayJournal struct {
	mu sync.Mutex

	entries       []paymentjournal.Entry
	completeCalls int
	removeCalls   int
	removed       []int64
}

func (j *stubReplayJournal) Complete(_ int64, _ int64, _ map[string]interface{}) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.completeCalls++
	return nil
}

func (j *stubReplayJournal) Pending(_ int64) []paymentjournal.Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]paymentjournal.Entry(nil), j.entries...)
}

func (j *stubReplayJournal) Remove(_ int64, taskID int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.removeCalls++
	j.removed = append(j.removed, taskID)
	return nil
}

func (j *stubReplayJournal) removedIDs() []int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]int64(nil), j.removed...)
}
