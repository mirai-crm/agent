package privatpos

import (
	"bytes"
	"encoding/json"
	"maps"
)

const (
	methodPingDevice  = "PingDevice"
	methodPurchase    = "Purchase"
	methodService     = "ServiceMessage"
	serviceIdentify   = "identify"
	paramMsgType      = "msgType"
	paramResult       = "result"
	paramCode         = "code"
	paramResponseCode = "responseCode"
)

// Request is one protocol request envelope.
type Request struct {
	Method string            `json:"method"`
	Step   int               `json:"step"`
	Params map[string]string `json:"params,omitempty"`
}

// Response is one protocol response envelope. Unknown top-level fields are
// preserved in Extra, and unknown params survive in Params.
type Response struct {
	Method           string         `json:"method"`
	Step             int            `json:"step"`
	Params           map[string]any `json:"params,omitempty"`
	Error            bool           `json:"error"`
	ErrorDescription string         `json:"errorDescription,omitempty"`
	Extra            map[string]any `json:"-"`
	raw              map[string]any
}

// Map returns the complete response envelope, including unknown top-level fields.
func (r Response) Map() map[string]any {
	if r.raw != nil {
		return cloneMap(r.raw)
	}
	out := map[string]any{
		"method":           r.Method,
		"step":             r.Step,
		"params":           r.Params,
		"error":            r.Error,
		"errorDescription": r.ErrorDescription,
	}
	maps.Copy(out, r.Extra)
	return cloneMap(out)
}

func (r *Response) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	type alias Response
	var parsed struct {
		Method           string         `json:"method"`
		Step             int            `json:"step"`
		Params           map[string]any `json:"params"`
		Error            bool           `json:"error"`
		ErrorDescription string         `json:"errorDescription"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}

	r.Method = parsed.Method
	r.Step = parsed.Step
	r.Params = parsed.Params
	r.Error = parsed.Error
	r.ErrorDescription = parsed.ErrorDescription

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&r.raw); err != nil {
		return err
	}

	extra := make(map[string]any)
	for key, value := range raw {
		switch key {
		case "method", "step", "params", "error", "errorDescription":
			continue
		}
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			return err
		}
		extra[key] = decoded
	}
	if len(extra) != 0 {
		r.Extra = extra
	}
	return nil
}

func cloneMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = cloneValue(value)
	}
	return dst
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		dst := make([]any, len(typed))
		for i, item := range typed {
			dst[i] = cloneValue(item)
		}
		return dst
	default:
		return value
	}
}

func handshakeRequest() Request {
	return Request{
		Method: methodPingDevice,
		Step:   0,
	}
}

func identifyRequest() Request {
	return Request{
		Method: methodService,
		Step:   0,
		Params: map[string]string{
			paramMsgType: serviceIdentify,
		},
	}
}

func purchaseRequest(amount, merchantID string) Request {
	return Request{
		Method: methodPurchase,
		Step:   0,
		Params: map[string]string{
			"amount":      amount,
			"discount":    "",
			"merchantId":  merchantID,
			"facepay":     "false",
			"subMerchant": "",
		},
	}
}
