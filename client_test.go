package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// testClient 仅在同包测试中切换本机端点，公开构造器不允许自定义 URL。
func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient("test-credential", Config{Organization: "org-test", Project: "proj-test"})
	if err != nil {
		t.Fatal(err)
	}
	client.endpoint = server.URL + "/v1/responses"
	return client
}

// TestCreateResponse 验证认证头、固定关闭存储与流式以及实际 REST 输出形状。
func TestCreateResponse(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer test-credential" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("OpenAI-Organization") != "org-test" || r.Header.Get("OpenAI-Project") != "proj-test" {
			t.Error("unexpected method, endpoint or authentication headers")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "caller-model" || body["input"] != "hello" || body["instructions"] != "answer briefly" || body["max_output_tokens"] != float64(50) || body["store"] != false || body["stream"] != false {
			t.Errorf("unexpected request: %v", body)
		}
		w.Header().Set("x-request-id", "req-test")
		fmt.Fprint(w, `{"id":"resp-test","status":"completed","model":"caller-model","output":[{"type":"reasoning"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"},{"type":"refusal","refusal":"ignored"},{"type":"output_text","text":" world"}]}],"unknown_future_field":true}`)
	})
	out, err := client.CreateResponse(context.Background(), CreateRequest{Model: "caller-model", Input: "hello", Instructions: "answer briefly", MaxOutputTokens: 50})
	if err != nil || out.ID != "resp-test" || out.RequestID != "req-test" || out.OutputText() != "Hello world" {
		t.Fatalf("unexpected response: %#v, %v", out, err)
	}
}

// TestHTTPErrorAndRedirectNeverRetry 验证拒绝、限流和重定向只提交一次且不暴露远端正文。
func TestHTTPErrorAndRedirectNeverRetry(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusTemporaryRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var count atomic.Int32
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				w.Header().Set("Location", "/leaked")
				w.Header().Set("x-request-id", "req-error")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"message":"test-credential private-input"}}`)
			})
			_, err := client.CreateResponse(context.Background(), CreateRequest{Model: "caller-model", Input: "private-input"})
			var apiError *APIError
			if !errors.As(err, &apiError) || apiError.StatusCode != status || apiError.RequestID != "req-error" || count.Load() != 1 || strings.Contains(err.Error(), "test-credential") || strings.Contains(err.Error(), "private-input") {
				t.Fatalf("unexpected error or repeated submission: %v, %d", err, count.Load())
			}
		})
	}
}

// TestInvalidAndBoundedResponse 验证无效 JSON、无身份输出和超限正文被拒绝。
func TestInvalidAndBoundedResponse(t *testing.T) {
	for _, body := range []string{`{`, `{"id":"r","status":"completed"}`, `{"id":"r","status":"completed","output":[]} {}`, strings.Repeat("x", maxResponseBytes+1)} {
		client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		if _, err := client.CreateResponse(context.Background(), CreateRequest{Model: "caller-model", Input: "hello"}); err == nil {
			t.Fatal("invalid response accepted")
		}
	}
}

// TestInputAndCredentialValidation 验证空输入和头注入在发送请求前被拒绝。
func TestInputAndCredentialValidation(t *testing.T) {
	for _, key := range []string{"", "bad\nkey", "bad key"} {
		if _, err := NewClient(key, Config{}); err == nil {
			t.Fatal("invalid credential accepted")
		}
	}
	if _, err := NewClient("test-credential", Config{Project: "bad\r\nheader"}); err == nil {
		t.Fatal("header injection accepted")
	}
	var count atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { count.Add(1) })
	for _, input := range []CreateRequest{{Input: "hello"}, {Model: "model"}, {Model: "model", Input: "hello", MaxOutputTokens: -1}} {
		if _, err := client.CreateResponse(context.Background(), input); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.CreateResponse(ctx, CreateRequest{Model: "model", Input: "hello"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if count.Load() != 0 {
		t.Fatal("invalid request reached server")
	}
	if client.endpoint == endpoint {
		t.Fatal("test endpoint not injected")
	}
	production, err := NewClient("test-credential", Config{})
	if err != nil || production.endpoint != endpoint || strings.Contains(fmt.Sprintf("%v %+v %#v %v %+v %#v", production, production, production, *production, *production, *production), "test-credential") {
		t.Fatal("production endpoint or credential formatting incorrect")
	}
}

// TestCredentialEchoInRequestID 验证远端头字段中的秘密回显不会成为可记录诊断。
func TestCredentialEchoInRequestID(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "req-test-credential")
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := client.CreateResponse(context.Background(), CreateRequest{Model: "model", Input: "hello"})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.RequestID != "" || strings.Contains(fmt.Sprintf("%#v", apiError), "test-credential") {
		t.Fatal("credential exposed in diagnostic")
	}
}
