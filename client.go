// Package openai 提供独立的 OpenAI Responses HTTP 实现，不注册 PADC Provider。
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// endpoint 固定官方服务，避免凭据被发送到调用方指定的其他地址。
const endpoint = "https://api.openai.com/v1/responses"

// maxResponseBytes 限制远端正文在内存中的最大尺寸。
const maxResponseBytes = 16 << 20

// Config 包含非秘密的可选组织和项目选择。
type Config struct {
	Organization string `json:"organization,omitempty"`
	Project      string `json:"project,omitempty"`
}

// Client 保存服务器注入的凭据和有界 HTTP 客户端，构造时不访问网络。
type Client struct {
	credential string
	config     Config
	http       *http.Client
	endpoint   string
}

// CreateRequest 包含明确的模型和文本输入，不支持流式、后台或工具调用。
type CreateRequest struct {
	Model           string `json:"model"`
	Input           string `json:"input"`
	Instructions    string `json:"instructions,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
}

// wireRequest 明确关闭响应存储和流式传输，不由调用方覆盖。
type wireRequest struct {
	CreateRequest
	Store  bool `json:"store"`
	Stream bool `json:"stream"`
}

// Response 保留响应状态与输出，未知的 API 字段不影响读取。
type Response struct {
	ID        string       `json:"id"`
	Status    string       `json:"status"`
	Model     string       `json:"model,omitempty"`
	Output    []OutputItem `json:"output"`
	RequestID string       `json:"request_id,omitempty"`
}

// OutputItem 表示 Responses 输出中的消息项，其他项的 type 仍可被识别。
type OutputItem struct {
	Type    string    `json:"type"`
	Role    string    `json:"role,omitempty"`
	Content []Content `json:"content,omitempty"`
}

// Content 区分普通文本与拒绝消息，不把拒绝文本当成正常答案。
type Content struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

// APIError 只提供 HTTP 状态和关联 ID，不保留可能回显凭据或输入的远端正文。
type APIError struct {
	StatusCode int
	RequestID  string
}

// Error 返回可记录的安全诊断，不包含远端错误消息。
func (e *APIError) Error() string {
	return fmt.Sprintf("OpenAI API returned HTTP %d", e.StatusCode)
}

// NewClient 检查凭据与头字段，使用官方地址且拒绝所有重定向。
func NewClient(apiKey string, config Config) (*Client, error) {
	if apiKey == "" || !validHeader(apiKey) || strings.ContainsAny(apiKey, " \t") {
		return nil, errors.New("valid server credential required")
	}
	if !validHeader(config.Organization) || !validHeader(config.Project) {
		return nil, errors.New("invalid organization or project header")
	}
	// 重定向会改变提交目标；单次 POST 不自动跟随或重试。
	return &Client{credential: apiKey, config: config, endpoint: endpoint, http: &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// String 避免常用格式化输出暴露 Client 内的秘密。
func (c Client) String() string { return "OpenAI Responses Client [credential redacted]" }

// GoString 避免 %#v 格式输出暴露 Client 内的秘密。
func (c Client) GoString() string { return c.String() }

// CreateResponse 只在调用时发送一次 POST，未知网络结果由调用方显式处理。
func (c *Client) CreateResponse(ctx context.Context, input CreateRequest) (Response, error) {
	if c == nil || c.http == nil {
		return Response{}, errors.New("initialized client required")
	}
	if strings.TrimSpace(input.Model) == "" || strings.TrimSpace(input.Input) == "" || input.MaxOutputTokens < 0 {
		return Response{}, errors.New("model, input and valid token limit required")
	}
	// 编码固定的非流式请求，并将秘密仅放入认证头。
	body, err := json.Marshal(wireRequest{CreateRequest: input})
	if err != nil {
		return Response{}, errors.New("cannot encode response request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, errors.New("cannot create response request")
	}
	req.Header.Set("Authorization", "Bearer "+c.credential)
	req.Header.Set("Content-Type", "application/json")
	if c.config.Organization != "" {
		req.Header.Set("OpenAI-Organization", c.config.Organization)
	}
	if c.config.Project != "" {
		req.Header.Set("OpenAI-Project", c.config.Project)
	}
	// Do 不进行应用层重试，网络故障不代表远端未产生副作用。
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, errors.New("OpenAI request transport failed; remote result unknown")
	}
	defer resp.Body.Close()
	requestID := resp.Header.Get("x-request-id")
	if !validHeader(requestID) || strings.Contains(requestID, c.credential) {
		requestID = ""
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, &APIError{StatusCode: resp.StatusCode, RequestID: requestID}
	}
	// 先限制正文，再检查单个 JSON 对象，避免接受超限或尾随材料。
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return Response{}, errors.New("OpenAI response unreadable or too large; remote result unknown")
	}
	var out Response
	if err := json.Unmarshal(data, &out); err != nil || out.ID == "" || out.Status == "" || out.Output == nil {
		return Response{}, errors.New("invalid OpenAI response; remote result unknown")
	}
	out.RequestID = requestID
	return out, nil
}

// OutputText 按响应顺序合并助手消息中的 output_text，保留其他输出供调用方检查。
func (r Response) OutputText() string {
	var text strings.Builder
	for _, item := range r.Output {
		if item.Type != "message" || item.Role != "assistant" {
			continue
		}
		for _, content := range item.Content {
			if content.Type == "output_text" {
				text.WriteString(content.Text)
			}
		}
	}
	return text.String()
}

// validHeader 拒绝控制字符和非 ASCII 字节，避免头注入和隐式编码。
func validHeader(value string) bool {
	for i := range len(value) {
		if value[i] < 32 || value[i] > 126 {
			return false
		}
	}
	return len(value) <= 4096
}
