package liveattestation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrAttestationClientRequestFailed marks a valid JSON-RPC error response from
// the client. It is distinct from malformed JSON/params so callers can emit
// the protocol's s=2 request-failed envelope instead of s=4.
var ErrAttestationClientRequestFailed = errors.New("client attestation generation failed")

// JSONRPCMessage 是 app-server 使用的最小 JSON-RPC 形状。
// Codex app-server 的线协议沿用 JSON-RPC 的对象形状，但按官方协议
// 不发送也不要求 jsonrpc 字段；为兼容测试工具和旧客户端这里仍接受
// 可选的 "2.0"。
type JSONRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// InitializeCapabilities 是 Codex app-server 的能力协商子集。
type InitializeCapabilities struct {
	RequestAttestation bool `json:"requestAttestation"`
}

// AttestationGenerateResponse 是客户端对 attestation/generate 的响应。
type AttestationGenerateResponse struct {
	Token string `json:"token"`
}

// ParseInitializeRequest 判断一条 JSON-RPC initialize 是否声明 requestAttestation。
// 缺少 capabilities 或 requestAttestation 时返回 false；类型错误返回错误，避免
// 把异常报文当成已完成协商。
func ParseInitializeRequest(raw []byte) (bool, error) {
	var message JSONRPCMessage
	if err := decodeJSONObject(raw, &message); err != nil {
		return false, err
	}
	if message.JSONRPC != "" && message.JSONRPC != "2.0" {
		return false, errors.New("unsupported JSON-RPC version")
	}
	if message.Method != "initialize" {
		return false, nil
	}
	if len(message.Params) == 0 || bytes.Equal(bytes.TrimSpace(message.Params), []byte("null")) {
		return false, nil
	}
	var params struct {
		Capabilities *InitializeCapabilities `json:"capabilities"`
	}
	if err := json.Unmarshal(message.Params, &params); err != nil {
		return false, fmt.Errorf("decode initialize params: %w", err)
	}
	if params.Capabilities == nil {
		return false, nil
	}
	return params.Capabilities.RequestAttestation, nil
}

// BuildAttestationGenerateRequest 构造服务端发给客户端的请求。
// requestID 仅用于匹配响应，不承载账号或会话信息。
func BuildAttestationGenerateRequest(requestID uint64) ([]byte, error) {
	if requestID == 0 {
		return nil, errors.New("attestation request id must be positive")
	}
	message := struct {
		ID     uint64   `json:"id"`
		Method string   `json:"method"`
		Params struct{} `json:"params"`
	}{ID: requestID, Method: "attestation/generate"}
	return json.Marshal(message)
}

// ParseAttestationGenerateResponse 提取客户端 token。调用方应先校验 pending
// request ID，再把 token 交给 NormalizeClientEnvelope 封装。
func ParseAttestationGenerateResponse(raw []byte) (json.RawMessage, string, error) {
	var message JSONRPCMessage
	if err := decodeJSONObject(raw, &message); err != nil {
		return nil, "", err
	}
	if message.JSONRPC != "" && message.JSONRPC != "2.0" {
		return nil, "", errors.New("unsupported JSON-RPC version")
	}
	if len(message.ID) == 0 || bytes.Equal(bytes.TrimSpace(message.ID), []byte("null")) {
		return nil, "", errors.New("attestation response is missing id")
	}
	if len(message.Error) > 0 && !bytes.Equal(bytes.TrimSpace(message.Error), []byte("null")) {
		return append(json.RawMessage(nil), message.ID...), "", fmt.Errorf("%w: JSON-RPC error returned", ErrAttestationClientRequestFailed)
	}
	if len(message.Result) == 0 || bytes.Equal(bytes.TrimSpace(message.Result), []byte("null")) {
		return append(json.RawMessage(nil), message.ID...), "", errors.New("attestation response is missing result")
	}
	var result AttestationGenerateResponse
	if err := json.Unmarshal(message.Result, &result); err != nil {
		return append(json.RawMessage(nil), message.ID...), "", fmt.Errorf("decode attestation result: %w", err)
	}
	if result.Token == "" {
		return append(json.RawMessage(nil), message.ID...), "", errors.New("attestation response token is empty")
	}
	if len(result.Token) > maxClientAttestationTokenBytes {
		return append(json.RawMessage(nil), message.ID...), "", errors.New("attestation response token exceeds size limit")
	}
	return append(json.RawMessage(nil), message.ID...), result.Token, nil
}

func decodeJSONObject(raw []byte, target any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("JSON-RPC message must be an object")
	}
	if err := json.Unmarshal(trimmed, target); err != nil {
		return fmt.Errorf("decode JSON-RPC message: %w", err)
	}
	return nil
}
