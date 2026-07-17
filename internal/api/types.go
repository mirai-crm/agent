// Package api defines the CRM device API contract and an HTTP client for it.
package api

import (
	"encoding/json"
	"time"
)

// Task names — mirror of DEVICE_TASK_NAMES on the server (single source of truth).
const (
	TaskPrintCheck   = "print_check"
	TaskPrintZReport = "print_z_report"
	TaskPurchase     = "purchase"
)

// DeviceType for receipt printers (only supported type in current scope).
const (
	DeviceTypeReceiptPrinter = "receipt_printer"
	DeviceTypePOSTerminal    = "pos_terminal"
)

// DeviceInfo is the public device data returned by GET /devices/info.
// It intentionally never contains secretToken.
type DeviceInfo struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	Type           string     `json:"type"`
	RegisteredAt   time.Time  `json:"registeredAt"`
	LastTaskAt     *time.Time `json:"lastTaskAt"`
	ProcessedTasks int        `json:"processedTasks"`
	LastPingAt     *time.Time `json:"lastPingAt"`
	QueuedTasks    int        `json:"queuedTasks"`
}

type infoResponse struct {
	Device DeviceInfo `json:"device"`
}

// Task is an element of the GET /tasks response array.
type Task struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Data      json.RawMessage `json:"data"`
	Priority  int             `json:"priority"`
	CreatedAt time.Time       `json:"createdAt"`
}

// TasksResponse is the body of GET /tasks.
type TasksResponse struct {
	Tasks []Task `json:"tasks"`
}

// PrintCheckData is the payload of a print_check task.
type PrintCheckData struct {
	CheckID int64 `json:"checkId"`
}

// PrintZReportData is the payload of a print_z_report task.
type PrintZReportData struct {
	ZReportID int64 `json:"zReportId"`
}

// PurchaseData is the payload of a purchase task.
type PurchaseData struct {
	AmountMinor int64  `json:"amountMinor"`
	TIN         string `json:"tin"`
}

// FinalizeItem is one task to finalize. Success = no ErrorMessage.
type FinalizeItem struct {
	ID           int64                  `json:"id"`
	Data         map[string]interface{} `json:"data,omitempty"`
	ErrorMessage string                 `json:"error_message,omitempty"`
}

// FinalizeRequest is the body of POST /tasks/finalize.
type FinalizeRequest struct {
	Tasks []FinalizeItem `json:"tasks"`
}

// FinalizeResponse is the response of POST /tasks/finalize.
type FinalizeResponse struct {
	Finalized []int64 `json:"finalized"`
	Skipped   []int64 `json:"skipped"`
}

// PingResponse is the response of POST /ping.
type PingResponse struct {
	OK         bool      `json:"ok"`
	ServerTime time.Time `json:"serverTime"`
}
