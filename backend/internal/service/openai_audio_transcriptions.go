package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

const OpenAIAudioTranscriptionMaxBodySize = 26 << 20
const audioTranscriptionMaxFileSize = 25 << 20

// OpenAIAudioTranscriptionRequest 不携带客户端身份字段，转录不参与推理会话收敛。
type OpenAIAudioTranscriptionRequest struct {
	Model, Language, Prompt, ResponseFormat string
	FileName, FileContentType, ContentType  string
	Audio, Body                             []byte
	DurationSeconds                         float64
	PayloadHash                             string
	ModelProvided                           bool
}

type AudioTranscriptionRequestError struct {
	Status  int
	Message string
}

func (e *AudioTranscriptionRequestError) Error() string { return e.Message }
func audioRequestError(status int, msg string) error {
	return &AudioTranscriptionRequestError{Status: status, Message: msg}
}

// ParseOpenAIAudioTranscriptionRequest 严格拒绝重复和截断字段，避免鉴权模型与实际上游模型不同。
func ParseOpenAIAudioTranscriptionRequest(contentType string, body []byte, desktop bool) (*OpenAIAudioTranscriptionRequest, error) {
	if len(body) > OpenAIAudioTranscriptionMaxBodySize {
		return nil, audioRequestError(http.StatusRequestEntityTooLarge, "audio upload exceeds 26 MiB")
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, audioRequestError(http.StatusBadRequest, "multipart/form-data with boundary is required")
	}
	p := &OpenAIAudioTranscriptionRequest{Body: body, ContentType: contentType}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	seen := map[string]bool{}
	for count := 0; ; count++ {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil || count >= 16 {
			return nil, audioRequestError(400, "invalid multipart body")
		}
		name := part.FormName()
		if seen[name] {
			return nil, audioRequestError(400, "duplicate multipart field")
		}
		seen[name] = true
		limit := int64(64 << 10)
		if name == "file" {
			limit = audioTranscriptionMaxFileSize
		}
		data, err := io.ReadAll(io.LimitReader(part, limit+1))
		if err != nil {
			return nil, audioRequestError(400, "invalid multipart field")
		}
		if int64(len(data)) > limit {
			return nil, audioRequestError(413, "multipart field exceeds limit")
		}
		if name == "file" {
			p.Audio, p.FileName, p.FileContentType = data, part.FileName(), part.Header.Get("Content-Type")
			if strings.ContainsAny(p.FileName+p.FileContentType, "\r\n\x00") {
				return nil, audioRequestError(400, "invalid file metadata")
			}
			continue
		}
		if part.FileName() != "" {
			return nil, audioRequestError(400, "unexpected file field")
		}
		v := strings.TrimSpace(string(data))
		switch name {
		case "model":
			p.Model = v
			p.ModelProvided = true
		case "language":
			p.Language = v
		case "prompt":
			p.Prompt = v
		case "response_format":
			p.ResponseFormat = v
		case "stream":
			enabled, err := strconv.ParseBool(v)
			if err != nil || enabled {
				return nil, audioRequestError(400, "only non-streaming transcription is supported")
			}
		default:
			return nil, audioRequestError(400, "unsupported transcription field")
		}
	}
	if len(p.Audio) == 0 {
		return nil, audioRequestError(400, "file is required and must not be empty")
	}
	if desktop && !p.ModelProvided {
		p.Model = "gpt-transcribe"
	}
	if p.Model == "" || len(p.Model) > 256 {
		return nil, audioRequestError(400, "model is required (max 256 bytes)")
	}
	if len(p.Language) > 32 {
		return nil, audioRequestError(400, "invalid language")
	}
	if p.ResponseFormat != "" && p.ResponseFormat != "json" && p.ResponseFormat != "text" {
		return nil, audioRequestError(400, "response_format must be json or text")
	}
	sum := sha256.Sum256(body)
	p.PayloadHash = hex.EncodeToString(sum[:])
	return p, nil
}

// upstreamBody 为 OAuth 只发送已知听写字段；API Key 使用原始表单并单点改写模型。
func (p *OpenAIAudioTranscriptionRequest) upstreamBody(oauth bool, model string) ([]byte, string, error) {
	if !oauth {
		// 桌面别名允许省略 model，重建表单以补齐标准 Audio API 的必填项。
		if p.Model == model && p.ModelProvided {
			return p.Body, p.ContentType, nil
		}
	}
	if oauth && p.Prompt != "" {
		return nil, "", audioRequestError(400, "prompt requires an OpenAI API-key transcription account")
	}
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	name := p.FileName
	if name == "" {
		name = "audio.wav"
	}
	fileType := p.FileContentType
	if _, _, err := mime.ParseMediaType(fileType); err != nil {
		fileType = "application/octet-stream"
	}
	file, err := w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": name})},
		"Content-Type":        {fileType},
	})
	if err != nil {
		return nil, "", err
	}
	if _, err = file.Write(p.Audio); err != nil {
		return nil, "", err
	}
	fields := map[string]string{"language": p.Language}
	if !oauth {
		fields["model"], fields["prompt"], fields["response_format"] = model, p.Prompt, p.ResponseFormat
	}
	for k, v := range fields {
		if v != "" {
			if err := w.WriteField(k, v); err != nil {
				return nil, "", err
			}
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return b.Bytes(), w.FormDataContentType(), nil
}

// audioTranscriptionValidText 拒绝 HTML/空包/非字符串 text，避免把前端页面回退当作成功转录。
func audioTranscriptionValidText(body []byte) (string, error) {
	var result struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Text == nil {
		return "", errors.New("upstream returned invalid transcription JSON")
	}
	return *result.Text, nil
}
