package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/mirai-agent/mirai-agent/internal/api"
	"github.com/mirai-agent/mirai-agent/internal/paymentjournal"
	"github.com/mirai-agent/mirai-agent/internal/privatpos"
)

type posClient interface {
	Purchase(context.Context, string, string, func() error) (privatpos.PurchaseOutcome, error)
	Close() error
}

type paymentJournal interface {
	Begin(deviceID, taskID int64, input map[string]interface{}) error
	MarkSent(deviceID, taskID int64) error
	Complete(deviceID, taskID int64, data map[string]interface{}) error
	Get(deviceID, taskID int64) (paymentjournal.Entry, bool)
}

type posExecutor struct {
	deviceID int64
	token    string
	journal  paymentJournal
	client   posClient
}

func newPOSExecutor(deviceID int64, token string, journal paymentJournal, client posClient) *posExecutor {
	return &posExecutor{deviceID: deviceID, token: token, journal: journal, client: client}
}

func (e *posExecutor) Close() error { return e.client.Close() }

func (e *posExecutor) Execute(ctx context.Context, task api.Task) (map[string]interface{}, error) {
	if task.Name != api.TaskPurchase {
		return nil, permanent(fmt.Errorf("unsupported task name %q", task.Name))
	}
	var input api.PurchaseData
	decoder := json.NewDecoder(bytes.NewReader(task.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return nil, permanent(fmt.Errorf("bad purchase data: %w", err))
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, permanent(errors.New("bad purchase data: trailing JSON value"))
	}
	if input.AmountMinor <= 0 {
		return nil, permanent(errors.New("purchase: amountMinor must be positive"))
	}
	if input.MerchantID == "" {
		input.MerchantID = "0"
	}

	if entry, ok := e.journal.Get(e.deviceID, task.ID); ok {
		if entry.Data != nil {
			if err := e.journal.Complete(e.deviceID, task.ID, entry.Data); err != nil {
				return nil, paymentPersistenceError(err)
			}
			return entry.Data, nil
		}
		data := unknownPayment(input, entry.RequestSent, "journal_incomplete", "payment intent was incomplete")
		if err := e.journal.Complete(e.deviceID, task.ID, data); err != nil {
			return nil, paymentPersistenceError(err)
		}
		return data, nil
	}
	if err := e.journal.Begin(e.deviceID, task.ID, map[string]interface{}{
		"amountMinor": input.AmountMinor,
		"merchantId":  input.MerchantID,
	}); err != nil {
		return nil, paymentPersistenceError(err)
	}

	outcome, err := e.client.Purchase(ctx, formatAmount(input.AmountMinor), input.MerchantID, func() error {
		return e.journal.MarkSent(e.deviceID, task.ID)
	})
	var data map[string]interface{}
	if outcome.Response != nil {
		status := "declined"
		switch responseCode(outcome.Response) {
		case "0000":
			status = "approved"
		case "0010":
			status = "partial"
		}
		data = paymentData(input, map[string]interface{}{
			"status":      status,
			"requestSent": outcome.RequestSent,
			"stage":       string(outcome.Stage),
			"response":    sanitizeResponse(outcome.Response.Map()),
		})
	} else {
		description := ""
		if err != nil {
			description = sanitizeError(err, e.token)
		}
		data = unknownPayment(input, outcome.RequestSent, string(outcome.Stage), description)
	}
	if completeErr := e.journal.Complete(e.deviceID, task.ID, data); completeErr != nil {
		return nil, paymentPersistenceError(completeErr)
	}
	return data, nil
}

type paymentPersistenceFailure struct{ err error }

func (e *paymentPersistenceFailure) Error() string { return "persist payment result: " + e.err.Error() }
func (e *paymentPersistenceFailure) Unwrap() error { return e.err }

func paymentPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	return &paymentPersistenceFailure{err: err}
}

func isPaymentPersistenceError(err error) bool {
	var target *paymentPersistenceFailure
	return errors.As(err, &target)
}

func formatAmount(amountMinor int64) string {
	return strconv.FormatInt(amountMinor/100, 10) + "." + fmt.Sprintf("%02d", amountMinor%100)
}

func responseCode(response *privatpos.Response) string {
	if response == nil {
		return ""
	}
	code, _ := response.Params["responseCode"].(string)
	return code
}

func unknownPayment(input api.PurchaseData, requestSent bool, stage, description string) map[string]interface{} {
	payment := map[string]interface{}{"status": "unknown", "requestSent": requestSent, "stage": stage}
	if description != "" {
		payment["errorDescription"] = description
	}
	return paymentData(input, payment)
}

func paymentData(input api.PurchaseData, payment map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"amountMinor": input.AmountMinor,
		"merchantId":  input.MerchantID,
		"payment":     payment,
	}
}

func sanitizeResponse(value map[string]interface{}) map[string]interface{} {
	return sanitizeMap(value)
}

func sanitizeMap(value map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(value))
	for key, item := range value {
		switch strings.ToLower(key) {
		case "track1", "cardholdername", "cardexpirydate":
			continue
		}
		out[key] = sanitizeValue(item)
	}
	return out
}

func sanitizeValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		return sanitizeMap(typed)
	case []interface{}:
		out := make([]interface{}, len(typed))
		for i, item := range typed {
			out[i] = sanitizeValue(item)
		}
		return out
	default:
		return value
	}
}
