// Package main implements a small, self-contained CLIProxyAPI plugin that
// periodically sends a deliberately tiny Codex Responses request for selected
// OAuth accounts. It uses only the native plugin ABI and host callbacks, so it
// does not need a second listener or access to CPA's private files.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"
)

const (
	pluginName    = "codex-wakeup"
	pluginVersion = "0.1.8"

	abiVersion = uint32(1)
	// Schema 6 preserves management JSON strings. The UI renders dynamic
	// content as text, so it does not need the host's legacy HTML entities.
	pluginMaxSchema = uint32(6)
	// schemaVersion is retained as the public/max schema diagnostic value.
	schemaVersion = pluginMaxSchema

	methodPluginRegister     = "plugin.register"
	methodPluginReconfigure  = "plugin.reconfigure"
	methodPluginQuiesce      = "plugin.quiesce"
	methodPluginShutdown     = "plugin.shutdown"
	methodManagementRegister = "management.register"
	methodManagementHandle   = "management.handle"
	methodHostAuthList       = "host.auth.list"
	methodHostAuthGet        = "host.auth.get"
	methodHostHTTPDo         = "host.http.do"
	methodHostLog            = "host.log"
)

var pluginInstance = newPluginRuntime()

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(rawHost *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil || rawHost == nil {
		return 1
	}
	pluginInstance.installHost(newHostBridge(unsafe.Pointer(rawHost)))
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := pluginInstance.handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", scrubError(err)))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	pluginInstance.shutdown()
}

type rpcEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rpcEnvelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(rpcEnvelope{OK: false, Error: &rpcError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type metadata struct {
	// These keys intentionally use the exact default encoding/json names used
	// by CLIProxyAPI's sdk/pluginapi.Metadata. The host decodes this nested
	// object into that type, whose fields have no json tags.
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo,omitempty"`
	ConfigFields     []configField `json:"ConfigFields,omitempty"`
}

type configField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description,omitempty"`
}

type capabilities struct {
	ManagementAPI bool `json:"management_api"`
}

func pluginRegistration() registration {
	return pluginRegistrationForSchema(pluginMaxSchema)
}

func pluginRegistrationForSchema(hostSchema uint32) registration {
	negotiatedSchema := hostSchema
	if negotiatedSchema == 0 {
		// Hosts that omit schema_version use the original schema-1 contract.
		negotiatedSchema = 1
	}
	if negotiatedSchema > pluginMaxSchema {
		negotiatedSchema = pluginMaxSchema
	}
	return registration{
		SchemaVersion: negotiatedSchema,
		Metadata: metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "helloshu",
			GitHubRepository: "https://github.com/helloshu/cpa-plugin-codex-wakeup",
			ConfigFields: []configField{
				{Name: "enabled", Type: "boolean", Description: "启用插件；宿主配置项。"},
				{Name: "auto_wake", Type: "boolean", Description: "启用后台定时唤醒。"},
				{Name: "scan_interval", Type: "string", Description: "调度扫描间隔，例如 30s。"},
				{Name: "state_file", Type: "string", Description: "相对于 CPA 工作目录的状态文件路径。"},
				{Name: "history_limit", Type: "integer", Description: "保留的执行历史条数。"},
				{Name: "request_timeout", Type: "string", Description: "单个账号请求超时，例如 60s。"},
				{Name: "default_model", Type: "string", Description: "默认 Codex 模型。"},
				{Name: "default_prompt", Type: "string", Description: "默认唤醒提示词。"},
				{Name: "default_max_output_tokens", Type: "integer", Description: "兼容保留；ChatGPT 官方直连不发送该字段，实际输出长度由后端决定。"},
				{Name: "run_on_start", Type: "boolean", Description: "插件启动时是否立即执行未运行任务。"},
			},
		},
		Capabilities: capabilities{ManagementAPI: true},
	}
}

type registrationRequest struct {
	SchemaVersion uint32          `json:"schema_version"`
	ConfigYAML    json.RawMessage `json:"config_yaml"`
	Config        json.RawMessage `json:"config"`
}

func requestedSchemaVersion(raw []byte) uint32 {
	var request registrationRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		// configure() reports malformed lifecycle requests. Keep this helper
		// fail-closed and never emit schema 0 if it is called independently.
		return 1
	}
	if request.SchemaVersion == 0 {
		return 1
	}
	if request.SchemaVersion > pluginMaxSchema {
		return pluginMaxSchema
	}
	return request.SchemaVersion
}

func (p *pluginRuntime) handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		if err := p.configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistrationForSchema(requestedSchemaVersion(request)))
	case methodPluginQuiesce:
		p.quiesce()
		return okEnvelope(map[string]any{})
	case methodPluginShutdown:
		p.shutdown()
		return okEnvelope(map[string]any{})
	case methodManagementRegister:
		return okEnvelope(managementRegistration())
	case methodManagementHandle:
		return p.handleManagementRPC(request)
	default:
		return errorEnvelope("unknown_method", fmt.Sprintf("unknown method: %s", method)), nil
	}
}
