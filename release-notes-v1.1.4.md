# GLM Vision Bridge v1.1.4

## 修复

- 修正管理页面读取配置、模型目录和事件日志时的 `fetch` 请求结构，确保 CPA Management Key 通过标准 `headers` 字段发送，不再因浏览器忽略无效的顶层 `Authorization` 字段而返回 401。
- 配置保存仍自动复用 CPA 管理中心的同源登录凭据，无需再次输入管理密码。

## 安全边界

- `/v0/resource/plugins/glm-vision-bridge/open` 继续只返回静态 HTML/JS。
- 动态配置、模型目录、事件读取和配置保存继续仅通过受认证的 `/v0/management/...` 接口完成。

## 验证

- 管理页面读取请求必须使用 `fetch(url, {headers})` 的回归测试。
- `go test ./...`
- `go vet ./...`
- `go test -race ./...`
