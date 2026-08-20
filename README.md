# GLM Vision Bridge

让纯文本大模型安全处理图片的 CLIProxyAPI 原生插件。

GLM Vision Bridge 将图片交给独立视觉模型转写，再把受控的视觉文本交给主文本模型完成推理和回答。客户端始终使用同一个公开模型，不需要感知内部路由。

> 1.1.4 使用独立的插件 ID、文件名和配置结构。已在 CLIProxyAPI v7.2.119、Linux amd64 环境完成文本、图片、多轮、流式及管理页面安全边界验证。


---

> 🏅 本项目已链接认可 [LINUX DO](https://linux.do/t/topic/2707846) 社区

---

## 特性

- 支持 OpenAI Chat、OpenAI Responses 和 Claude Messages。
- 纯文本请求直接进入主文本模型，不调用视觉模型。
- 最多四个视觉候选顺序回退；每个候选可独立设置上下文、预算、超时和启停状态。
- 视觉子请求固定 low 思考，不继承客户端工具或系统指令，也不设置输出 token 上限。
- 图片转写以 `gateway-generated / untrusted` 上下文注入，防止图片文字改变系统规则或触发工具。
- 处理后彻底移除原图，并限制重复看图工具；PDF 和未知媒体结构失败关闭。
- 并行工具返回的多张图片按同一任务批次处理；历史轮次复用有上限的持久视觉记忆，避免重复识图和上下文污染。
- 长会话在完整语义轮次与工具事务边界建立可复用摘要检查点。
- 最终输出预算按客户端协议显式下发，避免 Host 对缺失字段使用过低默认值。
- 内置中文管理页，可查看实际路由、配置和请求阶段事件。

## 工作原理

```text
客户端 → glm-vision-bridge
             │
             ├─ 纯文本 ───────────────────────────┐
             │                                     │
             └─ 图片 → 视觉模型链 → untrusted 文本 │
                                                   ▼
                                      主文本模型 → 文本备用链
                                                   ▼
                                                最终回答
```

视觉模型只提取图片事实，不回答完整任务。最终回答始终由 `primary_model` 或其文本备用链生成。

## 安装

### 使用发布包

1. 下载 Linux amd64 发布包并解压。
2. 将 `glm-vision-bridge.so` 放入 CPA 持久目录：

   ```text
   plugins/linux/amd64/glm-vision-bridge.so
   ```

3. 将 [`config.example.yaml`](config.example.yaml) 中的 `glm-vision-bridge` 配置块合并到 CPA `config.yaml`。
4. 按你的 CPA 模型名修改文本链与视觉链。
5. 重启 CLIProxyAPI。

如果机器上装过旧插件，请先移除旧共享库和旧配置块。1.0 不读取旧字段，也不提供迁移逻辑。

### 从源码构建

需要 Go 1.26、C 编译器和 `zip`：

```bash
make check
make package-linux-amd64
```

产物位于 `dist/`：

```text
glm-vision-bridge.so
glm-vision-bridge_1.0.0_linux_amd64.zip
checksums.txt
```

## 配置

完整示例见 [`config.example.yaml`](config.example.yaml)。最重要的字段如下：

```yaml
plugins:
  enabled: true
  configs:
    glm-vision-bridge:
      enabled: true
      priority: 100
      public_model: glm-vision-bridge

      primary_model: glm-5.2
      primary_context_tokens: 1000000
      primary_context_budget_tokens: 900000
      primary_output_token_limit: 64000
      text_fallback_models: [gpt-5.5, gpt-5.6-sol]

      vision_models:
        - model: gemini-3.1-flash-lite
          context_limit: 262144
          context_budget: 180000
          timeout_seconds: 20
          enabled: true

      on_vision_failure: error
```

约束：

- `public_model` 不能出现在文本链或视觉链中。
- 文本链与视觉链不能使用相同模型。
- 每个视觉模型的 `context_budget` 必须低于 `context_limit`。
- `primary_context_budget_tokens + primary_output_token_limit` 不能超过 `primary_context_tokens`。
- `on_vision_failure: error` 会在全部视觉候选失败时终止请求，避免主模型猜图；`text_only` 会继续处理文字部分。

### 输出预算

插件只修改请求顶层的输出字段，不会改动工具 Schema 中的同名属性：

| 客户端协议 | 下发字段 |
| --- | --- |
| OpenAI Chat | `max_tokens` |
| OpenAI Responses | `max_output_tokens` |
| Claude Messages | `max_tokens` |

客户端提供较小值时保留；缺失时注入 `primary_output_token_limit`；过大时封顶。

### 历史与缓存

- `history_attachment_mode: onDemand`：已识别旧图优先注入有长度上限的持久视觉记忆；未命中记忆时使用固定归档标记，只有明确要求重新查看时才恢复最近旧图。
- “图片已读取成功/识别完成”等完成确认不会触发恢复；“重新识别这张图”等明确要求会绕过缓存执行针对性重分析。
- `auto_compression_*`：长会话达到阈值后建立摘要检查点，不拆分工具调用与结果。
- `cache_path`：持久化视觉文本和历史摘要；缓存不保存原始图片。

## 验证

重启后确认日志包含：

```text
GLM Vision Bridge version=1.1.4
```

然后检查模型列表：

```bash
curl http://127.0.0.1:8317/v1/models \
  -H "Authorization: Bearer $CPA_API_KEY"
```

列表中应出现 `public_model`。建议依次验证：

1. 纯文本非流式请求。
2. 纯文本流式长输出，确认超过 1024 tokens 后仍自然结束。
3. 首次图片识别。
4. 同图缓存命中。
5. 多轮对话中的无关续问与明确追问旧图。

## 边界

- 当前仅处理图片，不解析 PDF、音频或视频。
- CPA Host 只有在视觉流建立后才向插件提供可取消的 `stream_id`；首包前完全卡住的请求无法由插件精确中止。
- OCR 结果可能有误，最终回答应保留必要的不确定性。
- 管理事件仅保存在内存中，重启后清空。

## 开发

```bash
go test ./...
go vet ./...
go test -race ./...
```

核心模块按职责拆分：协议适配、图片变换、视觉请求、文本流、历史压缩、缓存和管理页可以独立测试。第三方许可证保留在 `vendor/` 中。

## License

[MIT](LICENSE)
