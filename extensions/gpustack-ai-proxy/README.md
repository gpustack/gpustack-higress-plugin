---
title: AI 代理
keywords: [AI网关, AI代理]
description: AI 代理插件配置参考
---

> **Fork 说明**
>
> 本插件 fork 自 Higress `ai-proxy` 插件：
> https://github.com/alibaba/higress/tree/36f2f1ac5c7305c5a46c296c617b45d1fbacb7d3/plugins/wasm-go/extensions/ai-proxy
>
> 基于 higress commit `36f2f1ac5c73` 复制，并在 gpustack/gpustack-higress-plugins 中独立维护。
> 每个 `.go` 文件头部保留了对应的上游来源标注；本地改动可能与上游产生差异。
>
> **本地改动**：
>
> - 修复 Claude→OpenAI 协议转换丢弃 `tool_result` 内容里的图片块（见 gpustack/gpustack-higress-plugin#65）。
>   `tool` 消息的 `content` 在 OpenAI 协议里只有文本语义，因此 `tool` 消息保持文本 + `tool_call_id` 不变，
>   图片转成 `image_url` 放到该组 `tool` 消息**之后**的 `user` 消息里（带 `Image returned by tool result <id>:` 标注），
>   若紧随其后已有 `user` 消息则合并进去；工具调用与结果之间不会被插入 `user`。
> - 修复 Claude→OpenAI 协议转换把 `messages[]` 中的内联 `system` 消息提升到索引 0、
>   从而让动态提醒每轮改写稳定前缀并破坏 prefix cache 的问题。顶层 `system` 保持在前，
>   其余 `system` 的内容与 `cache_control` 会保留并降级/合入相邻 `user` 消息；工具调用与
>   工具结果之间不会被插入 `user`，同时满足严格 OpenAI 后端（如 vLLM）对消息位置和角色的要求。
>   注意：内联提醒不再具有顶层 `system` 的指令优先级（见 gpustack/gpustack#5934）。
> - 为 deepseek provider 的默认能力补充 `ApiNameResponses`，DeepSeek 原生提供 OpenAI Responses API。
> - 在 `capabilities` 白名单中放行三个 Anthropic 能力（`anthropic/v1/messages`、
>   `anthropic/v1/messages/count_tokens`、`anthropic/v1/complete`），使原生支持 Claude 协议的
>   provider 可以通过配置声明，而不必依赖 provider 类型硬编码。
> - 新增 `toolCallValidation`：在 Chat Completions 请求路径上做 tool/tool_calls 结构化配对校验，
>   默认关闭（见 gpustack/gpustack#6210）。
> - 新增 `requestPrioritySource` / `requestPriorityTarget`：把 `gpustack-ext-auth` 写入的调用方
>   推理请求优先级翻译成后端的调度输入——vLLM 写请求头 `x-vllm-priority = -p` 并写同样的请求体成员
>   （它的 pooling 端点只读请求体），SGLang 写请求体 `priority = p`，其余 provider 不处理（见 gpustack/gpustack#6739）。
> - 为 qwen provider 兼容模式（`qwenEnableCompatible: true`，GPUStack 默认）的默认能力补充
>   `cohere/v1/rerank` → `/compatible-api/v1/reranks`：入站 Cohere 风格的标准路径 `/v1/rerank`
>   （GPUStack UI / llama-box / vLLM 所用的拼写）此前会在 `isSupportedAPI` 处被拒
>   （`unsupported API name`）。DashScope 兼容模式 rerank 端点本身就是 Cohere/Jina 同构契约，
>   路径重写 + 模型映射即可完成转换，body 与响应原样透传。
> - 为 qwen provider 兼容模式的默认能力补充 `openai/v1/imagegeneration` →
>   `/compatible-mode/v1/images/generations`：qwen-image 系列（生成与编辑）提供标准 OpenAI Images
>   兼容端点，纯路径重写 + 模型映射即可。注意该端点仅部署在业务空间专属域名
>   （`{WorkspaceId}.cn-beijing.maas.aliyuncs.com`），旧域名 `dashscope.aliyuncs.com` 返回 404，
>   启用图像生成需将 `qwenDomain` 配置为业务空间域名。
>
> **已删除的上游文件**：本仓库的构建/发布流程不使用 Higress 官方 CLI `hgctl`，而是走
> `extensions/Makefile`（直接 `go build` 生成 wasm）+ `scripts/generate_metadata.py`。因此以下随
> 上游 fork 进来但在本仓库中不再被消费的文件已被移除：
>
> - `option.yaml` —— `hgctl` 插件工程配置（build/test/install 参数、配置结构体反射入口），本仓库不调用 hgctl。
> - `Makefile` —— 上游基于 tinygo 的单插件构建脚本，本仓库由 `extensions/Makefile` 统一构建。
> - `envoy.yaml` —— 上游 hgctl 本地测试用的 Envoy 配置。
> - `.gitignore` —— hgctl 生成的白名单式忽略规则（会误伤 `VERSION` 等文件），与本仓库根 `.gitignore` 冲突，已删除以对齐其它插件。
>
> `test/` 目录（上游 hgctl e2e 测试辅助代码）暂予保留，但本仓库的单元测试与构建不依赖它。

## 功能说明

`AI 代理`插件实现了基于 OpenAI API 契约的 AI 代理功能。目前支持 OpenAI、Azure OpenAI、月之暗面（Moonshot）和通义千问等 AI
服务提供商。

**🚀 自动协议兼容 (Auto Protocol Compatibility)**

插件现在支持**自动协议检测**，无需配置即可同时兼容 OpenAI 和 Claude 两种协议格式：

- **OpenAI 协议**: 请求路径 `/v1/chat/completions`，使用标准的 OpenAI Messages API 格式
- **Claude 协议**: 请求路径 `/v1/messages`，使用 Anthropic Claude Messages API 格式  
- **智能转换**: 自动检测请求协议，如果目标供应商不原生支持该协议，则自动进行协议转换
- **零配置**: 用户无需设置 `protocol` 字段，插件自动处理

> **协议支持说明：**

> 请求路径后缀匹配 `/v1/chat/completions` 时，对应文生文场景，会用 OpenAI 的文生文协议解析请求 Body，再转换为对应 LLM 厂商的文生文协议

> 请求路径后缀匹配 `/v1/messages` 时，对应 Claude 文生文场景，会自动检测供应商能力：如果支持原生 Claude 协议则直接转发，否则先转换为 OpenAI 协议再转发给供应商

> 请求路径后缀匹配 `/v1/embeddings` 时，对应文本向量场景，会用 OpenAI 的文本向量协议解析请求 Body，再转换为对应 LLM 厂商的文本向量协议

> 请求路径后缀匹配 `/v1/images/generations` 时，对应文生图场景，会用 OpenAI 的图片生成协议解析请求 Body，再转换为对应 LLM 厂商的图片生成协议

## 运行属性

插件执行阶段：`默认阶段`
插件执行优先级：`100`

## 配置字段

### 基本配置

| 名称       | 数据类型 | 填写要求 | 默认值 | 描述                         |
| ---------- | -------- | -------- | ------ | ---------------------------- |
| `provider` | object   | 必填     | -      | 配置目标 AI 服务提供商的信息 |
| `toolCallValidation` | string | 非必填 | `off` | Chat Completions 请求路径上的 `tool`/`tool_calls` 结构化配对校验。`off`（默认）：完全关闭；`strict`：校验失败返回 400。详见[tool/tool_calls 配对校验](#tooltool_calls-配对校验)。 |
| `requestPrioritySource` | string | 非必填 | `x-gpustack-request-priority` | 携带调用方推理请求优先级的请求头（`gpustack-ext-auth` 写入）：非负整数，越大越紧急，缺失按 0 处理。读取后即从发往上游的请求中移除；是否翻译、如何翻译由各 provider 的 `requestPriorityTarget` 决定。详见[推理请求优先级](#推理请求优先级)。 |

`provider`的配置字段说明如下：

| 名称                   | 数据类型               | 填写要求 | 默认值 | 描述                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| ---------------------- | ---------------------- | -------- | ------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------   |
| `type`                 | string                 | 必填     | -      | AI 服务提供商名称                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `apiTokens`            | array of string        | 非必填   | -      | 用于在访问 AI 服务时进行认证的令牌。如果配置了多个 token，插件会在请求时随机进行选择。部分服务提供商只支持配置一个 token。                                                                                                                                                                                                                                                                                                                 |
| `timeout`              | number                 | 非必填   | -      | 访问 AI 服务的超时时间。单位为毫秒。默认值为 120000，即 2 分钟。此项配置目前仅用于获取上下文信息，并不影响实际转发大模型请求。                                                                                                                                                                                                                                                                                                             |
| `modelMapping`         | map of string          | 非必填   | -      | AI 模型映射表，用于将请求中的模型名称映射为服务提供商支持模型名称。<br/>1. 支持前缀匹配。例如用 "gpt-3-\*" 匹配所有名称以“gpt-3-”开头的模型；<br/>2. 支持使用 "\*" 为键来配置通用兜底映射关系；<br/>3. 如果映射的目标名称为空字符串 ""，则表示保留原模型名称。<br/>4. 支持以 `~` 前缀使用正则匹配。例如用 "~gpt(.\*)" 匹配所有以 "gpt" 开头的模型并支持在目标模型中使用 capture group 引用匹配到的内容。示例: "~gpt(.\*): openai/gpt\$1" |
| `protocol`             | string                 | 非必填   | -      | 插件对外提供的 API 接口契约。目前支持以下取值：openai（默认值，使用 OpenAI 的接口契约）、original（使用目标服务提供商的原始接口契约）                                                                                                                                                                                                                                                                                                      |
| `context`              | object                 | 非必填   | -      | 配置 AI 对话上下文信息                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `customSettings`       | array of customSetting | 非必填   | -      | 为 AI 请求指定覆盖或者填充参数                                                                                                                                                                                                                                                                                                                                                                                                             |
| `failover`             | object                 | 非必填   | -      | 配置 apiToken 的 failover 策略，当 apiToken 不可用时，将其移出 apiToken 列表，待健康检测通过或冷却时间到期后重新添加回 apiToken 列表                                                                                                                                                                                                                                                                                                       |
| `retryOnFailure`       | object                 | 非必填   | -      | 当请求失败时立即进行重试                                                                                                                                                                                                                                                                                                                                                                                                                   |
| `requestPriorityTarget` | string | 非必填 | - | 该 provider 背后的后端如何接收调用方的推理请求优先级：`vllm`（写请求头 `x-vllm-priority = -p` 及请求体 `priority = -p`）、`sglang`（写请求体 `priority = p`）、不设（不处理）。GPUStack 按模型的后端类型渲染。详见[推理请求优先级](#推理请求优先级)。 |

| `reasoningContentMode` | string                 | 非必填   | -      | 如何处理大模型服务返回的推理内容。目前支持以下取值：passthrough（正常输出推理内容）、ignore（不输出推理内容）、concat（将推理内容拼接在常规输出内容之前）。默认为 passthrough。仅支持通义千问服务。                                                                                                                                                                                                                                        |
| `capabilities`         | map of string          | 非必填   | -      | 部分 provider 的部分 ai 能力原生兼容 openai/v1 格式，不需要重写，可以直接转发，通过此配置项指定来开启转发, key 表示的是采用的厂商协议能力，values 表示的真实的厂商该能力的 api path, 厂商协议能力当前支持: openai/v1/chatcompletions, openai/v1/embeddings, openai/v1/imagegeneration, openai/v1/audiospeech, cohere/v1/rerank                                                                                                             |
| `basePath`             | string                 | 非必填   | -      | 如果配置了 basePath，可用于在请求 path 中移除该前缀，或添加至请求 path 中，默认为进行移除                                                                                                                                                                                                                                                                                                                                                 |
| `basePathHandling`     | string                 | 非必填   | removePrefix | basePathHandling 用于指定 basePath 的处理方式。可选值：removePrefix（移除路径前缀，将请求转发给上游时去除 basePath 前缀后再拼接）、prepend（添加路径前缀，将请求转发给上游时在路径前面添加 basePath 前缀）                                                                                                                                                                                                                                                                                                                         |
| `contextCleanupCommands` | array of string      | 非必填   | -      | 上下文清理命令列表。当请求的 messages 中存在完全匹配任意一个命令的 user 消息时，将该消息及之前所有非 system 消息清理掉，只保留 system 消息和该命令之后的消息。可用于主动清理对话上下文。                                                                                                                                                                                                                                                    |

`context`的配置字段说明如下：

| 名称          | 数据类型 | 填写要求 | 默认值 | 描述                                                     |
| ------------- | -------- | -------- | ------ | -------------------------------------------------------- |
| `fileUrl`     | string   | 必填     | -      | 保存 AI 对话上下文的文件 URL。仅支持纯文本类型的文件内容 |
| `serviceName` | string   | 必填     | -      | URL 所对应的 Higress 后端服务完整名称                    |
| `servicePort` | number   | 必填     | -      | URL 所对应的 Higress 后端服务访问端口                    |

`customSettings`的配置字段说明如下：

| 名称        | 数据类型              | 填写要求 | 默认值 | 描述                                                                                                                         |
| ----------- | --------------------- | -------- | ------ | ---------------------------------------------------------------------------------------------------------------------------- |
| `name`      | string                | 必填     | -      | 想要设置的参数的名称，例如`max_tokens`                                                                                       |
| `value`     | string/int/float/bool | 必填     | -      | 想要设置的参数的值，例如 0                                                                                                   |
| `mode`      | string                | 非必填   | "auto" | 参数设置的模式，可以设置为"auto"或者"raw"，如果为"auto"则会自动根据协议对参数名做改写，如果为"raw"则不会有任何改写和限制检查 |
| `overwrite` | bool                  | 非必填   | true   | 如果为 false 则只在用户没有设置这个参数时填充参数，否则会直接覆盖用户原有的参数设置                                          |

custom-setting 会遵循如下表格，根据`name`和协议来替换对应的字段，用户需要填写表格中`settingName`列中存在的值。例如用户将`name`设置为`max_tokens`，在 openai 协议中会替换`max_tokens`，在 gemini 中会替换`maxOutputTokens`。
`none`表示该协议不支持此参数。如果`name`不在此表格中或者对应协议不支持此参数，同时没有设置 raw 模式，则配置不会生效。

| settingName | openai      | baidu             | spark       | qwen        | gemini          | hunyuan     | claude      | minimax            |
| ----------- | ----------- | ----------------- | ----------- | ----------- | --------------- | ----------- | ----------- | ------------------ |
| max_tokens  | max_tokens  | max_output_tokens | max_tokens  | max_tokens  | maxOutputTokens | none        | max_tokens  | tokens_to_generate |
| temperature | temperature | temperature       | temperature | temperature | temperature     | Temperature | temperature | temperature        |
| top_p       | top_p       | top_p             | none        | top_p       | topP            | TopP        | top_p       | top_p              |
| top_k       | none        | none              | top_k       | none        | topK            | none        | top_k       | none               |
| seed        | seed        | none              | none        | seed        | none            | none        | none        | none               |

如果启用了 raw 模式，custom-setting 会直接用输入的`name`和`value`去更改请求中的 json 内容，而不对参数名称做任何限制和修改。
对于大多数协议，custom-setting 都会在 json 内容的根路径修改或者填充参数。对于`qwen`协议，ai-proxy 会在 json 的`parameters`子路径下做配置。对于`gemini`协议，则会在`generation_config`子路径下做配置。

`failover` 的配置字段说明如下：

| 名称                | 数据类型        | 填写要求                                  | 默认值         | 描述                                                                 |
| ------------------- | --------------- | ----------------------------------------- | -------------- | -------------------------------------------------------------------- |
| enabled             | bool            | 非必填                                    | false          | 是否启用 apiToken 的 failover 机制                                   |
| failureThreshold    | int             | 非必填                                    | 3              | 触发 failover 连续请求失败的阈值（次数）                             |
| successThreshold    | int             | 非必填                                    | 1              | 健康检测的成功阈值（次数）                                           |
| healthCheckInterval | int             | 非必填                                    | 5000           | 健康检测的间隔时间，单位毫秒                                         |
| healthCheckTimeout  | int             | 非必填                                    | 5000           | 健康检测的超时时间，单位毫秒                                         |
| healthCheckModel    | string          | 启用 failover 时与 cooldownDuration 二选一 | -              | 健康检测使用的模型。配置后会通过健康检测恢复不可用的 apiToken        |
| cooldownDuration    | int             | 启用 failover 时与 healthCheckModel 二选一 | 0              | apiToken 不可用后的冷却恢复时间，单位毫秒。大于 0 时冷却到期自动恢复 |
| failoverOnStatus    | array of string | 非必填                                    | ["4.*", "5.*"] | 需要进行 failover 的原始请求的状态码，支持正则表达式匹配             |

`healthCheckModel` 和 `cooldownDuration` 至少需要配置一个。当两者同时配置时，apiToken 可通过健康检测提前恢复，也会在冷却时间到期后自动恢复。

`retryOnFailure` 的配置字段说明如下：

目前仅支持对非流式请求进行重试。

| 名称          | 数据类型        | 填写要求 | 默认值         | 描述                                               |
| ------------- | --------------- | -------- | -------------- | -------------------------------------------------- |
| enabled       | bool            | 非必填   | false          | 是否启用失败请求重试                               |
| maxRetries    | int             | 非必填   | 1              | 最大重试次数                                       |
| retryTimeout  | int             | 非必填   | 30000          | 重试超时时间，单位毫秒                             |
| retryOnStatus | array of string | 非必填   | ["4.*", "5.*"] | 需要进行重试的原始请求的状态码，支持正则表达式匹配 |

### 提供商特有配置

#### OpenAI

OpenAI 所对应的 `type` 为 `openai`。它特有的配置字段如下:

| 名称                 | 数据类型 | 填写要求 | 默认值 | 描述                                                                               |
| -------------------- | -------- | -------- | ------ | ---------------------------------------------------------------------------------- |
| `openaiCustomUrl`    | string   | 非必填   | -      | 基于 OpenAI 协议的自定义后端 URL，例如: <www.example.com/myai/v1/chat/completions> |
| `responseJsonSchema` | object   | 非必填   | -      | 预先定义 OpenAI 响应需满足的 Json Schema, 注意目前仅特定的几种模型支持该用法       |

#### Azure OpenAI

Azure OpenAI 所对应的 `type` 为 `azure`。它特有的配置字段如下：

| 名称              | 数据类型 | 填写要求 | 默认值 | 描述                                                                                                   |
| ----------------- | -------- | -------- | ------ | ------------------------------------------------------------------------------------------------------ |
| `azureServiceUrl` | string   | 必填     | -      | Azure OpenAI 服务的 URL。`/openai/v1` 新版路径无需日期型 `api-version`；legacy 路径或仅资源名称模式仍须包含。 |

**注意：**
1. Azure OpenAI 只支持配置一个 API Token。
2. `azureServiceUrl` 支持新版 `/openai/v1` 和 legacy 配置格式：
   1. 新版 v1 格式，例如：`https://YOUR_RESOURCE_NAME.openai.azure.com/openai/v1`
      - 插件会直接使用该 v1 base URL，且不会自动追加日期型 `api-version`。
   2. Legacy 完整路径格式，例如：`https://YOUR_RESOURCE_NAME.openai.azure.com/openai/deployments/YOUR_DEPLOYMENT_NAME/chat/completions?api-version=2024-02-15-preview`
      - 插件会直接将请求转发至该 URL，不会参考实际的请求路径。
   3. Legacy 部署名称格式，例如：`https://YOUR_RESOURCE_NAME.openai.azure.com/openai/deployments/YOUR_DEPLOYMENT_NAME?api-version=2024-02-15-preview`
      - 插件会根据实际的请求路径拼接后续路径。路径中的部署名称会保留不变，不会按照模型映射规则进行修改。同时支持 URL 中不包含部署名称的接口。
   4. Legacy 资源名称格式，例如：`https://YOUR_RESOURCE_NAME.openai.azure.com?api-version=2024-02-15-preview`
      - 插件会根据实际的请求路径拼接后续路径。路径中的部署名称会根据请求中的模型名称结合模型映射规则进行填入。同时支持 URL 中不包含部署名称的接口。

#### 月之暗面（Moonshot）

月之暗面所对应的 `type` 为 `moonshot`。它特有的配置字段如下：

| 名称             | 数据类型 | 填写要求 | 默认值 | 描述                                                                                                 |
| ---------------- | -------- | -------- | ------ | ---------------------------------------------------------------------------------------------------- |
| `moonshotFileId` | string   | 非必填   | -      | 通过文件接口上传至月之暗面的文件 ID，其内容将被用做 AI 对话的上下文。不可与 `context` 字段同时配置。 |

#### 通义千问（Qwen）

通义千问所对应的 `type` 为 `qwen`。它特有的配置字段如下：

| 名称                   | 数据类型        | 填写要求 | 默认值 | 描述                                                                                                    |
| ---------------------- | --------------- | -------- | ------ | ------------------------------------------------------------------------------------------------------- |
| `qwenEnableSearch`     | boolean         | 非必填   | -      | 是否启用通义千问内置的互联网搜索功能。                                                                  |
| `qwenFileIds`          | array of string | 非必填   | -      | 通过文件接口上传至 Dashscope 的文件 ID，其内容将被用做 AI 对话的上下文。不可与 `context` 字段同时配置。 |
| `qwenEnableCompatible` | boolean         | 非必填   | false  | 开启通义千问兼容模式。启用通义千问兼容模式后，将调用千问的兼容模式接口，同时对请求/响应不做修改。       |

#### 百川智能 (Baichuan AI)

百川智能所对应的 `type` 为 `baichuan` 。它并无特有的配置字段。

#### 零一万物（Yi）

零一万物所对应的 `type` 为 `yi`。它并无特有的配置字段。

#### 智谱 AI（Zhipu AI）

智谱 AI 所对应的 `type` 为 `zhipuai`。它并无特有的配置字段。

#### DeepSeek（DeepSeek）

DeepSeek 所对应的 `type` 为 `deepseek`。它并无特有的配置字段。

#### Groq

Groq 所对应的 `type` 为 `groq`。它并无特有的配置字段。

#### Grok

Grok 所对应的 `type` 为 `grok`。它并无特有的配置字段。

#### OpenRouter

OpenRouter 所对应的 `type` 为 `openrouter`。它并无特有的配置字段。

#### Fireworks AI

Fireworks AI 所对应的 `type` 为 `fireworks`。它并无特有的配置字段。

#### Galadriel

Galadriel 所对应的 `type` 为 `galadriel`。它并无特有的配置字段。

#### Systemone（Jev 决策服务）

Systemone 所对应的 `type` 为 `systemone`，用于将 Jev 决策契约（jevcompat SPEC 0.1，`POST /v1/systemone`）作为推理上游。请求路径不做改写（`/v1/systemone` 与 `/v1/models` 直通），Host 头固定为配置的 endpoint 的 authority（缺省为托管 `api.typesafe.ai`）。

特有配置字段：

- `systemoneEndpoint`：Jev 决策服务的 base url；缺省使用托管 TypeSafe API（`https://api.typesafe.ai`）。
- `systemoneModel`：请求未携带 `model` 字段时使用的默认决策引擎模型。
- `typesafeDangerZoneHeader`：`TypeSafe-Danger-Zone-Images` opt-in 头的注入策略，取值 `auto`（默认，请求携带图片且客户端未显式发送该头时注入）、`true`（无条件确保该头存在，不覆盖客户端显式发送的值）、`false`（完全不注入）。

#### 文心一言（Baidu）

文心一言所对应的 `type` 为 `baidu`。它并无特有的配置字段。

#### 360 智脑

360 智脑所对应的 `type` 为 `ai360`。它并无特有的配置字段。

#### GitHub 模型

GitHub 模型所对应的 `type` 为 `github`。它并无特有的配置字段。

#### Mistral

Mistral 所对应的 `type` 为 `mistral`。它并无特有的配置字段。

#### MiniMax

MiniMax 所对应的 `type` 为 `minimax`。它特有的配置字段如下：

| 名称             | 数据类型 | 填写要求                       | 默认值 | 描述                                                                    |
| ---------------- | -------- | ------------------------------ | ------ | ----------------------------------------------------------------------- |
| `minimaxApiType` | string   | v2 和 pro 中选填一项           | v2     | v2 代表 ChatCompletion v2 API，pro 代表 ChatCompletion Pro API          |
| `minimaxGroupId` | string   | `minimaxApiType` 为 pro 时必填 | -      | `minimaxApiType` 为 pro 时使用 ChatCompletion Pro API，需要设置 groupID |

#### Anthropic Claude

Anthropic Claude 所对应的 `type` 为 `claude`。它特有的配置字段如下：

| 名称            | 数据类型 | 填写要求 | 默认值 | 描述                                      |
| --------------- | -------- | -------- | ------ | ----------------------------------------- |
| `claudeVersion` | string   | 可选     | -      | Claude 服务的 API 版本，默认为 2023-06-01 |
| `claudeCodeMode` | boolean | 可选     | false  | 启用 Claude Code 模式，用于支持 Claude Code OAuth 令牌认证。启用后将伪装成 Claude Code 客户端发起请求 |

**Claude Code 模式说明**

启用 `claudeCodeMode: true` 时，插件将：
- 使用 Bearer Token 认证替代 x-api-key（适配 Claude Code OAuth 令牌）
- 设置 Claude Code 特定的请求头（user-agent、x-app、anthropic-beta）
- 为请求 URL 添加 `?beta=true` 查询参数
- 自动注入 Claude Code 的系统提示词（如未提供）

这允许在 Higress 中直接使用 Claude Code 的 OAuth Token 进行身份验证。

#### Ollama

Ollama 所对应的 `type` 为 `ollama`。它特有的配置字段如下：

| 名称               | 数据类型 | 填写要求 | 默认值 | 描述                                |
| ------------------ | -------- | -------- | ------ | ----------------------------------- |
| `ollamaServerHost` | string   | 必填     | -      | Ollama 服务器的主机地址             |
| `ollamaServerPort` | number   | 必填     | -      | Ollama 服务器的端口号，默认为 11434 |

#### 通用代理（Generic）

当只需要借助 AI Proxy 的鉴权、basePath 处理或首包超时能力，且不希望插件改写路径时，可将 `provider.type` 设置为 `generic`。该 Provider 不绑定任何模型厂商，也不会做能力映射。

| 名称          | 数据类型 | 填写要求 | 默认值 | 描述                                                                 |
| ------------- | -------- | -------- | ------ | -------------------------------------------------------------------- |
| `genericHost` | string   | 非必填   | -      | 指定要转发到的目标 Host；未配置时沿用客户端请求的 Host。 |

- 配置了 `apiTokens` 时，会自动写入 `Authorization: Bearer <token>` 请求头，复用全局的 Token 轮询能力。
- 当配置了 `firstByteTimeout` 时，会自动注入 `x-envoy-upstream-rq-first-byte-timeout-ms`。
- `basePath` 与 `basePathHandling` 同样适用，可在通用转发中快捷地移除或添加统一前缀。

#### 混元

混元所对应的 `type` 为 `hunyuan`。它特有的配置字段如下：

| 名称             | 数据类型 | 填写要求 | 默认值 | 描述                       |
| ---------------- | -------- | -------- | ------ | -------------------------- |
| `hunyuanAuthId`  | string   | 必填     | -      | 混元用于 v3 版本认证的 id  |
| `hunyuanAuthKey` | string   | 必填     | -      | 混元用于 v3 版本认证的 key |

#### 阶跃星辰 (Stepfun)

阶跃星辰所对应的 `type` 为 `stepfun`。它并无特有的配置字段。

#### Cloudflare Workers AI

Cloudflare Workers AI 所对应的 `type` 为 `cloudflare`。它特有的配置字段如下：

| 名称                  | 数据类型 | 填写要求 | 默认值 | 描述                                                                                                                       |
| --------------------- | -------- | -------- | ------ | -------------------------------------------------------------------------------------------------------------------------- |
| `cloudflareAccountId` | string   | 必填     | -      | [Cloudflare Account ID](https://developers.cloudflare.com/workers-ai/get-started/rest-api/#1-get-api-token-and-account-id) |

#### 星火 (Spark)

星火所对应的 `type` 为 `spark`。它并无特有的配置字段。

讯飞星火认知大模型的`apiTokens`字段值为`APIKey:APISecret`。即填入自己的 APIKey 与 APISecret，并以`:`分隔。

#### Gemini

Gemini 所对应的 `type` 为 `gemini`。它特有的配置字段如下：

| 名称                   | 数据类型      | 填写要求 | 默认值   | 描述                                                         |
| ---------------------- | ------------- | -------- | -------- | ------------------------------------------------------------ |
| `geminiSafetySetting`  | map of string | 非必填   | -        | Gemini AI 内容过滤和安全级别设定。参考[Safety settings](https://ai.google.dev/gemini-api/docs/safety-settings) |
| `apiVersion`           | string        | 非必填   | `v1beta` | 用于指定 API 的版本, 可选择 `v1` 或 `v1beta` 。 版本差异请参考[API versions explained](https://ai.google.dev/gemini-api/docs/api-versions)。 |
| `geminiThinkingBudget` | number        | 非必填   | -        | gemini2.5系列的参数，0是不开启思考，-1动态调整，具体参数指可参考官网 |

#### DeepL

DeepL 所对应的 `type` 为 `deepl`。它特有的配置字段如下：

| 名称         | 数据类型 | 填写要求 | 默认值 | 描述                         |
| ------------ | -------- | -------- | ------ | ---------------------------- |
| `targetLang` | string   | 必填     | -      | DeepL 翻译服务需要的目标语种 |

#### Cohere

Cohere 所对应的 `type` 为 `cohere`。它并无特有的配置字段。

#### Together-AI

Together-AI 所对应的 `type` 为 `together-ai`。它并无特有的配置字段。

#### Dify

Dify 所对应的 `type` 为 `dify`。它特有的配置字段如下:

| 名称             | 数据类型 | 填写要求 | 默认值 | 描述                                                                             |
| ---------------- | -------- | -------- | ------ | -------------------------------------------------------------------------------- |
| `difyApiUrl`     | string   | 非必填   | -      | dify 私有化部署的 url                                                            |
| `botType`        | string   | 非必填   | -      | dify 的应用类型，Chat/Completion/Agent/Workflow                                  |
| `inputVariable`  | string   | 非必填   | -      | dify 中应用类型为 workflow 时需要设置输入变量，当 botType 为 workflow 时一起使用 |
| `outputVariable` | string   | 非必填   | -      | dify 中应用类型为 workflow 时需要设置输出变量，当 botType 为 workflow 时一起使用 |

#### Google Vertex AI

Google Vertex AI 所对应的 type 为 vertex。支持两种认证模式：

**标准模式**（使用 Service Account）：

| 名称                         | 数据类型       | 填写要求   | 默认值    | 描述                                                                            |
|-----------------------------|---------------|--------|--------|-------------------------------------------------------------------------------|
| `vertexAuthKey`             | string        | 必填     | -      | 用于认证的 Google Service Account JSON Key，格式为 PEM 编码的 PKCS#8 私钥和 client_email 等信息 |
| `vertexRegion`              | string        | 必填     | -      | Google Cloud 区域（如 us-central1, europe-west4 等），用于构建 Vertex API 地址             |
| `vertexProjectId`           | string        | 必填     | -      | Google Cloud 项目 ID，用于标识目标 GCP 项目                                              |
| `vertexAuthServiceName`     | string        | 必填     | -      | 用于 OAuth2 认证的服务名称，该服务为了访问oauth2.googleapis.com                                |
| `geminiSafetySetting`       | map of string | 非必填   | -      | Gemini AI 内容过滤和安全级别设定。参考[Safety settings](https://ai.google.dev/gemini-api/docs/safety-settings)                             |
| `vertexTokenRefreshAhead`   | number        | 非必填   | -      | Vertex access token刷新提前时间(单位秒)                                                |

**Express Mode**（使用 API Key，简化配置）：

Express Mode 是 Vertex AI 推出的简化访问模式，只需 API Key 即可快速开始使用，无需配置 Service Account。详见 [Vertex AI Express Mode 文档](https://cloud.google.com/vertex-ai/generative-ai/docs/start/express-mode/overview)。

| 名称                         | 数据类型       | 填写要求   | 默认值    | 描述                                                                            |
|-----------------------------|---------------|--------|--------|-------------------------------------------------------------------------------|
| `apiTokens`                 | array of string | 必填   | -      | Express Mode 使用的 API Key，从 Google Cloud Console 的 API & Services > Credentials 获取 |
| `geminiSafetySetting`       | map of string | 非必填   | -      | Gemini AI 内容过滤和安全级别设定。参考[Safety settings](https://ai.google.dev/gemini-api/docs/safety-settings)                             |

**OpenAI 兼容模式**（使用 Vertex AI Chat Completions API）：

Vertex AI 提供了 OpenAI 兼容的 Chat Completions API 端点，可以直接使用 OpenAI 格式的请求和响应，无需进行协议转换。详见 [Vertex AI OpenAI 兼容性文档](https://cloud.google.com/vertex-ai/generative-ai/docs/migrate/openai/overview)。

| 名称                         | 数据类型       | 填写要求   | 默认值    | 描述                                                                            |
|-----------------------------|---------------|--------|--------|-------------------------------------------------------------------------------|
| `vertexOpenAICompatible`    | boolean       | 非必填   | false  | 启用 OpenAI 兼容模式。启用后将使用 Vertex AI 的 OpenAI-compatible Chat Completions API |
| `vertexAuthKey`             | string        | 必填     | -      | 用于认证的 Google Service Account JSON Key |
| `vertexRegion`              | string        | 必填     | -      | Google Cloud 区域（如 us-central1, europe-west4 等） |
| `vertexProjectId`           | string        | 必填     | -      | Google Cloud 项目 ID |
| `vertexAuthServiceName`     | string        | 必填     | -      | 用于 OAuth2 认证的服务名称 |

**注意**：OpenAI 兼容模式与 Express Mode 互斥，不能同时配置 `apiTokens` 和 `vertexOpenAICompatible`。

#### AWS Bedrock

AWS Bedrock 所对应的 type 为 bedrock。它支持两种认证方式：

1. **AWS Signature V4 认证**：使用 `awsAccessKey` 和 `awsSecretKey` 进行 AWS 标准签名认证
2. **Bearer Token 认证**：使用 `apiTokens` 配置 AWS Bearer Token（适用于 IAM Identity Center 等场景）

**注意**：两种认证方式二选一，如果同时配置了 `apiTokens`，将优先使用 Bearer Token 认证方式。

它特有的配置字段如下：

| 名称                        | 数据类型        | 填写要求            | 默认值 | 描述                                                |
|---------------------------|---------------|-------------------|-------|---------------------------------------------------|
| `apiTokens`               | array of string | 与 ak/sk 二选一   | -     | AWS Bearer Token，用于 Bearer Token 认证方式          |
| `awsAccessKey`            | string        | 与 apiTokens 二选一 | -     | AWS Access Key，用于 AWS Signature V4 认证            |
| `awsSecretKey`            | string        | 与 apiTokens 二选一 | -     | AWS Secret Access Key，用于 AWS Signature V4 认证     |
| `awsRegion`               | string        | 必填              | -     | AWS 区域，例如：us-east-1                              |
| `bedrockAdditionalFields` | map           | 非必填            | -     | Bedrock 额外模型请求参数                               |

#### NVIDIA Triton Interference Server

NVIDIA Triton Interference Server 所对应的 type 为 triton。它特有的配置字段如下：

| 名称                   | 数据类型 | 填写要求 | 默认值 | 描述                                      |
|----------------------|--------|--------|-------|------------------------------------------|
| `tritonModelVersion` | string | 非必填   | -     | 用于指定 Triton Server 中 model version     |
| `tritonDomain`       | string | 非必填   | -     | Triton Server 部署的指定请求 Domain          |

## tool/tool_calls 配对校验

> GPUStack 本地特性，上游 `ai-proxy` 没有此能力。见 gpustack/gpustack#6210。

当 Chat Completions 请求中的 `tool` 消息与 `assistant.tool_calls` 没有正确配对时，行为完全取决于
后端：严格的后端（如 vLLM）会直接返回 400，宽松的 OpenAI 兼容后端则会照常返回 200。其中最危险的是
**tool_calls 悬空**（assistant 发起了工具调用，但没有任何 tool 消息返回结果）：宽松后端会返回 200
并**凭空编造工具结果**，`finish_reason` 仍为 `stop`，调用方无从分辨。

本插件在请求路径上做一层纯结构化校验，定位是**拦截并告警**，不做任何修复。

### 校验规则

1. 每个 `role: "tool"` 消息的 `tool_call_id`，必须能在**之前**某个 `assistant.tool_calls[]` 的 `id` 中找到；
2. 带 `tool_calls` 的 assistant 消息之后，必须紧接着每个调用各一条 tool 消息；在全部回应完之前不允许出现其它 role；
3. 同一个请求内 `tool_call_id` 不可重复；
4. `assistant.tool_calls[].id` 不可重复；
5. `tool_calls[].id` 与 `tool_call_id` 都必须是**非空 JSON 字符串** —— 缺失、`null`、数字都视为畸形。
   （否则 gjson 会把「缺失」和 `null` 都读成 `""`、把数字读成它的十进制，两边刚好能配上而蒙混过关。）

规则 4 实际按**整个请求**判重，比字面表述更严格：如果两轮 assistant 复用同一个 `id`，回应它们的
tool 消息必然让 `tool_call_id` 重复，本来就会被规则 3 拒掉；在**签发** id 的位置报错，错误信息更有指向性。

插件**不会修复**消息列表（不补造 tool 结果、不去重 id）。网关不了解业务语义，错误的修复比不修复更糟。

### 失败时的行为

**这是一个 opt-in 能力,默认 `off`。** 不配置时插件完全不做这项校验,连扫描都不跑,行为与不带这个特性的
版本一模一样。本插件是上游 `ai-proxy` 的 fork、会随版本下发到所有部署,默认拦截等于让 fork 单方面把
今天能跑的请求变成 400,所以由需要的部署显式配 `toolCallValidation: strict` 打开。

`strict` 下校验失败返回 400，响应体为 OpenAI 风格的错误信封，`message` 与上游后端的措辞对齐，便于调用方复用已有的错误处理：

```json
{
  "error": {
    "message": "Messages with role 'tool' must be a response to a preceding message with 'tool_calls'",
    "type": "invalid_request_error",
    "code": "invalid_request_error"
  }
}
```

规则 1、3 对应上面这条消息；规则 2 对应
`An assistant message with 'tool_calls' must be followed by tool messages responding to each 'tool_call_id'.`；
规则 4 对应 `An assistant message with 'tool_calls' must not reuse a 'tool_calls[].id'.`。

### 日志与指标

每次拦截都会打印一条 WARN 日志，包含命中的规则、`model`、`consumer`（`x-mse-consumer`）以及出错的
消息下标与 `tool_call_id`。

同时递增计数器 `gpustack_ai_proxy_tool_call_pairing_rejected_total`，stat 名遵循 Higress AI 插件约定：

```text
route.<route>.upstream.<cluster>.model.<model>.consumer.<consumer>.metric.gpustack_ai_proxy_tool_call_pairing_rejected_total.rule.<rule>
```

其中 `ai_route`、`ai_cluster` 由 Higress 自带的 stats_tags 自动提取为 Prometheus label；`rule` 保留在
stat 名中，需要时可在 Prometheus 侧用 `metric_relabel_configs` 拆成 label。

`rule` 的取值为：`orphan_tool_message`、`unanswered_tool_calls`、`duplicate_tool_call_id`、
`duplicate_tool_calls_id`、`invalid_tool_call_id`、`invalid_tool_calls_id`。

**指标上的 label 全部由配置决定，不含任何请求可控的值** —— stat 名里的 `model.`/`consumer.` 槽位固定填
`none`（保留槽位是因为 Higress 的 stats_tags 正则靠这两个字面分隔符来提取 `ai_route`/`ai_cluster`）。
被拒的请求很廉价（400、不回源），如果把 `model`（来自请求体）或 `x-mse-consumer`（前面没有 key-auth 的
路由上客户端可自行设置）做成 label，调用方就能按请求数无限增长 Envoy 的 stat 注册表和插件内的 counter 缓存。
定位到具体 API Key 的诉求由上面那条 WARN 日志承担 —— 日志是只追加的，不存在这个问题。

### 开销

`off`（默认）下开销为零 —— 连请求体都不看。

打开 `strict` 后，校验是一次 gjson 扫描,**不做 `encoding/json` 反射解码,也不重新序列化**,并且用
`unsafe.String` 把请求体交给 gjson,避免 `gjson.GetBytes` 对整个 `messages` 数组做一次拷贝。
分配量只与消息条数有关,与请求体大小无关。

基准(原生 Apple M4 Pro,非 wasm,仅供比例参考;见 `tool_call_pairing_bench_test.go`):

| 请求体 | 本校验 | 同一请求体做一次 `json.Unmarshal` |
| --- | --- | --- |
| 200 轮 / 1.2 MB | 0.62 ms,233 allocs,222 KB | 3.94 ms,3422 allocs,1.47 MB |
| 400 轮 / 1.2 MB | 0.82 ms,439 allocs,560 KB | 5.01 ms,6823 allocs,1.50 MB |
| 200 轮 / 2.4 MB | 1.17 ms,233 allocs,222 KB | 7.65 ms,3422 allocs,2.70 MB |

作为对照,Anthropic→OpenAI 协议转换的反射式解码 + 重新序列化是完全不同量级的开销
(见 gpustack/gpustack#6217);本校验不在那条路径上。

### 适用范围

- 仅作用于 Chat Completions（`/v1/chat/completions`）请求，且在任何请求体改写之前执行，因此报错描述的是客户端**实际发出**的内容；
- Anthropic Messages（`/v1/messages`）用 `tool_use`/`tool_result` content block 表达同一关系，不在本校验范围内，包括被自动转换到 chat completions 路径的 Claude 请求；
- 请求体不是 JSON、或没有 `messages` 数组时，直接放行（fail-open）。

### 配置示例

```yaml
toolCallValidation: strict      # 不写 = off，即完全关闭
provider:
  type: openai
  apiTokens:
    - "YOUR_API_TOKEN"
```

在 GPUStack 部署中，该字段写在 WasmPlugin `gpustack-ai-proxy` 的 `spec.defaultConfig` 顶层即可；
GPUStack 的调谐器只会重写 `defaultConfig.providers` 和 `matchRules`，不会清掉其它 key。
由于 matchRule 未显式声明时会继承全局值，写在 `defaultConfig` 上即对所有路由生效；需要只对某条路由拦截时，
把它写进那条 `matchRules[].config` 即可。

配置了无法识别的取值时**不会**报配置错误，而是回退到默认的 `off` 并打一条 ERROR 日志 —— wasm-go 会吞掉
全局配置的解析错误并把全局配置清零，而 GPUStack 只在 `defaultConfig` 里放 `providers`，一旦清零整个
ai-proxy 会静默空转。因一个字段拼错拖垮整个代理，代价远大于按默认模式继续跑。

## 推理请求优先级

把调用方的推理请求优先级（由 `gpustack-ext-auth` 按用户 / API Key 解析并写入请求头）翻译成后端自己的
调度输入（gpustack/gpustack#6739）。两个扁平键，各管一件事：

| 键 | 位置 | 含义 |
| --- | --- | --- |
| `requestPrioritySource` | `defaultConfig` 顶层，matchRule 未声明时继承 | 携带优先级的请求头名，缺省 `x-gpustack-request-priority`。值是非负整数，**越大越紧急**；请求没带该头时按 `0` 处理。读取后该头即被移除，不会到达上游 |
| `requestPriorityTarget` | 每个 `provider` 条目 | 该 provider 背后是什么后端：`vllm`、`sglang`，或不设（不处理）。GPUStack 按模型的后端类型渲染 |

### 两种后端的翻译

| `requestPriorityTarget` | 写入 | 值 | 原因 |
| --- | --- | --- | --- |
| `vllm` | 请求头 `x-vllm-priority`，以及请求体顶层 `priority` | `-p` | vLLM **越小越优先**。generation 类端点读请求头（并覆盖 body 里的 `priority`），pooling 类端点（`/v1/embeddings`）只读请求体，所以两处都写、每个请求都写（包括 `0`），客户端自带的 body `priority` 两种情况下都失效 |
| `sglang` | 请求体顶层 `priority` | `p` | SGLang **越大越优先**，只读请求体，没有对应的请求头 |
| 不设 / 非法 | 不写 | - | 非法值打一条 ERROR 日志后按不设处理，不算配置错误（原因同 `toolCallValidation`） |

同一个 `p` 在两个后端上的排序结果一致，只是符号相反。`0` 落在各后端自己的基线上，与未经网关的流量平等。
SGLang 后端**不要**加 `--schedule-low-priority-values-first`，否则它的方向翻转、高优先级用户变成最低。

例：交互式应用 `p = 20`、普通用户未配置、次要业务 `p = 5`。

| 调用者 | vLLM 收到 | SGLang 收到 |
| --- | --- | --- |
| 交互式应用 | `x-vllm-priority: -20` | `"priority": 20` |
| 次要业务 | `x-vllm-priority: -5` | `"priority": 5` |
| 普通用户 | `x-vllm-priority: 0` | `"priority": 0`（显式写 0：SGLang 把缺省当作最低，不是基线） |

### 生效范围

- vLLM 的请求头在请求头阶段写入，与协议无关（OpenAI / Claude 入站均可）。
- 请求体成员（两种目标都写）写在**最终**请求体上：协议转换之后（Claude → OpenAI 转换会重建请求体）、`provider.context`
  插入系统消息之后（会再次重建请求体）、`retryOnFailure` 重试时（回放的是处理前的请求体、请求头从零构建，header 也一并重写）
  都会重新写入。仅对 chat completions / completions / responses / embeddings 且请求体为 JSON 的请求生效；multipart 与
  `/v1/images/*` 不碰。写入前先删掉请求体里所有客户端自带的 `priority` 成员（无论多少个，一趟完成），重复出现也只会剩
  网关写的那一个。`generic` provider 不走 `handleRequestBody`、原样转发请求体，由它自己的 `OnRequestBody` 写入。
- 插件只改写它会读取的请求体，所以决定“读不读”的媒体类型判断对 `application/json` 不区分大小写、并识别
  `application/*+json`；否则 `Content-Type: Application/JSON` 就能绕过覆盖。
- vLLM 的 `/v1/score`、`/v1/rerank`、`/v1/classify` 不在本插件的 API 分类里，原样透传，优先级对它们不生效。
- **SGLang 的 Anthropic 原生端点（`/v1/messages`）没有 `priority` 字段**：模型开启 `native_anthropic_api`
  时 Claude 请求原样透传，优先级不生效且无报错；关闭该开关让 ai-proxy 转成 OpenAI 协议即可生效。
- 优先级是否真正参与调度取决于后端启动参数：vLLM 需 `--scheduling-policy priority`，SGLang 需
  `--enable-priority-scheduling`。未启用时 vLLM 对非 0 值返回错误，SGLang 静默忽略。
- 源头的请求头由 `gpustack-ext-auth` 在认证阶段写入并剥离客户端伪造的副本；本插件信任它，但会按非负 32 位整数
  （vLLM 校验的范围）兜底：值非法或超范围时**每个 VM** 打一条 WARN（值受客户端影响，不参与 warn-once 的 key）
  后按 `0` 处理，不中断请求。

### 配置示例

```yaml
requestPrioritySource: x-gpustack-request-priority   # 不写即此值
providers:
  - id: model-42            # vLLM 后端的模型
    type: openai
    requestPriorityTarget: vllm
  - id: model-43            # SGLang 后端的模型
    type: openai
    requestPriorityTarget: sglang
  - id: model-7             # 外部 provider / 其它后端：不设 = 不处理
    type: openai
```

## 用法示例

### 使用 OpenAI 协议代理 Azure OpenAI 服务

使用最基本的 Azure OpenAI 服务，不配置任何上下文。

**配置信息**

```yaml
provider:
  type: azure
  apiTokens:
    - "YOUR_AZURE_OPENAI_API_TOKEN"
  azureServiceUrl: "https://YOUR_RESOURCE_NAME.openai.azure.com/openai/v1",
```

**请求示例**

```json
{
  "model": "gpt-3",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "temperature": 0.3
}
```

**响应示例**

```json
{
  "choices": [
    {
      "content_filter_results": {
        "hate": {
          "filtered": false,
          "severity": "safe"
        },
        "self_harm": {
          "filtered": false,
          "severity": "safe"
        },
        "sexual": {
          "filtered": false,
          "severity": "safe"
        },
        "violence": {
          "filtered": false,
          "severity": "safe"
        }
      },
      "finish_reason": "stop",
      "index": 0,
      "logprobs": null,
      "message": {
        "content": "你好！我是一个AI助手，可以回答你的问题和提供帮助。有什么我可以帮到你的吗？",
        "role": "assistant"
      }
    }
  ],
  "created": 1714807624,
  "id": "chatcmpl-abcdefg1234567890",
  "model": "gpt-35-turbo-16k",
  "object": "chat.completion",
  "prompt_filter_results": [
    {
      "prompt_index": 0,
      "content_filter_results": {
        "hate": {
          "filtered": false,
          "severity": "safe"
        },
        "self_harm": {
          "filtered": false,
          "severity": "safe"
        },
        "sexual": {
          "filtered": false,
          "severity": "safe"
        },
        "violence": {
          "filtered": false,
          "severity": "safe"
        }
      }
    }
  ],
  "system_fingerprint": null,
  "usage": {
    "completion_tokens": 40,
    "prompt_tokens": 15,
    "total_tokens": 55
  }
}
```

### 使用 OpenAI 协议代理通义千问服务

使用通义千问服务，并配置从 OpenAI 大模型到通义千问的模型映射关系。

**配置信息**

```yaml
provider:
  type: qwen
  apiTokens:
    - 'YOUR_QWEN_API_TOKEN'
  modelMapping:
    'gpt-3': 'qwen-turbo'
    'gpt-35-turbo': 'qwen-plus'
    'gpt-4-turbo': 'qwen-max'
    'gpt-4-*': 'qwen-max'
    'gpt-4o': 'qwen-vl-plus'
    'text-embedding-v1': 'text-embedding-v1'
    '*': 'qwen-turbo'
```

**AI 对话请求示例**

URL: <http://your-domain/v1/chat/completions>

请求示例：

```json
{
  "model": "gpt-3",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "temperature": 0.3
}
```

响应示例：

```json
{
  "id": "c2518bd3-0f46-97d1-be34-bb5777cb3108",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "我是通义千问，由阿里云开发的AI助手。我可以回答各种问题、提供信息和与用户进行对话。有什么我可以帮助你的吗？"
      },
      "finish_reason": "stop"
    }
  ],
  "created": 1715175072,
  "model": "qwen-turbo",
  "object": "chat.completion",
  "usage": {
    "prompt_tokens": 24,
    "completion_tokens": 33,
    "total_tokens": 57
  }
}
```

**多模态模型 API 请求示例（适用于 `qwen-vl-plus` 和 `qwen-vl-max` 模型）**

URL: <http://your-domain/v1/chat/completions>

请求示例：

```json
{
  "model": "gpt-4o",
  "messages": [
    {
      "role": "user",
      "content": [
        {
          "type": "image_url",
          "image_url": {
            "url": "https://dashscope.oss-cn-beijing.aliyuncs.com/images/dog_and_girl.jpeg"
          }
        },
        {
          "type": "text",
          "text": "这个图片是哪里？"
        }
      ]
    }
  ],
  "temperature": 0.3
}
```

响应示例：

```json
{
  "id": "17c5955d-af9c-9f28-bbde-293a9c9a3515",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": [
          {
            "text": "这张照片显示的是一位女士和一只狗在海滩上。由于我无法获取具体的地理位置信息，所以不能确定这是哪个地方的海滩。但是从视觉内容来看，它可能是一个位于沿海地区的沙滩海岸线，并且有海浪拍打着岸边。这样的场景在全球许多美丽的海滨地区都可以找到。如果您需要更精确的信息，请提供更多的背景或细节描述。"
          }
        ]
      },
      "finish_reason": "stop"
    }
  ],
  "created": 1723949230,
  "model": "qwen-vl-plus",
  "object": "chat.completion",
  "usage": {
    "prompt_tokens": 1279,
    "completion_tokens": 78
  }
}
```

**文本向量请求示例**

URL: <http://your-domain/v1/embeddings>

请求示例：

```json
{
  "model": "text-embedding-v1",
  "input": "Hello"
}
```

响应示例：

```json
{
  "object": "list",
  "data": [
    {
      "object": "embedding",
      "index": 0,
      "embedding": [
        -1.0437825918197632,
        5.208984375,
        3.0483806133270264,
        -1.7897135019302368,
        -2.0107421875,
        ...,
        0.8125,
        -1.1759847402572632,
        0.8174641728401184,
        1.0432943105697632,
        -0.5885213017463684
      ]
    }
  ],
  "model": "text-embedding-v1",
  "usage": {
    "prompt_tokens": 1,
    "total_tokens": 1
  }
}
```

### 使用通义千问配合纯文本上下文信息

使用通义千问服务，同时配置纯文本上下文信息。

**配置信息**

```yaml
provider:
  type: qwen
  apiTokens:
    - "YOUR_QWEN_API_TOKEN"
  modelMapping:
    "*": "qwen-turbo"
  context:
    - fileUrl: "http://file.default.svc.cluster.local/ai/context.txt",
      serviceName: "file.dns",
      servicePort: 80
```

**请求示例**

```json
{
  "model": "gpt-3",
  "messages": [
    {
      "role": "user",
      "content": "请概述文案内容"
    }
  ],
  "temperature": 0.3
}
```

**响应示例**

```json
{
  "id": "cmpl-77861a17681f4987ab8270dbf8001936",
  "object": "chat.completion",
  "created": 9756990,
  "model": "moonshot-v1-128k",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "这份文案是一份关于..."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 20181,
    "completion_tokens": 439,
    "total_tokens": 20620
  }
}
```

### 使用通义千问配合其原生的文件上下文

提前上传文件至通义千问，以文件内容作为上下文使用其 AI 服务。

**配置信息**

```yaml
provider:
  type: qwen
  apiTokens:
    - 'YOUR_QWEN_API_TOKEN'
  modelMapping:
    '*': 'qwen-long' # 通义千问的文件上下文只能在 qwen-long 模型下使用
  qwenFileIds:
    - 'file-fe-xxx'
    - 'file-fe-yyy'
```

**请求示例**

```json
{
  "model": "gpt-4-turbo",
  "messages": [
    {
      "role": "user",
      "content": "请概述文案内容"
    }
  ],
  "temperature": 0.3
}
```

**响应示例**

```json
{
  "output": {
    "choices": [
      {
        "finish_reason": "stop",
        "message": {
          "role": "assistant",
          "content": "您上传了两个文件，`context.txt` 和 `context_2.txt`，它们似乎都包含了关于xxxx"
        }
      }
    ]
  },
  "usage": {
    "total_tokens": 2023,
    "output_tokens": 530,
    "input_tokens": 1493
  },
  "request_id": "187e99ba-5b64-9ffe-8f69-01dafbaf6ed7"
}
```

### 使用 original 协议代理百炼智能体应用

**配置信息**

```yaml
provider:
  type: qwen
  apiTokens:
    - 'YOUR_DASHSCOPE_API_TOKEN'
  protocol: original
```

**请求实例**

```json
{
  "input": {
    "prompt": "介绍一下Dubbo"
  },
  "parameters": {},
  "debug": {}
}
```

**响应实例**

```json
{
  "output": {
    "finish_reason": "stop",
    "session_id": "677e7e8fbb874e1b84792b65042e1599",
    "text": "Apache Dubbo 是一个..."
  },
  "usage": {
    "models": [
      {
        "output_tokens": 449,
        "model_id": "qwen-max",
        "input_tokens": 282
      }
    ]
  },
  "request_id": "b59e45e3-5af4-91df-b7c6-9d746fd3297c"
}
```

### 使用 OpenAI 协议代理豆包大模型服务

**配置信息**

```yaml
provider:
  type: doubao
  apiTokens:
    - YOUR_DOUBAO_API_KEY
  modelMapping:
    '*': YOUR_DOUBAO_ENDPOINT
  timeout: 1200000
```

### 使用 original 协议代理 Coze 应用

**配置信息**

```yaml
provider:
  type: coze
  apiTokens:
    - YOUR_COZE_API_KEY
  protocol: original
```

### 使用月之暗面配合其原生的文件上下文

提前上传文件至月之暗面，以文件内容作为上下文使用其 AI 服务。

**配置信息**

```yaml
provider:
  type: moonshot
  apiTokens:
    - "YOUR_MOONSHOT_API_TOKEN"
  moonshotFileId: "YOUR_MOONSHOT_FILE_ID",
  modelMapping:
    '*': "moonshot-v1-32k"
```

**请求示例**

```json
{
  "model": "gpt-4-turbo",
  "messages": [
    {
      "role": "user",
      "content": "请概述文案内容"
    }
  ],
  "temperature": 0.3
}
```

**响应示例**

```json
{
  "id": "cmpl-e5ca873642ca4f5d8b178c1742f9a8e8",
  "object": "chat.completion",
  "created": 1872961,
  "model": "moonshot-v1-128k",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "文案内容是关于一个名为“xxxx”的支付平台..."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 11,
    "completion_tokens": 498,
    "total_tokens": 509
  }
}
```

### 使用 OpenAI 协议代理 Groq 服务

**配置信息**

```yaml
provider:
  type: groq
  apiTokens:
    - 'YOUR_GROQ_API_TOKEN'
```

**请求示例**

```json
{
  "model": "llama3-8b-8192",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ]
}
```

**响应示例**

```json
{
  "id": "chatcmpl-26733989-6c52-4056-b7a9-5da791bd7102",
  "object": "chat.completion",
  "created": 1715917967,
  "model": "llama3-8b-8192",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "😊 Ni Hao! (That's \"hello\" in Chinese!)\n\nI am LLaMA, an AI assistant developed by Meta AI that can understand and respond to human input in a conversational manner. I'm not a human, but a computer program designed to simulate conversations and answer questions to the best of my ability. I'm happy to chat with you in Chinese or help with any questions or topics you'd like to discuss! 😊"
      },
      "logprobs": null,
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 16,
    "prompt_time": 0.005,
    "completion_tokens": 89,
    "completion_time": 0.104,
    "total_tokens": 105,
    "total_time": 0.109
  },
  "system_fingerprint": "fp_dadc9d6142",
  "x_groq": {
    "id": "req_01hy2awmcxfpwbq56qh6svm7qz"
  }
}
```

### 使用 OpenAI 协议代理 Grok 服务

**配置信息**

```yaml
provider:
  type: grok
  apiTokens:
    - 'YOUR_GROK_API_TOKEN'
```

**请求示例**

```json
{
  "messages": [
    {
      "role": "system",
      "content": "You are a helpful assistant that can answer questions and help with tasks."
    },
    {
      "role": "user",
      "content": "What is 101*3?"
    }
  ],
  "model": "grok-4"
}
```

**响应示例**

```json
{
  "id": "a3d1008e-4544-40d4-d075-11527e794e4a",
  "object": "chat.completion",
  "created": 1752854522,
  "model": "grok-4",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "101 multiplied by 3 is 303.",
        "refusal": null
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 32,
    "completion_tokens": 9,
    "total_tokens": 135,
    "prompt_tokens_details": {
      "text_tokens": 32,
      "audio_tokens": 0,
      "image_tokens": 0,
      "cached_tokens": 6
    },
    "completion_tokens_details": {
      "reasoning_tokens": 94,
      "audio_tokens": 0,
      "accepted_prediction_tokens": 0,
      "rejected_prediction_tokens": 0
    },
    "num_sources_used": 0
  },
  "system_fingerprint": "fp_3a7881249c"
}
```

### 使用 OpenAI 协议代理 OpenRouter 服务

**配置信息**

```yaml
provider:
  type: openrouter
  apiTokens:
    - 'YOUR_OPENROUTER_API_TOKEN'
  modelMapping:
    'gpt-4': 'openai/gpt-4-turbo-preview'
    'gpt-3.5-turbo': 'openai/gpt-3.5-turbo'
    'claude-3': 'anthropic/claude-3-opus'
    '*': 'openai/gpt-3.5-turbo'
```

**请求示例**

```json
{
  "model": "gpt-4",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "temperature": 0.7
}
```

**响应示例**

```json
{
  "id": "gen-1234567890abcdef",
  "object": "chat.completion",
  "created": 1699123456,
  "model": "openai/gpt-4-turbo-preview",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "你好！我是一个AI助手，通过OpenRouter平台提供服务。我可以帮助回答问题、协助创作、进行对话等。有什么我可以帮助你的吗？"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 12,
    "completion_tokens": 46,
    "total_tokens": 58
  }
}
```

### 使用 OpenAI 协议代理 Fireworks AI 服务

**配置信息**

```yaml
provider:
  type: fireworks
  apiTokens:
    - "YOUR_FIREWORKS_API_TOKEN"
  modelMapping:
    "gpt-4": "accounts/fireworks/models/llama-v3p1-70b-instruct"
    "gpt-3.5-turbo": "accounts/fireworks/models/llama-v3p1-8b-instruct"
    "*": "accounts/fireworks/models/llama-v3p1-8b-instruct"
```

**请求示例**

```json
{
  "model": "gpt-4",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "temperature": 0.7,
  "max_tokens": 100
}
```

**响应示例**

```json
{
  "id": "fw-123456789",
  "object": "chat.completion",
  "created": 1699123456,
  "model": "accounts/fireworks/models/llama-v3p1-70b-instruct",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "你好！我是一个由 Fireworks AI 提供的人工智能助手，基于 Llama 3.1 模型。我可以帮助回答问题、进行对话和提供各种信息。有什么我可以帮助你的吗？"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 15,
    "completion_tokens": 45,
    "total_tokens": 60
  }
}
```

### 使用 OpenAI 协议代理 Galadriel 服务

**配置信息**

```yaml
provider:
  type: galadriel
  apiTokens:
    - "YOUR_GALADRIEL_API_TOKEN"
  modelMapping:
    "gpt-4": "llama3.1"
    "gpt-3.5-turbo": "llama3.1"
    "*": "llama3.1"
```

**请求示例**

```json
{
  "model": "llama3.1",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ]
}
```

**响应示例**

```json
{
  "id": "id",
  "choices": [
    {
      "finish_reason": "stop",
      "index": 0,
      "logprobs": null,
      "message": {
        "content": "你好！我是一个AI助手，基于Llama 3.1模型构建。我通过Galadriel网络运行，这是一个去中心化的AI推理平台。我可以帮助回答问题、进行对话、协助完成各种任务。有什么我可以帮助你的吗？",
        "refusal": null,
        "role": "assistant",
        "function_call": null,
        "tool_calls": null
      }
    }
  ],
  "created": 1728558433,
  "model": "neuralmagic/Meta-Llama-3.1-8B-Instruct-FP8",
  "object": "chat.completion",
  "service_tier": null,
  "system_fingerprint": null
}
```

### 使用自动协议兼容功能

插件现在支持自动协议检测，可以同时处理 OpenAI 和 Claude 两种协议格式的请求。

**配置信息**

```yaml
provider:
  type: claude  # 原生支持 Claude 协议的供应商
  apiTokens:
    - 'YOUR_CLAUDE_API_TOKEN'
  version: '2023-06-01'
```

**OpenAI 协议请求示例**

URL: `http://your-domain/v1/chat/completions`

```json
{
  "model": "claude-3-opus-20240229",
  "max_tokens": 1024,
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ]
}
```

**Claude 协议请求示例**

URL: `http://your-domain/v1/messages`

```json
{
  "model": "claude-3-opus-20240229",
  "max_tokens": 1024,
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ]
}
```

**响应示例**

两种协议格式的请求都会返回相应格式的响应：

```json
{
  "id": "msg_01Jt3GzyjuzymnxmZERJguLK",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "您好,我是一个由人工智能公司Anthropic开发的聊天助手。我的名字叫Claude,是一个聪明友善、知识渊博的对话系统。很高兴认识您!我可以就各种话题与您聊天,回答问题,提供建议和帮助。我会尽最大努力给您有帮助的回复。希望我们能有个愉快的交流!"
      },
      "finish_reason": "stop"
    }
  ],
  "created": 1717385918,
  "model": "claude-3-opus-20240229",
  "object": "chat.completion",
  "usage": {
    "prompt_tokens": 16,
    "completion_tokens": 126,
    "total_tokens": 142
  }
}
```

### 使用 Claude Code 模式

Claude Code 是 Anthropic 提供的官方 CLI 工具。通过启用 `claudeCodeMode`，可以使用 Claude Code 的 OAuth Token 进行身份验证：

**配置信息**

```yaml
provider:
  type: claude
  apiTokens:
    - 'sk-ant-oat01-xxxxx'  # Claude Code OAuth Token
  claudeCodeMode: true  # 启用 Claude Code 模式
```

启用此模式后，插件将自动：
- 使用 Bearer Token 认证（而非 x-api-key）
- 设置 Claude Code 特定的请求头和查询参数
- 注入 Claude Code 的系统提示词（如未提供）

**请求示例**

```json
{
  "model": "claude-sonnet-4-5-20250929",
  "max_tokens": 8192,
  "messages": [
    {
      "role": "user",
      "content": "List files in current directory"
    }
  ]
}
```

插件将自动转换为适合 Claude Code 的请求格式，包括：
- 添加系统提示词：`"You are Claude Code, Anthropic's official CLI for Claude."`
- 设置适当的认证和请求头

### 使用智能协议转换

当目标供应商不原生支持 Claude 协议时，插件会自动进行协议转换：

**配置信息**

```yaml
provider:
  type: qwen  # 不原生支持 Claude 协议，会自动转换
  apiTokens:
    - 'YOUR_QWEN_API_TOKEN'
  modelMapping:
    'claude-3-opus-20240229': 'qwen-max'
    '*': 'qwen-turbo'
```

**Claude 协议请求**

URL: `http://your-domain/v1/messages` (自动转换为 OpenAI 协议调用供应商)

```json
{
  "model": "claude-3-opus-20240229",
  "max_tokens": 1024,
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ]
}
```

### 使用 OpenAI 协议代理混元服务

**配置信息**

```yaml
provider:
  type: 'hunyuan'
  hunyuanAuthKey: '<YOUR AUTH KEY>'
  apiTokens:
    - ''
  hunyuanAuthId: '<YOUR AUTH ID>'
  timeout: 1200000
  modelMapping:
    '*': 'hunyuan-lite'
```

**请求示例**

请求脚本：

```shell
curl --location 'http://<your higress domain>/v1/chat/completions' \
--header 'Content-Type:  application/json' \
--data '{
  "model": "gpt-3",
  "messages": [
    {
      "role": "system",
      "content": "你是一个名专业的开发人员！"
    },
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "temperature": 0.3,
  "stream": false
}'
```

**响应示例**

```json
{
  "id": "fd140c3e-0b69-4b19-849b-d354d32a6162",
  "choices": [
    {
      "index": 0,
      "delta": {
        "role": "assistant",
        "content": "你好！我是一名专业的开发人员。"
      },
      "finish_reason": "stop"
    }
  ],
  "created": 1717493117,
  "model": "hunyuan-lite",
  "object": "chat.completion",
  "usage": {
    "prompt_tokens": 15,
    "completion_tokens": 9,
    "total_tokens": 24
  }
}
```

### 使用 OpenAI 协议代理百度文心一言服务

**配置信息**

```yaml
provider:
  type: baidu
  apiTokens:
    - 'YOUR_BAIDU_API_TOKEN'
  modelMapping:
    'gpt-3': 'ERNIE-4.0'
    '*': 'ERNIE-4.0'
```

**请求示例**

```json
{
  "model": "gpt-4-turbo",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "stream": false
}
```

**响应示例**

```json
{
  "id": "as-e90yfg1pk1",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "你好，我是文心一言，英文名是ERNIE Bot。我能够与人对话互动，回答问题，协助创作，高效便捷地帮助人们获取信息、知识和灵感。"
      },
      "finish_reason": "stop"
    }
  ],
  "created": 1717251488,
  "model": "ERNIE-4.0",
  "object": "chat.completion",
  "usage": {
    "prompt_tokens": 4,
    "completion_tokens": 33,
    "total_tokens": 37
  }
}
```

### 使用 OpenAI 协议代理 MiniMax 服务

**配置信息**

```yaml
provider:
  type: minimax
  apiTokens:
    - 'YOUR_MINIMAX_API_TOKEN'
  modelMapping:
    'gpt-3': 'abab6.5s-chat'
    'gpt-4': 'abab6.5g-chat'
    '*': 'abab6.5t-chat'
```

**请求示例**

```json
{
  "model": "gpt-3",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "stream": false
}
```

**响应示例**

```json
{
  "id": "03ac4fcfe1c6cc9c6a60f9d12046e2b4",
  "choices": [
    {
      "finish_reason": "stop",
      "index": 0,
      "message": {
        "content": "你好，我是一个由MiniMax公司研发的大型语言模型，名为MM智能助理。我可以帮助回答问题、提供信息、进行对话和执行多种语言处理任务。如果你有任何问题或需要帮助，请随时告诉我！",
        "role": "assistant",
        "name": "MM智能助理",
        "audio_content": ""
      }
    }
  ],
  "created": 1734155471,
  "model": "abab6.5s-chat",
  "object": "chat.completion",
  "usage": {
    "total_tokens": 116,
    "total_characters": 0,
    "prompt_tokens": 70,
    "completion_tokens": 46
  },
  "input_sensitive": false,
  "output_sensitive": false,
  "input_sensitive_type": 0,
  "output_sensitive_type": 0,
  "output_sensitive_int": 0,
  "base_resp": {
    "status_code": 0,
    "status_msg": ""
  }
}
```

### 使用 OpenAI 协议代理 GitHub 模型服务

**配置信息**

```yaml
provider:
  type: github
  apiTokens:
    - 'YOUR_GITHUB_ACCESS_TOKEN'
  modelMapping:
    'gpt-4o': 'gpt-4o'
    'gpt-4': 'Phi-3.5-MoE-instruct'
    'gpt-3.5': 'cohere-command-r-08-2024'
    'text-embedding-3-large': 'text-embedding-3-large'
```

**请求示例**

```json
{
  "messages": [
    {
      "role": "system",
      "content": "You are a helpful assistant."
    },
    {
      "role": "user",
      "content": "What is the capital of France?"
    }
  ],
  "stream": true,
  "temperature": 1.0,
  "top_p": 1.0,
  "max_tokens": 1000,
  "model": "gpt-4o"
}
```

**响应示例**

```json
{
  "choices": [
    {
      "finish_reason": "stop",
      "index": 0,
      "logprobs": null,
      "message": {
        "content": "The capital of France is Paris.",
        "role": "assistant"
      }
    }
  ],
  "created": 1728131051,
  "id": "chatcmpl-AEy7PU2JImdsD1W6Jw8GigZSEnM2u",
  "model": "gpt-4o-2024-08-06",
  "object": "chat.completion",
  "system_fingerprint": "fp_67802d9a6d",
  "usage": {
    "completion_tokens": 7,
    "prompt_tokens": 24,
    "total_tokens": 31
  }
}
```

**文本向量请求示例**

```json
{
  "input": ["first phrase", "second phrase", "third phrase"],
  "model": "text-embedding-3-large"
}
```

响应示例：

```json
{
  "object": "list",
  "data": [
    {
      "object": "embedding",
      "index": 0,
      "embedding": [
        -0.0012583479,
        0.0020349282,
        ...
        0.012051377,
        -0.0053306012,
        0.0060688322
      ]
    }
  ],
  "model": "text-embedding-3-large",
  "usage": {
    "prompt_tokens": 6,
    "total_tokens": 6
  }
}
```

### 使用 OpenAI 协议代理 360 智脑服务

**配置信息**

```yaml
provider:
  type: ai360
  apiTokens:
    - 'YOUR_360_API_TOKEN'
  modelMapping:
    'gpt-4o': '360gpt-turbo-responsibility-8k'
    'gpt-4': '360gpt2-pro'
    'gpt-3.5': '360gpt-turbo'
    'text-embedding-3-small': 'embedding_s1_v1.2'
    '*': '360gpt-pro'
```

**请求示例**

```json
{
  "model": "gpt-4o",
  "messages": [
    {
      "role": "system",
      "content": "你是一个专业的开发人员！"
    },
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ]
}
```

**响应示例**

```json
{
  "choices": [
    {
      "message": {
        "role": "assistant",
        "content": "你好，我是360智脑，一个大型语言模型。我可以帮助回答各种问题、提供信息、进行对话等。有什么可以帮助你的吗？"
      },
      "finish_reason": "",
      "index": 0
    }
  ],
  "created": 1724257207,
  "id": "5e5c94a2-d989-40b5-9965-5b971db941fe",
  "model": "360gpt-turbo",
  "object": "",
  "usage": {
    "completion_tokens": 33,
    "prompt_tokens": 24,
    "total_tokens": 57
  },
  "messages": [
    {
      "role": "system",
      "content": "你是一个专业的开发人员！"
    },
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "context": null
}
```

**文本向量请求示例**

URL: <http://your-domain/v1/embeddings>

请求示例：

```json
{
  "input": ["你好"],
  "model": "text-embedding-3-small"
}
```

响应示例：

```json
{
  "data": [
    {
      "embedding": [
        -0.011237,
        -0.015433,
        ...,
        -0.028946,
        -0.052778,
        0.003768,
        -0.007917,
        -0.042201
      ],
      "index": 0,
      "object": ""
    }
  ],
  "model": "embedding_s1_v1.2",
  "object": "",
  "usage": {
    "prompt_tokens": 2,
    "total_tokens": 2
  }
}
```

### 使用 OpenAI 协议代理 Cloudflare Workers AI 服务

**配置信息**

```yaml
provider:
  type: cloudflare
  apiTokens:
    - 'YOUR_WORKERS_AI_API_TOKEN'
  cloudflareAccountId: 'YOUR_CLOUDFLARE_ACCOUNT_ID'
  modelMapping:
    '*': '@cf/meta/llama-3-8b-instruct'
```

**请求示例**

```json
{
  "model": "gpt-3.5",
  "max_tokens": 1024,
  "messages": [
    {
      "role": "user",
      "content": "Who are you?"
    }
  ]
}
```

**响应示例**

```json
{
  "id": "id-1720367803430",
  "object": "chat.completion",
  "created": 1720367803,
  "model": "@cf/meta/llama-3-8b-instruct",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "I am LLaMA, an AI assistant developed by Meta AI that can understand and respond to human input in a conversational manner. I'm not a human, but a computer program designed to simulate conversation and answer questions to the best of my knowledge. I can be used to generate text on a wide range of topics, from science and history to entertainment and culture.\n\nI'm a large language model, which means I've been trained on a massive dataset of text from the internet and can generate human-like responses. I can understand natural language and respond accordingly, making me suitable for tasks such as:\n\n* Answering questions on various topics\n* Generating text based on a given prompt\n* Translating text from one language to another\n* Summarizing long pieces of text\n* Creating chatbot dialogues\n\nI'm constantly learning and improving, so the more conversations I have with users like you, the better I'll become."
      },
      "logprobs": null,
      "finish_reason": "stop"
    }
  ]
}
```

### 使用 OpenAI 协议代理 Spark 服务

**配置信息**

```yaml
provider:
  type: spark
  apiTokens:
    - 'APIKey:APISecret'
  modelMapping:
    'gpt-4o': 'generalv3.5'
    'gpt-4': 'generalv3'
    '*': 'general'
```

**请求示例**

```json
{
  "model": "gpt-4o",
  "messages": [
    {
      "role": "system",
      "content": "你是一名专业的开发人员！"
    },
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "stream": false
}
```

**响应示例**

```json
{
  "id": "cha000c23c6@dx190ef0b4b96b8f2532",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "你好！我是一名专业的开发人员，擅长编程和解决技术问题。有什么我可以帮助你的吗？"
      }
    }
  ],
  "created": 1721997415,
  "model": "generalv3.5",
  "object": "chat.completion",
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 19,
    "total_tokens": 29
  }
}
```

### 使用 OpenAI 协议代理 Gemini 服务

**配置信息**

```yaml
provider:
  type: gemini
  apiTokens:
    - "YOUR_GEMINI_API_TOKEN"
  modelMapping:
    "*": "gemini-pro"
  geminiSafetySetting:
    "HARM_CATEGORY_SEXUALLY_EXPLICIT" :"BLOCK_NONE"
    "HARM_CATEGORY_HATE_SPEECH" :"BLOCK_NONE"
    "HARM_CATEGORY_HARASSMENT" :"BLOCK_NONE"
    "HARM_CATEGORY_DANGEROUS_CONTENT" :"BLOCK_NONE"
```

**请求示例**

```json
{
  "model": "gpt-3.5",
  "messages": [
    {
      "role": "user",
      "content": "Who are you?"
    }
  ],
  "stream": false
}
```

**响应示例**

```json
{
  "id": "chatcmpl-b010867c-0d3f-40ba-95fd-4e8030551aeb",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "I am a large multi-modal model, trained by Google. I am designed to provide information and answer questions to the best of my abilities."
      },
      "finish_reason": "stop"
    }
  ],
  "created": 1722756984,
  "model": "gemini-pro",
  "object": "chat.completion",
  "usage": {
    "prompt_tokens": 5,
    "completion_tokens": 29,
    "total_tokens": 34
  }
}
```

### 使用 OpenAI 协议代理 DeepL 文本翻译服务

**配置信息**

```yaml
provider:
  type: deepl
  apiTokens:
    - 'YOUR_DEEPL_API_TOKEN'
  targetLang: 'ZH'
```

**请求示例**
此处 `model` 表示 DeepL 的服务类型，只能填 `Free` 或 `Pro`。`content` 中设置需要翻译的文本；在 `role: system` 的 `content` 中可以包含可能影响翻译但本身不会被翻译的上下文，例如翻译产品名称时，可以将产品描述作为上下文传递，这种额外的上下文可能会提高翻译的质量。

```json
{
  "model": "Free",
  "messages": [
    {
      "role": "system",
      "content": "money"
    },
    {
      "content": "sit by the bank"
    },
    {
      "content": "a bank in China"
    }
  ]
}
```

**响应示例**

```json
{
  "choices": [
    {
      "index": 0,
      "message": { "name": "EN", "role": "assistant", "content": "坐庄" }
    },
    {
      "index": 1,
      "message": { "name": "EN", "role": "assistant", "content": "中国银行" }
    }
  ],
  "created": 1722747752,
  "model": "Free",
  "object": "chat.completion",
  "usage": {}
}
```

### 使用 OpenAI 协议代理 Together-AI 服务

**配置信息**

```yaml
provider:
  type: together-ai
  apiTokens:
    - 'YOUR_TOGETHER_AI_API_TOKEN'
  modelMapping:
    '*': 'Qwen/Qwen2.5-72B-Instruct-Turbo'
```

**请求示例**

```json
{
  "model": "Qwen/Qwen2.5-72B-Instruct-Turbo",
  "messages": [
    {
      "role": "user",
      "content": "Who are you?"
    }
  ]
}
```

**响应示例**

```json
{
  "id": "8f5809d54b73efac",
  "object": "chat.completion",
  "created": 1734785851,
  "model": "Qwen/Qwen2.5-72B-Instruct-Turbo",
  "prompt": [],
  "choices": [
    {
      "finish_reason": "eos",
      "seed": 12830868308626506000,
      "logprobs": null,
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "I am Qwen, a large language model created by Alibaba Cloud. I am designed to assist users in generating various types of text, such as articles, stories, poems, and more, as well as answering questions and providing information on a wide range of topics. How can I assist you today?",
        "tool_calls": []
      }
    }
  ],
  "usage": {
    "prompt_tokens": 33,
    "completion_tokens": 61,
    "total_tokens": 94
  }
}
```

### 使用 OpenAI 协议代理 Dify 服务

**配置信息**

```yaml
provider:
  type: dify
  apiTokens:
    - 'YOUR_DIFY_API_TOKEN'
  modelMapping:
    '*': 'dify'
```

**请求示例**

```json
{
  "model": "gpt-4-turbo",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "stream": false
}
```

**响应示例**

```json
{
  "id": "e33fc636-f9e8-4fae-8d5e-fbd0acb09401",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "你好！我是ChatGPT，由OpenAI开发的人工智能语言模型。我可以帮助回答问题、提供建议或进行各种对话。如果你有任何需要，随时告诉我哦！"
      },
      "finish_reason": "stop"
    }
  ],
  "created": 1736657752,
  "model": "dify",
  "object": "chat.completion",
  "usage": {
    "prompt_tokens": 16,
    "completion_tokens": 243,
    "total_tokens": 259
  }
}
```

### 使用 OpenAI 协议代理 Google Vertex 服务（标准模式）

**配置信息**

```yaml
provider:
  type: vertex
  vertexAuthKey: |
    {
      "type": "service_account",
      "project_id": "your-project-id",
      "private_key_id": "your-private-key-id",
      "private_key": "-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n",
      "client_email": "your-service-account@your-project.iam.gserviceaccount.com",
      "token_uri": "https://oauth2.googleapis.com/token"
    }
  vertexRegion: us-central1
  vertexProjectId: your-project-id
  vertexAuthServiceName: your-auth-service-name
```

**请求示例**

```json
{
  "model": "gemini-2.0-flash-001",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "stream": false
}
```

**响应示例**

```json
{
  "id": "chatcmpl-0000000000000",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "你好！我是 Vertex AI 提供的 Gemini 模型，由 Google 开发的人工智能助手。我可以回答问题、提供信息和帮助完成各种任务。有什么我可以帮您的吗？"
      },
      "finish_reason": "stop"
    }
  ],
  "created": 1729986750,
  "model": "gemini-2.0-flash-001",
  "object": "chat.completion",
  "usage": {
    "prompt_tokens": 15,
    "completion_tokens": 43,
    "total_tokens": 58
  }
}
```

### 使用 OpenAI 协议代理 Google Vertex 服务（Express Mode）

Express Mode 是 Vertex AI 的简化访问模式，只需 API Key 即可快速开始使用。

**配置信息**

```yaml
provider:
  type: vertex
  apiTokens:
    - "YOUR_API_KEY"
```

**请求示例**

```json
{
  "model": "gemini-2.5-flash",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "stream": false
}
```

**响应示例**

```json
{
  "id": "chatcmpl-0000000000000",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "你好！我是 Gemini，由 Google 开发的人工智能助手。有什么我可以帮您的吗？"
      },
      "finish_reason": "stop"
    }
  ],
  "created": 1729986750,
  "model": "gemini-2.5-flash",
  "object": "chat.completion",
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 25,
    "total_tokens": 35
  }
}
```

### 使用 OpenAI 协议代理 Google Vertex 服务（OpenAI 兼容模式）

OpenAI 兼容模式使用 Vertex AI 的 OpenAI-compatible Chat Completions API，请求和响应都使用 OpenAI 格式，无需进行协议转换。

**配置信息**

```yaml
provider:
  type: vertex
  vertexOpenAICompatible: true
  vertexAuthKey: |
    {
      "type": "service_account",
      "project_id": "your-project-id",
      "private_key_id": "your-private-key-id",
      "private_key": "-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n",
      "client_email": "your-service-account@your-project.iam.gserviceaccount.com",
      "token_uri": "https://oauth2.googleapis.com/token"
    }
  vertexRegion: us-central1
  vertexProjectId: your-project-id
  vertexAuthServiceName: your-auth-service-name
  modelMapping:
    "gpt-4": "gemini-2.0-flash"
    "*": "gemini-1.5-flash"
```

**请求示例**

```json
{
  "model": "gpt-4",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "stream": false
}
```

**响应示例**

```json
{
  "id": "chatcmpl-abc123",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "你好！我是由 Google 开发的 Gemini 模型。我可以帮助回答问题、提供信息和进行对话。有什么我可以帮您的吗？"
      },
      "finish_reason": "stop"
    }
  ],
  "created": 1729986750,
  "model": "gemini-2.0-flash",
  "object": "chat.completion",
  "usage": {
    "prompt_tokens": 12,
    "completion_tokens": 35,
    "total_tokens": 47
  }
}
```

### 使用 OpenAI 协议代理 Google Vertex 图片生成服务

Vertex AI 支持使用 Gemini 模型进行图片生成。通过 ai-proxy 插件，可以使用 OpenAI 的 `/v1/images/generations` 接口协议来调用 Vertex AI 的图片生成能力。

**配置信息**

```yaml
provider:
  type: vertex
  apiTokens:
    - "YOUR_API_KEY"
  modelMapping:
    "dall-e-3": "gemini-2.0-flash-exp"
  geminiSafetySetting:
    HARM_CATEGORY_HARASSMENT: "OFF"
    HARM_CATEGORY_HATE_SPEECH: "OFF"
    HARM_CATEGORY_SEXUALLY_EXPLICIT: "OFF"
    HARM_CATEGORY_DANGEROUS_CONTENT: "OFF"
```

**使用 curl 请求**

```bash
curl -X POST "http://your-gateway-address/v1/images/generations" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-2.0-flash-exp",
    "prompt": "一只可爱的橘猫在阳光下打盹",
    "size": "1024x1024"
  }'
```

**使用 OpenAI Python SDK**

```python
from openai import OpenAI

client = OpenAI(
    api_key="any-value",  # 可以是任意值，认证由网关处理
    base_url="http://your-gateway-address/v1"
)

response = client.images.generate(
    model="gemini-2.0-flash-exp",
    prompt="一只可爱的橘猫在阳光下打盹",
    size="1024x1024",
    n=1
)

# 获取生成的图片（base64 编码）
image_data = response.data[0].b64_json
print(f"Generated image (base64): {image_data[:100]}...")
```

**响应示例**

```json
{
  "created": 1729986750,
  "data": [
    {
      "b64_json": "iVBORw0KGgoAAAANSUhEUgAABAAAAAQACAIAAADwf7zUAAAA..."
    }
  ],
  "usage": {
    "total_tokens": 1356,
    "input_tokens": 13,
    "output_tokens": 1120
  }
}
```

**支持的尺寸参数**

Vertex AI 支持的宽高比（aspectRatio）：`1:1`、`3:2`、`2:3`、`3:4`、`4:3`、`4:5`、`5:4`、`9:16`、`16:9`、`21:9`

Vertex AI 支持的分辨率（imageSize）：`1k`、`2k`、`4k`

| OpenAI size 参数 | Vertex AI aspectRatio | Vertex AI imageSize |
|------------------|----------------------|---------------------|
| 256x256          | 1:1                  | 1k                  |
| 512x512          | 1:1                  | 1k                  |
| 1024x1024        | 1:1                  | 1k                  |
| 1792x1024        | 16:9                 | 2k                  |
| 1024x1792        | 9:16                 | 2k                  |
| 2048x2048        | 1:1                  | 2k                  |
| 4096x4096        | 1:1                  | 4k                  |
| 1536x1024        | 3:2                  | 2k                  |
| 1024x1536        | 2:3                  | 2k                  |
| 1024x768         | 4:3                  | 1k                  |
| 768x1024         | 3:4                  | 1k                  |
| 1280x1024        | 5:4                  | 1k                  |
| 1024x1280        | 4:5                  | 1k                  |
| 2560x1080        | 21:9                 | 2k                  |

**注意事项**

- 图片生成使用 Gemini 模型（如 `gemini-2.0-flash-exp`、`gemini-3-pro-image-preview`），不同模型的可用性可能因区域而异
- 返回的图片数据为 base64 编码格式（`b64_json`）
- 可以通过 `geminiSafetySetting` 配置内容安全过滤级别
- 如果需要使用模型映射（如将 `dall-e-3` 映射到 Gemini 模型），可以配置 `modelMapping`

### 使用 OpenAI 协议代理 AWS Bedrock 服务

对于 Bedrock，`/v1/chat/completions` 会继续转换为 Bedrock Runtime Converse API；`/v1/messages` 会直接转发到 Bedrock Mantle 的 Anthropic Messages API：`https://bedrock-mantle.{awsRegion}.api.aws/anthropic/v1/messages`，请求体、响应体和流式 SSE 都保持 Anthropic 原生格式，仅执行模型映射和认证处理。使用 `apiTokens` 访问 Mantle 时，插件会写入 `x-api-key` 请求头。

AWS Bedrock 支持两种认证方式：

#### 方式一：使用 AWS Access Key/Secret Key 认证（AWS Signature V4）

**配置信息**

```yaml
provider:
  type: bedrock
  awsAccessKey: "YOUR_AWS_ACCESS_KEY_ID"
  awsSecretKey: "YOUR_AWS_SECRET_ACCESS_KEY"
  awsRegion: "us-east-1"
  bedrockAdditionalFields:
    top_k: 200
```

#### 方式二：使用 Bearer Token 认证（适用于 IAM Identity Center 等场景）

**配置信息**

```yaml
provider:
  type: bedrock
  apiTokens:
    - "YOUR_AWS_BEARER_TOKEN"
  awsRegion: "us-east-1"
  bedrockAdditionalFields:
    top_k: 200
```

**请求示例**

```json
{
  "model": "us.anthropic.claude-3-5-haiku-20241022-v1:0",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "stream": false
}
```

**响应示例**

```json
{
  "id": "dc5812e2-6a62-49d6-829e-5c327b15e4e2",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "你好!我是Claude,一个由Anthropic开发的AI助手。很高兴认识你!我的目标是以诚实、有益且有意义的方式与人类交流。我会尽力提供准确和有帮助的信息,同时保持诚实和正直。请问我今天能为你做些什么呢?"
      },
      "finish_reason": "stop"
    }
  ],
  "created": 1749657608,
  "model": "arn:aws:bedrock:us-west-2::foundation-model/anthropic.claude-3-5-haiku-20241022-v1:0",
  "object": "chat.completion",
  "usage": {
    "prompt_tokens": 16,
    "completion_tokens": 101,
    "total_tokens": 117
  }
}
```

### 使用 OpenAI 协议代理 NVIDIA Triton Interference Server 服务

**配置信息**

```yaml
providers:
  - type: triton
    tritonDomain: <LOCAL_TRITON_DOMAIN>
    tritonModelVersion: <MODEL_VERSION>
    apiTokens:
      - "****"
    modelMapping:
      "*": gpt2
```

**请求示例**

```json
{
  "model": "gpt2",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ],
  "stream": false
}
```

**响应示例**

```json
{
    "choices": [
        {
            "index": 0,
            "message": {
                "role": "assistant",
                "content": "我是一个AI模型"
            },
            "finish_reason": "stop",
        }
    ],
    "model": "gpt2",
}
```

### 使用上下文清理命令

配置上下文清理命令后，用户可以通过发送特定消息来主动清理对话历史，实现"重新开始对话"的效果。

**配置信息**

```yaml
provider:
  type: qwen
  apiTokens:
    - "YOUR_QWEN_API_TOKEN"
  modelMapping:
    "*": "qwen-turbo"
  contextCleanupCommands:
    - "清理上下文"
    - "/clear"
    - "重新开始"
    - "新对话"
```

**请求示例**

当用户发送包含清理命令的请求时：

```json
{
  "model": "gpt-3",
  "messages": [
    {
      "role": "system",
      "content": "你是一个助手"
    },
    {
      "role": "user",
      "content": "你好"
    },
    {
      "role": "assistant",
      "content": "你好！有什么可以帮助你的？"
    },
    {
      "role": "user",
      "content": "今天天气怎么样"
    },
    {
      "role": "assistant",
      "content": "抱歉，我无法获取实时天气信息。"
    },
    {
      "role": "user",
      "content": "清理上下文"
    },
    {
      "role": "user",
      "content": "现在开始新话题，介绍一下你自己"
    }
  ]
}
```

**实际发送给 AI 服务的请求**

插件会自动清理"清理上下文"命令及之前的所有非 system 消息：

```json
{
  "model": "qwen-turbo",
  "messages": [
    {
      "role": "system",
      "content": "你是一个助手"
    },
    {
      "role": "user",
      "content": "现在开始新话题，介绍一下你自己"
    }
  ]
}
```

**说明**

- 清理命令必须完全匹配配置的字符串，部分匹配不会触发清理
- 当存在多个清理命令时，只处理最后一个匹配的命令
- 清理会保留所有 system 消息，删除命令及之前的 user、assistant、tool 消息
- 清理命令之后的所有消息都会保留

## 完整配置示例

### Kubernetes 示例

以下以使用 OpenAI 协议代理 Groq 服务为例，展示完整的插件配置示例。

```yaml
apiVersion: extensions.higress.io/v1alpha1
kind: WasmPlugin
metadata:
  name: ai-proxy-groq
  namespace: higress-system
spec:
  matchRules:
    - config:
        provider:
          type: groq
          apiTokens:
            - 'YOUR_API_TOKEN'
      ingress:
        - groq
  url: oci://higress-registry.cn-hangzhou.cr.aliyuncs.com/plugins/ai-proxy:1.0.0
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  annotations:
    higress.io/backend-protocol: HTTPS
    higress.io/destination: groq.dns
    higress.io/proxy-ssl-name: api.groq.com
    higress.io/proxy-ssl-server-name: 'on'
  labels:
    higress.io/resource-definer: higress
  name: groq
  namespace: higress-system
spec:
  ingressClassName: higress
  rules:
    - host: <YOUR-DOMAIN>
      http:
        paths:
          - backend:
              resource:
                apiGroup: networking.higress.io
                kind: McpBridge
                name: default
            path: /
            pathType: Prefix
---
apiVersion: networking.higress.io/v1
kind: McpBridge
metadata:
  name: default
  namespace: higress-system
spec:
  registries:
    - domain: api.groq.com
      name: groq
      port: 443
      type: dns
```

访问示例：

```bash
curl "http://<YOUR-DOMAIN>/v1/chat/completions" -H "Content-Type: application/json" -d '{
  "model": "llama3-8b-8192",
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ]
}'
```

### Docker-Compose 示例

`docker-compose.yml` 配置文件：

```yaml
version: '3.7'
services:
  envoy:
    image: higress-registry.cn-hangzhou.cr.aliyuncs.com/higress/envoy:1.20
    entrypoint: /usr/local/bin/envoy
    # 开启了 debug 级别日志方便调试
    command: -c /etc/envoy/envoy.yaml --component-log-level wasm:debug
    networks:
      - higress-net
    ports:
      - '10000:10000'
    volumes:
      - ./envoy.yaml:/etc/envoy/envoy.yaml
      - ./plugin.wasm:/etc/envoy/plugin.wasm
networks:
  higress-net: {}
```

`envoy.yaml` 配置文件：

```yaml
admin:
  address:
    socket_address:
      protocol: TCP
      address: 0.0.0.0
      port_value: 9901
static_resources:
  listeners:
    - name: listener_0
      address:
        socket_address:
          protocol: TCP
          address: 0.0.0.0
          port_value: 10000
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                '@type': type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                scheme_header_transformation:
                  scheme_to_overwrite: https
                stat_prefix: ingress_http
                # Output envoy logs to stdout
                access_log:
                  - name: envoy.access_loggers.stdout
                    typed_config:
                      '@type': type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog
                # Modify as required
                route_config:
                  name: local_route
                  virtual_hosts:
                    - name: local_service
                      domains: ['*']
                      routes:
                        - match:
                            prefix: '/'
                          route:
                            cluster: claude
                            timeout: 300s
                http_filters:
                  - name: claude
                    typed_config:
                      '@type': type.googleapis.com/udpa.type.v1.TypedStruct
                      type_url: type.googleapis.com/envoy.extensions.filters.http.wasm.v3.Wasm
                      value:
                        config:
                          name: claude
                          vm_config:
                            runtime: envoy.wasm.runtime.v8
                            code:
                              local:
                                filename: /etc/envoy/plugin.wasm
                          configuration:
                            '@type': 'type.googleapis.com/google.protobuf.StringValue'
                            value: | # 插件配置
                              {
                                "provider": {
                                  "type": "claude",                                
                                  "apiTokens": [
                                    "YOUR_API_TOKEN"
                                  ]                  
                                }
                              }
                  - name: envoy.filters.http.router
  clusters:
    - name: claude
      connect_timeout: 30s
      type: LOGICAL_DNS
      dns_lookup_family: V4_ONLY
      lb_policy: ROUND_ROBIN
      load_assignment:
        cluster_name: claude
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: api.anthropic.com # API 服务地址
                      port_value: 443
      transport_socket:
        name: envoy.transport_sockets.tls
        typed_config:
          '@type': type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
          'sni': 'api.anthropic.com'
```

访问示例：

```bash
curl "http://localhost:10000/v1/chat/completions"  -H "Content-Type: application/json"  -d '{
  "model": "claude-3-opus-20240229",
  "max_tokens": 1024,
  "messages": [
    {
      "role": "user",
      "content": "你好，你是谁？"
    }
  ]
}'
```


## Implementation Notes

Design decisions and hazards (agent guidance, migrated from the
repository-level AGENTS.md — see that file for repo-wide policies).

**Vendored fork** of the Higress `ai-proxy` plugin, copied verbatim from upstream and maintained here. Unlike the other plugins in this repo it is **not** an original implementation — treat upstream as the source of truth for everything except the local patches below. It is the largest plugin by far (~10 MiB wasm) and the only one with sub-packages (`config/`, `provider/`, `util/`, `test/`).

- **The upstream base commit lives in [FORK_BASE](extensions/gpustack-ai-proxy/FORK_BASE)** — machine-readable (`commit=`, `path=`, `upstream_version=`), and it is the single source of truth for "what are we forked from". The per-file attribution headers repeat the same commit; they must agree. Current base: higress [`36f2f1ac5c73`](https://github.com/alibaba/higress/tree/36f2f1ac5c7305c5a46c296c617b45d1fbacb7d3/plugins/wasm-go/extensions/ai-proxy), upstream `VERSION` 2.0.3 → local `2.0.3-patched` (the `-patched` suffix marks the divergence).
- **Re-syncing is a wipe-and-replay, not a merge.** Delete the vendored tree, re-extract `git archive <commit> plugins/wasm-go/extensions/ai-proxy`, then replay the divergences in order: drop the four hgctl files, vendor `plugins/wasm-go/pkg/pathutil` and `plugins/wasm-go/pkg/jsonbody` (stdlib-only helpers ai-proxy now depends on; upstream wires them via `go.mod` replace directives, which cannot resolve in this repo's layout — copy their `.go` files to `upstream/<name>/` as plain packages, drop their `go.mod`, and let the module-path rewrite point the imports at them), rewrite the module path, set `pluginName`, insert attribution headers immediately before each `package` line, suffix `VERSION`, re-insert the three README fork notices, re-apply the `demoteInlineSystemMessages` patch, the deepseek `ApiNameResponses` capability, the Anthropic entries of the `FromJson` capability whitelist, the `stripClaudeInternalMessageFields` O(K) rewrite, the `enforceToolCallPairing` call in `onHttpRequestBody` and the openai `max_completion_tokens` rewrite, the qwen `cohere/v1/rerank` capability and the qwen `openai/v1/imagegeneration` capability, re-apply the PR-#53 upstream hardening fixes (the SSE `data:` prefix guards in gemini/dify/minimax/spark/triton, the path-segment boundary in `prependBasePath`/`applyProviderBasePath`, and the object-start gate in the vendored `jsonbody`), re-apply the video passthrough patch (divergence 12: the `/v1/videos/sync` `""` classification, the `DontReadResponseBody` early return for video content, and the no-failover branch at the top of `OnRequestFailed`), restore the GPUStack-local files that have no upstream counterpart at all, rewrite `FORK_BASE`, `gofmt -w`. Verify by normalising the local tree back (strip headers, reverse the path rewrite — including the `upstream/<name>` → `pkg/<name>` import mapping — `gofmt` **both** sides) and diffing against the upstream tree: the only real divergence must be `claude_to_openai.go`, `claude_to_openai_test.go`, `deepseek.go`, `provider.go`, `provider/openai.go`, `provider_test.go` (the strip-rewrite tests), `gemini.go`, `dify.go`, `minimax.go`, `spark.go`, `triton.go` (the SSE `data:` guards), `basepath_traversal_test.go` (import lines only, from the pkg vendoring), `config/config.go`, the three READMEs, `VERSION`, `go.mod`, `FORK_BASE`, the GPUStack-local files that have no upstream counterpart (`provider/deepseek_test.go`, `provider/capabilities_anthropic_test.go`, `provider/qwen_rerank_capabilities_test.go`, `provider/qwen_image_generation_capabilities_test.go`, `provider/sse_data_prefix_guard_test.go`, `provider/basepath_segment_boundary_test.go`, `upstream/jsonbody/object_start_gate_test.go`, `provider/max_completion_tokens.go`, `provider/max_completion_tokens_test.go`, `provider/max_completion_tokens_bench_test.go`, `openai_max_tokens_integration_test.go`, `tool_call_pairing.go`, `tool_call_pairing_test.go`, `tool_call_pairing_bench_test.go`, `metrics.go`, `config/tool_call_validation.go`, `config/tool_call_validation_test.go`), the vendored `upstream/` packages themselves, plus `main.go` and a header-only diff in `test/provider_wasm_smoke.go` (both headers sit after a pre-existing leading comment, so a `head -1` normaliser misses them; `main.go` additionally carries the 4-line `enforceToolCallPairing` hook). `wasm-unit-test.wasm` in the plugin root is an untracked, git-ignored test-run artifact — do not mistake it for a divergence.

- **Functional divergence 1 — `mergeSystemMessages` in [claude_to_openai.go](extensions/gpustack-ai-proxy/provider/claude_to_openai.go)** (gpustack/gpustack#5934). Claude clients (notably Claude Code) send a system-role message *inside* the `messages` array **in addition to** Claude's top-level `system` field. Upstream's converter prepends the top-level `system` and leaves the embedded one where it was, producing two system messages with one of them not at index 0 — strict OpenAI-compatible backends (vLLM) then reject the request with `System message must be at the beginning.`. The patch collapses every system-role message into a single leading one: 0 → unchanged, 1 → preserved verbatim but moved to front, ≥2 → contents concatenated into one block array. `systemContentBlocks` normalises `string` vs `[]chatMessageContent` content so block-form system messages keep their `cache_control`.
- **Functional divergence 2 — `ApiNameResponses` in the deepseek provider's `DefaultCapabilities` ([deepseek.go](extensions/gpustack-ai-proxy/provider/deepseek.go))**. DeepSeek serves the OpenAI Responses API natively ([docs](https://api-docs.deepseek.com/zh-cn/guides/responses_api)), so this is a one-line path mapping, not a converter — everything else rides the generic path: no `TransformRequestBody` override (model mapping via `defaultTransformRequestBody`), no `GetApiName` override (main.go's `pathSuffixToApiName` already resolves `/v1/responses`), and `ApiNameResponses` is already in `isStreamingAPI` / `needToProcessRequestBody`. Notably **no request-body scrubbing** as doubao does for its unsupported `parallel_tool_calls` / `tool_choice`: DeepSeek documents that unsupported params are *silently ignored* (`静默忽略`) rather than rejected, so deleting them would only lose fidelity. The mapped path is `PathOpenAIResponses` (`/v1/responses`) — DeepSeek's docs show `base_url` without `/v1`, but `/v1` is its OpenAI-compat alias for the same endpoint (both answer 401, not 404, on a bad key) and it is the shape `ApiNameChatCompletion` / `ApiNameModels` already use in this same map. Pinned by [deepseek_test.go](extensions/gpustack-ai-proxy/provider/deepseek_test.go) (GPUStack-local, no upstream counterpart) because there is no conversion fallback: a dropped capability makes `OnRequestBody` return `errUnsupportedApiName` — a hard failure, not a degraded response.
- **Functional divergence 3 — the three Anthropic surfaces in the `capabilities` whitelist of `ProviderConfig.FromJson` ([provider.go](extensions/gpustack-ai-proxy/provider/provider.go))**. Upstream's `switch` drops any capability key it doesn't recognise, and `anthropic/v1/messages` (plus `count_tokens` / `complete`) was not in the list — so an operator could declare it and be silently ignored. That is not a cosmetic path-mapping loss: main.go's `onHttpRequestHeaders` rewrites an inbound `/v1/messages` to `/v1/chat/completions` and sets `needClaudeResponseConversion` **exactly when** `IsSupportedAPI(ApiNameAnthropicMessages)` is false, so a dropped key means the request gets protocol-converted anyway — indistinguishable from never having configured it, with no error and no log. Whitelisting the three keys lets any provider type that serves Claude natively say so from configuration instead of requiring a provider type that hardcodes them in `DefaultCapabilities`; capabilities are a top-up over `setDefaultCapabilities`, not a replacement, which is what makes this preferable to swapping `provider.type`. Pinned by [capabilities_anthropic_test.go](extensions/gpustack-ai-proxy/provider/capabilities_anthropic_test.go) (GPUStack-local, no upstream counterpart).
- **Functional divergence 4 — the `toolCallValidation` tool/tool_calls pairing check** (gpustack/gpustack#6210). Lives entirely in GPUStack-local files ([tool_call_pairing.go](extensions/gpustack-ai-proxy/tool_call_pairing.go), [metrics.go](extensions/gpustack-ai-proxy/metrics.go), [config/tool_call_validation.go](extensions/gpustack-ai-proxy/config/tool_call_validation.go)); the only edits to vendored files are a 4-line hook at the top of `onHttpRequestBody` and the field/getter/Validate wiring in `config/config.go`. A malformed `tool` / `assistant.tool_calls` chain is provider-dependent today: strict backends 400, permissive ones return **200 with a fabricated tool result** and `finish_reason: "stop"`, which the caller cannot detect. `request-validation` cannot express it (JSON Schema has no cross-element dependency), so it lands here, in the component that already parses `tool_calls`.
  - **Reject-and-alert, never repair.** No synthesised tool results, no de-duplicated ids: the gateway does not know the business semantics and a wrong repair is worse than none.
  - **`validateToolCallPairing` is pure** (gjson only, no host calls) so it is unit-tested without a proxy-wasm host — [tool_call_pairing_test.go](extensions/gpustack-ai-proxy/tool_call_pairing_test.go) covers the issue's four malformed shapes plus its two controls. `toolCallPairingDecision` is the same idea one level up: it takes `(mode, apiName, claudeConverted, body)` and returns the violation, so **every gate is testable too** and the only untested code left in `enforceToolCallPairing` is the `SendHttpResponseWithDetail` call itself. Don't inline it back into the I/O shell.
  - **Both ids must be non-empty JSON *strings*** (`nonEmptyJSONString`). `gjson.Result.String()` renders a missing field and a `null` as `""` and a number as its digits — on *both* sides of the comparison. Without the type check, an assistant call with no `id` followed by a tool message with no `tool_call_id` pairs `"" == ""` and the request passes, which is precisely the malformed history the check exists to stop.
  - **Every id that reaches `toolPairingViolation.Detail` must be escaped** (`%q`, or `strconv.Quote` inside `joinPending`). `nonEmptyJSONString` returns the *unescaped* JSON value, so `{"id": "a\nb"}` arrives as a Go string containing a real newline, and `Detail` is logged with `%s` — appended raw, a caller could inject whole forged WARN lines into a line-oriented log pipeline. `TestViolationDetailEscapesControlCharactersInIDs` fails if any id-bearing diagnostic drops the escaping. (This is the opposite call from the rejection *body*, where `%q` was rejected: `%q` is Go string-literal escaping, correct for a log line and wrong for JSON.)
  - **`pending` is a set, not a slice.** A single assistant message may carry an attacker-chosen number of parallel calls and the body limit is 100 MiB, so linear scan-and-shift per tool result would be O(k²) and could pin an Envoy worker even though the parse itself is cheap. `pendingOrder` exists only so the rejection message lists ids deterministically, and is filtered once on the error path.
  - **It must stay a gjson scan, never an `encoding/json` decode** (gpustack/gpustack#6217). The Anthropic→OpenAI converter's reflection-based decode of the whole document is what burns ~3 s of Envoy worker CPU on a 1.2 MB / 200-round Claude Code history and saturates the gateway; this check runs on the *other* path (`/v1/chat/completions`, p50 0.29 s in that reproduction) so any comparable cost would be a much worse regression there. [tool_call_pairing_bench_test.go](extensions/gpustack-ai-proxy/tool_call_pairing_bench_test.go) pins the walk against a bare `json.Unmarshal` of the same body as a guard — native Apple M4 Pro, 200 rounds / 1.2 MB: 0.62 ms / 233 allocs vs 3.94 ms / 3422 allocs.
  - **`parseBodyNoCopy` (`unsafe.String`) is load-bearing, not a micro-optimisation.** `gjson.GetBytes` copies `Result.Raw` out of the byte slice; asking it for the whole `messages` array therefore duplicated ~1.2 MB per request. ai-proxy's own fast path only asks gjson for scalars (`stream`, `model` in `defaultTransformRequestBody`), so that copy would have been new MB-scale per-request pressure introduced by this check. The aliasing contract is documented on the helper: nothing derived from the Result escapes `validateToolCallPairing`, and the caller does not touch `body` until it returns. `gjson.GetBytes` is still correct for the tiny `model` lookup on the rejection path.
  - **Rule 4 (`tool_calls[].id` uniqueness) is enforced request-wide, not per-array.** Stricter than the issue's wording, but forced by rule 3: two assistant turns reusing an id make the answering `tool_call_id`s repeat, which rule 3 rejects anyway — so the choice is only *where* the error is reported, and the issuing site is the actionable one. Reverting it to per-array would make the cross-turn case fail as `duplicate_tool_call_id` on the tool message instead, pointing at the wrong place.
  - **Gated on `apiName == ApiNameChatCompletion` *and* `!needsClaudeResponseConversion(ctx)`.** A Claude request rewritten onto `/v1/chat/completions` carries a chat-completions apiName but a Claude-shaped body; the walk would find no `role:"tool"` messages and pass by accident. The explicit skip says so on purpose. Anthropic's `tool_use`/`tool_result` blocks are out of scope.
  - **The check runs on the inbound body**, before `ReplaceByCustomSettings` and before the provider's `TransformRequestBody`, so the 400 describes what the client actually sent.
  - **Rejection returns `types.ActionPause` after `SendHttpResponseWithDetail`**, matching Higress `request-validation`'s body-phase shape — *not* the `ActionContinue` that the neighbouring `util.ErrorHandler` path uses, which is the "body dropped, `bytes_sent=0`" hazard documented under gpustack-rate-limit.
  - **Two modes only — `off` (default) and `strict`** (`ToolCallValidationDefault`). Deliberate, and a deviation from the issue's "default true" and its `strict`/`warn-only`/`off` tiering: confirmed with the requester, only one deployment wants the check. This is a vendored fork that ships to every GPUStack deployment, so any default that rejects would make the fork unilaterally turn today's working requests into fleet-wide 400s. A `warn` tier was implemented and then dropped — it would have run the walk on every chat-completions request fleet-wide to collect telemetry nobody asked for. Off means the very first line of `enforceToolCallPairing` returns, so an unconfigured plugin pays literally nothing. Kept as a string enum rather than a boolean so re-adding `warn` later stays non-breaking; `config_test` pins that a stale `warn` value falls back to the default rather than silently behaving like strict.
  - Because there is no "detected but forwarded" mode, the metric is `..._pairing_rejected_total` with no `action` label — every violation the walk sees is a 400.
  - **No request-controlled value may become a metric label here.** The stat carries only `route_name`, `cluster_name` and `rule`; the `model.` / `consumer.` slots are kept (Higress's stats_tags regexes match on those literal separators to extract `ai_route` / `ai_cluster`) but filled with the `none` sentinel. A rejected request is cheap to send — 400, no upstream call — so a label a caller can pick freely (`model` from the body; `x-mse-consumer` on any route without key-auth in front) would mint a new Envoy stat and a permanent `metricCounters` entry per request. The API-key attribution the issue asked for lives on the WARN log line instead, which is append-only. Note `gpustack-rate-limit` does put `model`/`consumer` in its stat names — same exposure, pre-existing; don't copy it here.
  - **Ordering in `enforceToolCallPairing`: send first, log and count after.** The one thing read before `SendHttpResponseWithDetail` is the `x-mse-consumer` header, because a header read is not guaranteed to still resolve once the local reply is queued and that attribution is the point of the log line. That leaves exactly one hostcall in the window — fewer than ai-proxy's own 413 path. (The rate-limit "hostcall-free window" hazard in this file is documented for the *async* redis-callback shape; this is a synchronous body-phase reply, the shape Higress `request-validation` uses. Kept tight anyway because it costs nothing.)
  - **An unrecognised spelling must NOT be a config error** — it falls back to the default and returns a warning string. wasm-go's `RuleMatcher.ParseRuleConfig` captures `parseGlobalConfig`'s error into a local `globalConfigError` and then *drops it on the floor* whenever any matchRule exists, leaving `m.globalConfig` at its zero value for every rule to inherit. GPUStack's reconciler (`model_ai_proxy_plugin_spec` in gpustack `gateway/utils.py`) puts `providers` **only** in `defaultConfig` and `{"activeProviderId": ...}` in each matchRule — so a zeroed global resolves no provider and ai-proxy silently no-ops every AI request. A typo in this one field would take the whole proxy down.
  - The warning is **returned, not logged, from the config package**: wasm-go's package-level `log.Log` is nil until the host installs it, so any `log.*` call on a config path panics in plain `go test`. `logToolCallValidationWarning` (in `tool_call_pairing.go`) emits it from `parseGlobalConfig` / `parseOverrideRuleConfig`, which already run host-side.
  - `toolCallValidation` is parsed at the **top** of `PluginConfig.FromJson`, before the legacy `provider` branch's early `return`, and only when the key exists so a matchRule override inherits the global value — which is how a hand-edited `defaultConfig.toolCallValidation` reaches every rule, since `ai_proxy_diff_spec` only ever rewrites `defaultConfig["providers"]` and `matchRules` and leaves other `defaultConfig` keys untouched.
- **Functional divergence 5 — the `max_tokens` → `max_completion_tokens` rewrite in the openai provider (call site in [openai.go](extensions/gpustack-ai-proxy/provider/openai.go), helpers in the GPUStack-local [max_completion_tokens.go](extensions/gpustack-ai-proxy/provider/max_completion_tokens.go))** (gpustack/gpustack#6505). Per the OpenAI Chat Completions spec, the o-series and gpt-5+ reasoning models only accept `max_completion_tokens` and reject the deprecated `max_tokens` with an `unsupported_parameter` 400, while legacy models keep accepting `max_tokens` — so callers (playground, user apps) legitimately keep sending `max_tokens` and the *proxy* normalizes per target model instead of every client special-casing model families. `rewriteMaxTokensToMaxCompletionTokens` takes the restricted-families prefix list as a parameter, so another provider hitting the same restriction reuses it with its own list; the openai call sits in `openaiProvider.TransformRequestBody` **after** `defaultTransformRequestBody`, i.e. it judges the **mapped** model name; the OpenAI prefix list (`gpt-5`/`o1`/`o3`/`o4`, last `/`-segment, lowercased) is kept in sync with the GPUStack server's `_get_model_output_token_dict` in `gpustack/routes/model_provider.py`. Field moves are raw sjson writes only (no unmarshal — the fork's per-request-cost discipline), and the token-parameter lookup is a single ForEach pass over `ParseBytes(body)` taken only after the model matched a restricted family: gjson is lazy (`Result.Get` re-scans `t.Raw` per call), so per-key `GetBytes` near-full-body scans for keys sitting after the large `messages` array. The `ParseBytes` + `Result.Get` variant suggested in the plugin PR's bot review measures ~60x slower with +64 KiB/op on the legacy short-circuit path (it parses before the model check); the shipped hybrid is end-to-end ~10-20% faster on restricted-model rewrites and unchanged on legacy — numbers pinned in `max_completion_tokens_bench_test.go` alongside the rejected variants. When `max_completion_tokens` is already present (or `max_tokens` is JSON `null`) the deprecated parameter is *dropped* instead, since sending both is rejected upstream anyway. Scoped to `ApiNameChatCompletion` and to provider type `openai`: other openai-protocol providers (vLLM, DeepSeek, …) still take `max_tokens` per their own specs. Azure OpenAI has the same model-family restriction but addresses models by deployment name, where prefix matching is unreliable, and is deliberately not covered. Pinned by [max_completion_tokens_test.go](extensions/gpustack-ai-proxy/provider/max_completion_tokens_test.go) (pure helpers, incl. a custom-prefix-list case for provider extension) and [openai_max_tokens_integration_test.go](extensions/gpustack-ai-proxy/openai_max_tokens_integration_test.go) (full body pipeline on the in-process wasm host, go + wasm modes — `TransformRequestBody` cannot be unit-tested directly because `defaultTransformRequestBody`'s proxywasm host calls nil-deref outside a VM).
- **Functional divergence 6 — the `cohere/v1/rerank` capability in the qwen provider ([qwen.go](extensions/gpustack-ai-proxy/provider/qwen.go))**. GPUStack exposes rerank on `/v1/rerank` — the de-facto standard Cohere/Jina spelling, spoken by the GPUStack UI, llama-box and vLLM — which `main.go`'s `getApiName` resolves to `ApiNameCohereV1Rerank`, while upstream's qwen `DefaultCapabilities` only declares `qwen/v1/rerank` (the `/v1/reranks` spelling mirroring DashScope's own compatible endpoint path). `qwenProvider.OnRequestBody` therefore rejected the request at `isSupportedAPI` with `unsupported API name` before it ever reached DashScope. The patch adds `ApiNameCohereV1Rerank` → `/compatible-api/v1/reranks` to the **compatible-mode branch only**: that endpoint is itself Cohere/Jina-shaped (`{"model","query","documents","top_n"[,"return_documents"]}` in, `results[].index/relevance_score[/document.text]` out — verified against LiteLLM's `DashScopeRerankConfig` and a live DashScope call: `compatible-api/v1/reranks` accepts the flat body and answers Jina-style `{object,results[],model,id,usage}`; note `gte-rerank-v2` is no longer accepted there, the live test used `qwen3-rerank`), so a path rewrite plus model mapping is the whole conversion and the body/response pass through verbatim (both are passthrough in compatible mode). Known edges deliberately left unhandled, matching the minimal-bridge decision: Cohere-only optional fields (`max_tokens_per_doc`, `relevant_spans`, `priority`, v1's `max_chunks_per_doc`) are forwarded as-is (DashScope's tolerance of unknown fields is unguaranteed; LiteLLM strips them), and `{text}`-object `documents` elements are not flattened. It is absent from the native branch — `/api/v1/services/rerank/text-rerank/text-rerank` expects `input.query`/`input.documents` and cannot parse a Cohere body, so a path rewrite there would trade an explicit gateway error for an upstream 400; a real converter would be needed first. GPUStack's `QwenConfig` defaults `qwenEnableCompatible` to true, so every GPUStack qwen provider takes the compatible branch. Pinned by [qwen_rerank_capabilities_test.go](extensions/gpustack-ai-proxy/provider/qwen_rerank_capabilities_test.go) (GPUStack-local, no upstream counterpart).
- **Functional divergence 7 — the `openai/v1/imagegeneration` capability in the qwen provider ([qwen.go](extensions/gpustack-ai-proxy/provider/qwen.go))**. The Qwen-Image generation-and-editing models (qwen-image-3.0(-pro), 2.1-pro/turbo) are served on a plain **OpenAI Images** surface — `POST /compatible-mode/v1/images/generations`, flat OpenAI body in (the optional `image` extension field switches T2I to I2I editing; `response_format=b64_json` is accepted but ignored, URLs are always returned), standard `{created,data[].url,usage}` out — verified live against a workspace domain. Upstream's qwen provider declares no image-generation capability, so an inbound `/v1/images/generations` died at `isSupportedAPI`. The patch adds `ApiNameImageGeneration` → `/compatible-mode/v1/images/generations` to the **compatible-mode branch only** (pure path rewrite + model mapping; body and response pass through verbatim). **Deployment requirement**: this endpoint exists **only on the per-workspace `maas.aliyuncs.com` domains** — the legacy `dashscope.aliyuncs.com` answers 404 with an empty body for it (while chat/embeddings/reranks still work there) — so `qwenDomain` must be set to the `{WorkspaceId}.cn-beijing.maas.aliyuncs.com`-style domain for image generation; that domain serves every other qwen surface too. Absent from the native branch: the native surface (`/api/v1/services/aigc/multimodal-generation/generation`, the only option on the legacy domain) speaks a nested `input`/`parameters` dialect that a flat OpenAI Images body cannot satisfy. Pinned by [qwen_image_generation_capabilities_test.go](extensions/gpustack-ai-proxy/provider/qwen_image_generation_capabilities_test.go) (GPUStack-local, no upstream counterpart).
- **Functional divergence 8 — hardening fixes on vendored upstream code, from the bot review of PR #53**. Three clusters, all flagged by gemini-code-assist / Copilot on [PR #53](https://github.com/gpustack/gpustack-higress-plugin/pull/53), all real defects in code vendored from upstream 36f2f1ac5c73 — worth fixing locally even though they widen the divergence, and worth offering back upstream:
  - **SSE `data:` prefix guard** in the streaming loops of `gemini.go`, `dify.go`, `minimax.go`, `spark.go` and `triton.go`: the loops stripped a fixed 6-byte prefix from every line of length ≥ 6, so a framed event's non-data fields (`event:`, `id:`, comments) were fed to `json.Unmarshal` as error-log spam. Now `strings.HasPrefix(data, "data:")` + `TrimPrefix`/`TrimSpace` (which also handles `data:` without the space, and keeps spark's `[DONE]` check working). Pinned by [sse_data_prefix_guard_test.go](extensions/gpustack-ai-proxy/provider/sse_data_prefix_guard_test.go) (gemini + spark exercised end-to-end; the other three carry the identical mechanical guard).
  - **Path-segment boundary for base-path prefixing** in `provider.go`: `prependBasePath` and `applyProviderBasePath` decided "already prefixed" with a byte prefix, so basePath `/api` + `/api2/v1/models` was mistaken for prefixed and 400'd (and providerBasePath `/v` silently skipped prefixing `/v2/models`). The check is now `pathPart == base || strings.HasPrefix(pathPart, base+"/")` on the raw path portion, against the **trailing-slash-trimmed** base (`validatePrefix` accepts a single trailing slash on basePath; comparing against the raw value turned `/api/` + `/api/v1/models` into a double prefix). Deliberately *not* `pathutil.WithinPrefix`, which `path.Clean`s first and would reroute already-prefixed dot-segment targets from the `ValidateWithin` branch (reject-on-escape) into `SafeJoin` (suffix-join semantics), silently relaxing upstream's traversal rejection. Pinned by [basepath_segment_boundary_test.go](extensions/gpustack-ai-proxy/provider/basepath_segment_boundary_test.go) alongside upstream's `basepath_traversal_test.go`.
  - **ActionPause propagation for the queued basePath-traversal 400** in `provider.go` + `main.go`: `handleRequestHeaders` queued a local 400 with no way to signal it (void signature), so `OnRequestHeaders` returned nil and the header callback kept iterating (`HeaderStopIteration`/`ActionContinue`) on a stream that already had a reply queued — unlike the 413 rejection in `main.go`, which returns `types.ActionPause`. The rejection now sets a context flag (`ctxKeyQueuedLocalReply`, exported via `HasQueuedLocalReply`) that `main.go` checks right after `OnRequestHeaders` — before the error path, so `util.ErrorHandler` can never queue a second reply on the same stream. Changing the signature instead would touch every provider's `OnRequestHeaders`; the flag keeps the divergence to two files.
  - **Object-start gate in `jsonbody.DuplicateTopLevelMember`** (vendored `upstream/jsonbody/`): the scan started counting at the first `{` anywhere, so a multipart image-edit form whose prompt field quotes JSON text could be rejected as a duplicate `model` member. Bodies whose start (after whitespace) is not `{` now return `(false, nil)`. Pinned by [object_start_gate_test.go](extensions/gpustack-ai-proxy/upstream/jsonbody/object_start_gate_test.go).
- **Every `.go` file carries a 4-line attribution header** naming its exact upstream URL at the fork commit. GPUStack-local files carry a "no upstream counterpart" header instead. Keep it when editing — it is the only way to tell "we changed this" from "upstream looks like this" when re-syncing.
- **Four upstream files were deliberately deleted** because this repo builds with `extensions/Makefile` (plain `go build` → wasm) + `scripts/generate_metadata.py`, never `hgctl`: `option.yaml` (hgctl project config), `Makefile` (upstream's tinygo single-plugin build), `envoy.yaml` (hgctl local-test Envoy config), and `.gitignore` (hgctl's allowlist-style rules, which swallow `VERSION` and fight the root `.gitignore`). `README_dev.md` is upstream's hgctl dev doc, kept only for reference with a fork note prepended — it still describes the deleted `envoy.yaml`.
- `pluginName` is `"gpustack-ai-proxy"`, **not** upstream's `"ai-proxy"`. It flows into log prefixes and `response_code_details=via_wasm::...<pluginName>.<detail>`, so keeping upstream's value would make our fork indistinguishable from a real upstream `ai-proxy` filter in access logs.
- **Upstream `ai-proxy` is no longer fetched in `remote_plugins.yaml`** — same treatment as model-router/model-mapper. This fork is a drop-in replacement (config schema untouched), and shipping both would double ~10 MiB of wasm in the wheel for no benefit.
- `test/` holds upstream's hgctl e2e helper package. Nothing in the build or in `go test ./...` depends on it; it is retained purely to keep the diff against upstream small.
- **Functional divergence 11 — `hoistToolResultImages` in [claude_to_openai.go](extensions/gpustack-ai-proxy/provider/claude_to_openai.go)** (gpustack/gpustack-higress-plugin#65). Anthropic allows `image` blocks inside a `tool_result`'s `content` array (a screenshot-reading tool, a browser tool, …). Upstream's conversion copies the whole `tool_result` block into the tool message's `claude_content_blocks` and builds the message's visible `content` from `GetStringValue()`, which concatenates **text blocks only** — so the pixels existed solely in an internal field that `stripClaudeInternalMessageFields` removes before forwarding. The model never saw them, silently. The fix is a post-pass over the converted messages, because an OpenAI `tool` message's `content` is text-only (strict backends reject an `image_url` array there) and the pixels need somewhere OpenAI-shaped to live: for each contiguous group of `tool` messages, the images are re-encoded with the same `claudeImageToOpenAIContent` helper the top-level `image` case uses (base64 → data URL, `url` → verbatim) and emitted right after the group as a `user` turn, each image preceded by a one-line `Image returned by tool result <tool_use_id>:` label so the association survives. If a `user` message already follows the group — the companion text the Claude turn carried next to the `tool_result`, or the caller's next turn — the images are merged in front of its content via `mergeChatContents` rather than a new message, since several chat templates reject adjacent `user` turns. **Ordering constraints worth keeping straight**: nothing may be inserted *between* the `tool_calls` assistant message and its tool results, which is why the images trail the whole group and not each tool message; inline `system` messages between tool results are passed through by this pass (they do not end the group) and left to `demoteInlineSystemMessages`, which runs after it and merges a demoted reminder into the image turn. Bodies without tool-result images return the input slice untouched — no allocation on the common path; the detection reads `ClaudeContentBlocks`, which the tool message already carries. Pinned by the `tool_result_image_*` subtests in [claude_to_openai_test.go](extensions/gpustack-ai-proxy/provider/claude_to_openai_test.go), including a check that `image_url` survives `stripClaudeInternalMessageFields`. Not handled: `document` blocks inside `tool_result` (same shape of loss, no OpenAI equivalent for a tool turn — would need the same hoist into a `file` part if a caller ever needs it).
- **Functional divergence 9 — upstream 401/403 remapped to 502 in `onHttpResponseHeaders` ([main.go](extensions/gpustack-ai-proxy/main.go), `remapUpstreamAuthErrorStatus`)** (gpustack/gpustack#6646). An upstream inference provider's 401 (e.g. invalid external-provider credentials) passed through verbatim collides with the meaning 401 already has on GPUStack's console API ("management session invalid"), so the UI's global 401 handler cleared the cached user and kicked the user out of a valid session to `#/login`. The fix is gateway-side per the maintainer decision in the issue: in `onHttpResponseHeaders`'s non-200 branch, when the response is from upstream (the `IsResponseFromUpstream()` gate at the top of the callback), no retry is in flight (`OnRequestFailed` returned `ActionContinue` — its retry path returns `HeaderStopAllIterationAndWatermark` and regenerates the response), and the status is `401`/`403`, rewrite `:status` to `502`, drop the upstream's now-meaningless `WWW-Authenticate`, and leave the body verbatim so the upstream error detail stays diagnosable (a `upstream_auth_error_remapped` warn log records provider + original status). Scope: gateway-generated 401/403 never reach this plugin's response phase (authn rejects earlier in the request phase at higher priority); other upstream statuses (400, 429, 5xx) pass through unchanged, as before. Pinned by [upstream_auth_status.go](extensions/gpustack-ai-proxy/test/upstream_auth_status.go) (GPUStack-local, no upstream counterpart).
- **Functional divergence 10 — `requestPrioritySource` / `requestPriorityTarget`** ([provider/request_priority.go](extensions/gpustack-ai-proxy/provider/request_priority.go), hooks in [main.go](extensions/gpustack-ai-proxy/main.go) `onHttpRequestHeader` and [provider.go](extensions/gpustack-ai-proxy/provider/provider.go) `handleRequestBody`) (gpustack/gpustack#6739). GPUStack-local, no upstream counterpart. `gpustack-ext-auth` resolves the caller and writes its request priority as `x-gpustack-request-priority` (non-negative, larger = more urgent, absent = none); this plugin translates it into the active provider's backend form, decided per provider by `requestPriorityTarget`: `vllm` → header `x-vllm-priority = -p` plus body `priority = -p` (vLLM schedules the lower value first; its generation endpoints read the header, which overrides a body `priority`, its pooling endpoints read the body only, so both are written on every request -- 0 included -- and a client-supplied body value is neutralised either way); `sglang` → body `priority = p` (SGLang schedules the higher value first and has no priority header); unset → nothing. The source header is removed whatever the target, so the gateway-internal header never reaches an upstream. Three placement facts worth keeping straight: (1) the header half runs in `onHttpRequestHeader` *before* the provider's `handleRequestHeaders`, which writes the full header set back and therefore preserves it; (2) the body half is injected in `handleRequestBody` **after** `TransformRequestBody` and before both `replaceRequestBody` exits -- the Claude→OpenAI converter rebuilds the body from the Claude schema, which has no `priority`, so an earlier injection would be lost -- plus a second point on the `IsOriginal()` early return, where no other code touches the body, a third after `insertContext` (the `provider.context` system message rebuilds the body from the provider schema, which has no `priority`), a fourth in the `generic` provider's own `OnRequestBody` (it forwards the body outside `handleRequestBody`), and a fifth in `sendRetryRequest` after its header/body transform (the retry replays the pre-phase body with freshly built headers, so the header is re-written there as well). Every existing top-level `priority` member is removed before the write -- in one pass when there are several, since sjson touches one occurrence -- so a body carrying the key twice cannot keep a client-chosen copy for a last-wins decoder. The body is only rewritten when the plugin reads it, so `isSupportedRequestContentType` matches `application/json` case-insensitively and `application/*+json`; (3) the body half is gated to JSON bodies of chat completions / completions / responses / embeddings, because SGLang's Anthropic endpoint (`/v1/messages`, reached when the Model sets `native_anthropic_api`) has no `priority` member and a multipart body is not JSON. The translation is a sign, not a range mapping: `p = 0` lands on each backend's own baseline, and SGLang must therefore NOT run with `--schedule-low-priority-values-first`. A malformed source value (not a non-negative int32; one warning per VM, since the value is client-influenced) or an unrecognised target is a warning plus the default, never a config error, for the reason spelled out under divergence 4 (`toolCallValidation`). Pinned by [request_priority_test.go](extensions/gpustack-ai-proxy/provider/request_priority_test.go) and [request_priority_integration_test.go](extensions/gpustack-ai-proxy/request_priority_integration_test.go) (both GPUStack-local).
- **Functional divergence 12 — video requests are not replayed and video bytes are not buffered ([main.go](extensions/gpustack-ai-proxy/main.go), [provider/path_classifier.go](extensions/gpustack-ai-proxy/provider/path_classifier.go), [provider/failover.go](extensions/gpustack-ai-proxy/provider/failover.go))**. Three small changes that make asynchronous video work behind the generic-proxy-router's self-routing IDs (see that plugin's README). (1) `POST /v1/videos/sync` is classified as `""`, ahead of the `/v1/videos` suffix entry: it returns raw video bytes rather than a task object, so the plugin must neither parse nor rewrite its body in either direction — it is an ordinary model request. (2) `onHttpResponseHeaders` returns early with `DontReadResponseBody` for `ApiNameRetrieveVideoContent`: completed video content is an opaque stream, never JSON or SSE. (3) `OnRequestFailed` returns `ActionContinue` for `ApiNameVideos`, `ApiNameVideoRemix`, `ApiNameRetrieveVideo` and `ApiNameRetrieveVideoContent` before both the apiToken failover and `retryOnFailure`: an async create may have been accepted by the engine even when its response was lost, and a second attempt through another token would submit the job twice. Follow-ups address the job by ID on the instance that owns it, so failing them over to another token/target cannot succeed either. The cost is explicit: a video create whose response was lost is reported as a failure, not retried.

