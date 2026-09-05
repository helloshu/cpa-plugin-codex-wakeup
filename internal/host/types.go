package host

import (
	"context"
	"encoding/json"
	"errors"
)

var ErrHostUnavailable = errors.New("host callback unavailable")

type Header map[string][]string

type HTTPRequest struct {
	Method  string `json:"method"`
	URL     string `json:"url"`
	Headers Header `json:"headers,omitempty"`
	Body    []byte `json:"body,omitempty"`
}

type HTTPResponse struct {
	StatusCode int    `json:"status_code"`
	Headers    Header `json:"headers,omitempty"`
	Body       []byte `json:"body,omitempty"`
}

type AuthFile struct {
	ID          string `json:"id,omitempty"`
	AuthIndex   string `json:"auth_index,omitempty"`
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Label       string `json:"label,omitempty"`
	Status      string `json:"status,omitempty"`
	StatusMsg   string `json:"status_message,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	Unavailable bool   `json:"unavailable,omitempty"`
	RuntimeOnly bool   `json:"runtime_only,omitempty"`
	Source      string `json:"source,omitempty"`
	Path        string `json:"path,omitempty"`
	Email       string `json:"email,omitempty"`
	Account     string `json:"account,omitempty"`
	AccountType string `json:"account_type,omitempty"`
	Priority    int    `json:"priority,omitempty"`
	// RawJSON is populated only by host.auth.get. It is never serialized back
	// to management responses and must not be logged.
	RawJSON []byte `json:"-"`
}

type Client interface {
	ListAuthFiles(context.Context) ([]AuthFile, error)
	GetAuthFile(context.Context, string) (AuthFile, error)
	HTTPDo(context.Context, HTTPRequest) (HTTPResponse, error)
	Log(context.Context, string, string, map[string]any)
}

func DecodeJSON[T any](raw json.RawMessage) (T, error) {
	var result T
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	return result, nil
}
