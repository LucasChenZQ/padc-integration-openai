# OpenAI Responses Integration

本目录是可以独立审核和编译的 OpenAI API Integration 源码项目。`declaration.json` 使用 PADC 的 `IntegrationDefinition` 格式，声明 `openai.responses` Module 和 `openai.responses.create` 协议。`implementation_key` 是静态实现身份，尚未与 PADC Provider、账号实例、认证绑定或 Action 执行链连接。

源码仅依赖 Go 标准库。`go.mod` 和 `dependency-lock.json` 固定 Go 1.26.4，并显式声明空第三方 module 图。审核来源仓库根目录必须包含本目录的全部文件，固定真实 commit；不能把整个 PADC 主仓库作为此项目的来源归档。

## 独立验证

在本目录执行：

```sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -mod=readonly ./...
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go build -mod=readonly ./...
```

要求实际 Go 工具链为 1.26.4。测试只连接 `httptest` 本机服务，覆盖请求编码、Bearer 认证、输出读取、输入校验、取消、错误、重定向拒绝、单次提交及响应尺寸限制。测试不需要 API key，不调用真实 OpenAI 服务，也不证明某个模型或账号可用。官方认证服务另在固定隔离镜像中关闭网络编译源码。

## HTTP 实现

`NewClient(apiKey, Config)` 仅构造客户端。由服务器调用方从秘密设施取得凭据并注入，不将真实凭据写入声明、源码、日志或浏览器。可选 `Organization` 和 `Project` 对应 `OpenAI-Organization`、`OpenAI-Project` 头。

`CreateResponse(ctx, CreateRequest)` 在调用时向固定的 `https://api.openai.com/v1/responses` 发送一次 POST。调用方必须指定 `Model` 和非空 `Input`，可以指定 `Instructions` 和 `MaxOutputTokens`。请求固定 `store=false`、`stream=false`，不实现工具、流式、后台执行或自动重试。`store=false` 控制响应存储，不意味着服务端完全不保留任何数据。

客户端超时为 30 秒，拒绝重定向，最多读取 16 MiB 成功正文。`Response` 保留 ID、状态、输出和 `x-request-id`，`OutputText()` 合并助手消息的 `output_text`，不合并 refusal。调用方应检查 `Status`，不能把 HTTP 成功自动视为完整模型输出。HTTP 错误只返回状态与请求 ID，不保存可能回显输入或秘密的错误正文。传输失败、正文读取失败或成功正文格式损坏保留远端结果未知的含义，调用方不能据此自动重复 POST。

本项目不会自行读取环境变量、获取账号、列出模型或发起远端调用。真实 API 调用必须由独立调用方明确发起，使用自己获准使用的模型和服务器凭据。

## 官方依据

实现核对了 [OpenAI Responses create](https://developers.openai.com/api/reference/resources/responses/methods/create/) 的端点、输入与输出结构，以及 [OpenAI API overview](https://developers.openai.com/api/reference/overview/#authentication) 的 Bearer 认证、服务器秘密与组织／项目头要求。这里不设置默认模型，也不声称已通过真实账号调用验收。
