package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cpa_buffer;

typedef int (*cpa_host_call_fn)(void*, const char*, const uint8_t*, size_t, cpa_buffer*);
typedef void (*cpa_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cpa_host_call_fn call;
	cpa_host_free_fn free_buffer;
} cpa_host_api;

static int cpa_host_call(void* raw, const char* method, const uint8_t* request,
			size_t request_len, cpa_buffer* response) {
	if (raw == NULL) return -1;
	cpa_host_api* api = (cpa_host_api*)raw;
	if (api->call == NULL) return -2;
	return api->call(api->host_ctx, method, request, request_len, response);
}

static void cpa_host_free(void* raw, void* ptr, size_t len) {
	if (raw == NULL || ptr == NULL) return;
	cpa_host_api* api = (cpa_host_api*)raw;
	if (api->free_buffer != NULL) api->free_buffer(ptr, len);
}
*/
import "C"

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"unsafe"

	"github.com/helloshu/cpa-plugin-codex-wakeup/internal/host"
)

type hostBridge struct {
	mu  sync.RWMutex
	ptr unsafe.Pointer
}

func closeHostClient(client host.Client) {
	if bridge, ok := client.(*hostBridge); ok {
		bridge.close()
	}
}

func newHostBridge(ptr unsafe.Pointer) *hostBridge {
	return &hostBridge{ptr: ptr}
}

func (b *hostBridge) close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.ptr = nil
	b.mu.Unlock()
}

func (b *hostBridge) call(ctx context.Context, method string, request any) ([]byte, error) {
	if b == nil {
		return nil, host.ErrHostUnavailable
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	rawRequest, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal host request: %w", err)
	}
	response, err := b.callRaw(method, rawRequest)
	if err != nil {
		return response, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	return response, nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (b *hostBridge) callRaw(method string, rawRequest []byte) ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.ptr == nil {
		return nil, host.ErrHostUnavailable
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var requestPtr *C.uint8_t
	var requestMem unsafe.Pointer
	if len(rawRequest) != 0 {
		requestMem = C.CBytes(rawRequest)
		defer C.free(requestMem)
		requestPtr = (*C.uint8_t)(requestMem)
	}
	var response C.cpa_buffer
	code := C.cpa_host_call(b.ptr, cMethod, requestPtr, C.size_t(len(rawRequest)), &response)
	var out []byte
	if response.ptr != nil && response.len != 0 {
		out = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.cpa_host_free(b.ptr, response.ptr, response.len)
	}
	if code != 0 {
		return out, fmt.Errorf("host callback %s returned %d", method, int(code))
	}
	return out, nil
}

func (b *hostBridge) ListAuthFiles(ctx context.Context) ([]host.AuthFile, error) {
	raw, err := b.call(ctx, methodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	raw, err = unwrapCallbackResult(raw)
	if err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	return decodeAuthList(raw)
}

func (b *hostBridge) GetAuthFile(ctx context.Context, authIndex string) (host.AuthFile, error) {
	raw, err := b.call(ctx, methodHostAuthGet, map[string]string{"auth_index": authIndex})
	if err != nil {
		return host.AuthFile{}, err
	}
	raw, err = unwrapCallbackResult(raw)
	if err != nil {
		return host.AuthFile{}, err
	}
	if err := contextError(ctx); err != nil {
		return host.AuthFile{}, err
	}
	return decodeAuthFileResponse(raw, authIndex)
}

func (b *hostBridge) HTTPDo(ctx context.Context, request host.HTTPRequest) (host.HTTPResponse, error) {
	raw, err := b.call(ctx, methodHostHTTPDo, request)
	if err != nil {
		return host.HTTPResponse{}, err
	}
	raw, err = unwrapCallbackResult(raw)
	if err != nil {
		return host.HTTPResponse{}, err
	}
	if err := contextError(ctx); err != nil {
		return host.HTTPResponse{}, err
	}
	response, err := decodeHTTPResponse(raw)
	if err != nil {
		return host.HTTPResponse{}, err
	}
	if err := contextError(ctx); err != nil {
		return host.HTTPResponse{}, err
	}
	return response, nil
}

func (b *hostBridge) Log(ctx context.Context, level, message string, fields map[string]any) {
	if b == nil {
		return
	}
	cleanFields := make(map[string]any, len(fields))
	for key, value := range fields {
		cleanFields[key] = sanitizeValue(value)
	}
	_, _ = b.call(ctx, methodHostLog, map[string]any{
		"level":   level,
		"message": sanitizeText(message),
		"fields":  cleanFields,
	})
}

func unwrapCallbackResult(raw []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, errors.New("host callback returned an empty response")
	}
	var envelope struct {
		OK     *bool           `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.OK != nil {
		if !*envelope.OK {
			return nil, fmt.Errorf("host callback error: %s", truncateAndScrub(envelope.Error, 240))
		}
		if len(envelope.Result) != 0 && string(envelope.Result) != "null" {
			return envelope.Result, nil
		}
		return []byte("{}"), nil
	}
	return raw, nil
}

func decodeAuthList(raw []byte) ([]host.AuthFile, error) {
	var direct []host.AuthFile
	if json.Unmarshal(raw, &direct) == nil {
		return direct, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", err)
	}
	for _, key := range []string{"auth_files", "files", "items", "accounts", "data"} {
		if value, ok := object[key]; ok {
			if result, err := decodeAuthList(value); err == nil {
				return result, nil
			}
		}
	}
	return nil, errors.New("decode host.auth.list: expected an array")
}

func decodeAuthFileResponse(raw []byte, authIndex string) (host.AuthFile, error) {
	var wrapper struct {
		AuthIndex string          `json:"auth_index"`
		Name      string          `json:"name"`
		Path      string          `json:"path"`
		JSON      json.RawMessage `json:"json"`
		Auth      json.RawMessage `json:"auth"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return host.AuthFile{}, fmt.Errorf("decode host.auth.get: %w", err)
	}
	if len(wrapper.JSON) == 0 || string(wrapper.JSON) == "null" {
		wrapper.JSON = wrapper.Auth
	}
	if len(wrapper.JSON) == 0 || string(wrapper.JSON) == "null" {
		// Some host versions return the physical JSON directly.
		wrapper.JSON = raw
	}
	material, err := decodeJSONBytes(wrapper.JSON)
	if err != nil {
		return host.AuthFile{}, fmt.Errorf("decode host.auth.get json: %w", err)
	}
	var result host.AuthFile
	if err = json.Unmarshal(material, &result); err != nil {
		return host.AuthFile{}, fmt.Errorf("decode auth material: %w", err)
	}
	if result.AuthIndex == "" {
		result.AuthIndex = firstNonEmpty(wrapper.AuthIndex, authIndex)
	}
	if result.Name == "" {
		result.Name = wrapper.Name
	}
	result.RawJSON = append([]byte(nil), material...)
	return result, nil
}

func decodeHTTPResponse(raw []byte) (host.HTTPResponse, error) {
	fields, err := rawObject(raw)
	if err != nil {
		return host.HTTPResponse{}, fmt.Errorf("decode host.http.do: %w", err)
	}
	statusRaw := field(fields, "status_code", "statuscode", "status")
	var status int
	if len(statusRaw) != 0 {
		if err = json.Unmarshal(statusRaw, &status); err != nil {
			return host.HTTPResponse{}, fmt.Errorf("decode host.http.do status: %w", err)
		}
	}
	body, err := decodeJSONBytes(field(fields, "body"))
	if err != nil {
		return host.HTTPResponse{}, fmt.Errorf("decode host.http.do body: %w", err)
	}
	headers, err := decodeHeaders(field(fields, "headers"))
	if err != nil {
		return host.HTTPResponse{}, fmt.Errorf("decode host.http.do headers: %w", err)
	}
	return host.HTTPResponse{StatusCode: status, Headers: headers, Body: body}, nil
}

func rawObject(raw []byte) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	return object, nil
}

func field(fields map[string]json.RawMessage, names ...string) json.RawMessage {
	for key, value := range fields {
		normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
		for _, wanted := range names {
			if normalized == strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(wanted, "_", ""), "-", "")) {
				return value
			}
		}
	}
	return nil
}

func decodeJSONBytes(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if decoded, err := base64.StdEncoding.DecodeString(text); err == nil {
			return decoded, nil
		}
		return []byte(text), nil
	}
	var bytesValue []byte
	if json.Unmarshal(raw, &bytesValue) == nil {
		return bytesValue, nil
	}
	return append([]byte(nil), raw...), nil
}

func decodeHeaders(raw json.RawMessage) (host.Header, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	result := make(host.Header, len(values))
	for key, value := range values {
		var list []string
		if json.Unmarshal(value, &list) == nil {
			result[key] = list
			continue
		}
		var item string
		if err := json.Unmarshal(value, &item); err != nil {
			return nil, err
		}
		result[key] = []string{item}
	}
	return result, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func asHTTPHeader(value host.Header) http.Header {
	result := make(http.Header, len(value))
	for key, values := range value {
		result[key] = append([]string(nil), values...)
	}
	return result
}
