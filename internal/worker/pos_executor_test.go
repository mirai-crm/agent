package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mirai-agent/mirai-agent/internal/api"
	"github.com/mirai-agent/mirai-agent/internal/paymentjournal"
	"github.com/mirai-agent/mirai-agent/internal/privatpos"
)

func TestPOSExecutorResolvesMerchantIDByTIN(t *testing.T) {
	journal := &stubPaymentJournal{}
	client := &stubPOSClient{}
	executor := newPOSExecutor(7, "token", map[string]string{"1111111111": "3"}, journal, client)

	data, err := executor.Execute(context.Background(), purchaseTask(42, 12345, "1111111111"))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if client.merchantID != "3" {
		t.Fatalf("Purchase() merchantID = %q, want %q", client.merchantID, "3")
	}
	if data["tin"] != "1111111111" {
		t.Fatalf("finalize tin = %#v, want %q", data["tin"], "1111111111")
	}
	if _, ok := data["merchantId"]; ok {
		t.Fatal("finalize data unexpectedly contains merchantId")
	}
	if journal.input["tin"] != "1111111111" {
		t.Fatalf("journal tin = %#v, want %q", journal.input["tin"], "1111111111")
	}
}

func TestPOSExecutorRejectsUnboundTINBeforePurchase(t *testing.T) {
	journal := &stubPaymentJournal{}
	client := &stubPOSClient{}
	executor := newPOSExecutor(7, "token", map[string]string{"1111111111": "3"}, journal, client)

	_, err := executor.Execute(context.Background(), purchaseTask(42, 12345, "9999999999"))
	if err == nil || !strings.Contains(err.Error(), `no merchantId binding for tin "9999999999"`) {
		t.Fatalf("Execute() error = %v", err)
	}
	if client.called {
		t.Fatal("Purchase() called for unbound tin")
	}
	if journal.began {
		t.Fatal("journal entry created for unbound tin")
	}
}

func purchaseTask(id, amountMinor int64, tin string) api.Task {
	data, _ := json.Marshal(map[string]any{"amountMinor": amountMinor, "tin": tin})
	return api.Task{ID: id, Name: api.TaskPurchase, Data: data}
}

type stubPOSClient struct {
	called     bool
	merchantID string
}

func (c *stubPOSClient) Purchase(_ context.Context, _ string, merchantID string, beforeSend func() error) (privatpos.PurchaseOutcome, error) {
	c.called = true
	c.merchantID = merchantID
	if err := beforeSend(); err != nil {
		return privatpos.PurchaseOutcome{}, err
	}
	return privatpos.PurchaseOutcome{
		Response: &privatpos.Response{
			Method: "Purchase",
			Params: map[string]any{"responseCode": "0000"},
		},
		RequestSent: true,
		Stage:       privatpos.StageCompleted,
	}, nil
}

func (c *stubPOSClient) Close() error { return nil }

type stubPaymentJournal struct {
	began bool
	input map[string]any
	data  map[string]any
}

func (j *stubPaymentJournal) Begin(_, _ int64, input map[string]any) error {
	j.began = true
	j.input = input
	return nil
}

func (j *stubPaymentJournal) MarkSent(_, _ int64) error { return nil }

func (j *stubPaymentJournal) Complete(_, _ int64, data map[string]any) error {
	j.data = data
	return nil
}

func (j *stubPaymentJournal) Get(_, _ int64) (paymentjournal.Entry, bool) {
	return paymentjournal.Entry{}, false
}
