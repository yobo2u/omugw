# DashScope OpenAI-Compatible `GET /v1/models` Contract

**Date:** September 6, 2026
**Topic:** Whether Alibaba Cloud Model Studio / DashScope OpenAI-compatible
mode officially documents `GET /compatible-mode/v1/models` (the OpenAI
`listModels` analogue), and what first-party sources actually expose on
that prefix.
**Trigger:** Design of model auto-discovery for omugw on configured
`dashscope.compatible` endpoints. Discovered models will only augment
authenticated gateway `GET /v1/models`; routing remains explicit. This
note is the first-party contract the design must cite for
`dashscope.compatible`.
**Scope:** First-party Alibaba Cloud / DashScope documentation on
help.aliyun.com (CN + EN) and the official DashScope Python SDK at
commit `39c16c758a46b6145ef2c1e13e076c15b869f281`. No third-party
write-ups. No live HTTP against DashScope. Retrieved 2026-09-06.

Native listing (`GET /api/v1/models`) is **not** restated here. See
[`dashscope-native-model-discovery.md`](dashscope-native-model-discovery.md).
OpenAI's own `GET /v1/models` contract is
[`openai-list-models-contract.md`](openai-list-models-contract.md); do
not copy that schema onto DashScope compatible-mode.

## Findings

### 1. Official support for compatible-mode `GET /v1/models`: **no**

Alibaba Cloud Model Studio does **not** document an OpenAI-compatible
Models resource. There is no first-party page titled "OpenAI compatible -
Models", no documented HTTP method `GET` on
`/compatible-mode/v1/models`, and no documented response envelope of
OpenAI `object: "list"` / `data[]` of `object: "model"` for that path.

The toolkit index is the official inventory of OpenAI-compatible
surfaces. CN [工具包/框架](https://help.aliyun.com/zh/model-studio/toolkits-and-frameworks/)
and EN [Toolkit/Framework](https://help.aliyun.com/en/model-studio/toolkits-and-frameworks/)
list, in this order:

- OpenAI compatible - Chat
- OpenAI-compatible - Responses
- OpenAI compatible - Completions
- OpenAI-compatible - Vision
- OpenAI compatible - File
- OpenAI-compatible - Batch (file input)
- OpenAI-compatible - Batch Chat
- OpenAI compatible - Embedding
- OpenAI-compatible - Conversations
- LangChain

There is no Models entry. The Chat page is the next article after the
index; the Conversations page is the last OpenAI-compatible article
before LangChain. Neither index mentions `GET /v1/models`.

The official "list models" operation lives under API Reference (Models)
→ More, **not** under Toolkit/Framework:

- CN [更多](https://help.aliyun.com/zh/model-studio/more-about-models/):
  「查询模型列表」→ `/zh/model-studio/list-models`
- EN [More](https://help.aliyun.com/en/model-studio/more-about-models/):
  "List models" → `/en/model-studio/list-models`

That page documents **native** `GET /api/v1/models`. CN:

> 调用 GET /api/v1/models 接口查询百炼平台上可用的模型列表

EN:

> Call the GET /api/v1/models endpoint to retrieve the list of available
> models on Model Studio.

Hosts on that page are `/api/v1/models` (Beijing
`https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api/v1/models`,
Singapore `https://dashscope-intl.aliyuncs.com/api/v1/models`, and
further native hosts). None of those URLs contain `compatible-mode`.
Schema, pagination, filters, and permissions for that native operation
are in
[`dashscope-native-model-discovery.md`](dashscope-native-model-discovery.md).

**Claim:** compatible-mode does not officially support
`GET /v1/models`. Absence from the toolkit index plus the official list
operation being native `/api/v1/models` is the first-party evidence.
This is **not** a runtime 404 proof (see §8).

### 2. What compatible-mode *does* document

Compatible-mode is a prefix, not a full OpenAI REST clone. Documented
resources under `/compatible-mode/v1` are POST-heavy inference plus a
small set of GET resources that are **not** Models.

#### 2.1 Chat — POST only

CN [OpenAI Chat接口兼容](https://help.aliyun.com/zh/model-studio/compatibility-of-openai-with-dashscope)
and EN [OpenAI compatible - Chat](https://help.aliyun.com/en/model-studio/compatibility-of-openai-with-dashscope):

SDK / OpenAI-compatible client `BASE_URL` (ends at `/compatible-mode/v1`,
no resource path):

```
北京：https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1
弗吉尼亚：https://dashscope-us.aliyuncs.com/compatible-mode/v1
新加坡：https://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1
日本（东京）：https://{WorkspaceId}.ap-northeast-1.maas.aliyuncs.com/compatible-mode/v1
```

HTTP "完整访问 endpoint" / "full endpoint" is **POST** chat completions
only:

```
北京：POST https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/chat/completions
弗吉尼亚：POST https://dashscope-us.aliyuncs.com/compatible-mode/v1/chat/completions
新加坡：POST https://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1/chat/completions
日本（东京）：POST https://{WorkspaceId}.ap-northeast-1.maas.aliyuncs.com/compatible-mode/v1/chat/completions
```

The HTTP section heading is 「提交接口调用」 / "Endpoint". The only
method shown is POST. Request parameters (`model`, `messages`, `stream`,
…) and response parameters (`id`, `choices`, `usage`, …) are the Chat
Completions contract. There is no Models subsection, no
`client.models.list()`, and no `GET …/models` curl.

Third-party client setup on the same page tells operators to paste the
SDK `BASE_URL` (ending `/compatible-mode/v1`, **without**
`/chat/completions`) and a **model name chosen from the page's
supported-models list**, not from a list-models call:

CN:

> **模型名称**：填写支持 OpenAI 兼容协议的大语言模型名称。可选模型以本文
> “兼容OpenAI需要信息”中支持的模型列表部分为准

EN:

> **Model name**: Enter the name of a large language model that supports
> the OpenAI compatible protocol. For the models that you can choose
> from, see Supported models.

Qwen-Audio is explicitly **out** of compatible-mode:

CN: 「Qwen-Audio不支持OpenAI兼容协议，仅支持DashScope协议。」

EN: "Qwen-Audio does not support the OpenAI compatible protocol. Use the
DashScope protocol instead."

That is a protocol-family exclusion, not a list-models filter.

#### 2.2 Responses — POST only

CN [OpenAI Responses接口兼容](https://help.aliyun.com/zh/model-studio/compatibility-with-openai-responses-api)
documents `POST /compatible-mode/v1/responses` (and warns that the old
path `/api/v2/apps/protocols/compatible-mode/v1/responses` is being
retired). Regional SDK `base_url` values again end at
`/compatible-mode/v1`. HTTP examples are POST. No Models resource.

#### 2.3 File — GET exists, on `/files`, not `/models`

CN [OpenAI文件接口兼容](https://help.aliyun.com/zh/model-studio/openai-file-interface)
documents GET on the File resource:

```
GET https://dashscope.aliyuncs.com/compatible-mode/v1/files
GET https://dashscope.aliyuncs.com/compatible-mode/v1/files/{file_id}
```

(The HTTP "需要配置的endpoint" lines also show the international host
`https://dashscope-intl.aliyuncs.com/compatible-mode/v1/files` and
`…/files/{file_id}`.)

List response shape is OpenAI-style file list (`object: "list"`,
`data[]` of `object: "file"`, `has_more`), **not** a model list.
Pagination is cursor `after` + `limit` (default 2000, range `[1,2000]`),
plus `purpose` / `create_before` / `create_after` filters. This proves
compatible-mode **can** document GET — and that when it does, the
resource is named. Models is not among those resources.

#### 2.4 Conversations — GET exists, on `/conversations`, not `/models`

CN [OpenAI Conversations接口兼容](https://help.aliyun.com/zh/model-studio/openai-compatible-conversations)
documents:

```
GET …/compatible-mode/v1/conversations/{conversation_id}
GET …/compatible-mode/v1/conversations/{conversation_id}/items
GET …/compatible-mode/v1/conversations/{conversation_id}/items/{item_id}
```

(Beijing `{WorkspaceId}.cn-beijing.maas.aliyuncs.com` and Singapore
`{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com`.) Create/update remain
POST; delete is DELETE. Again: GET is documented where the resource
exists. Models is not documented.

#### 2.5 Completions, Vision, Batch, Embedding

The toolkit index links these as separate OpenAI-compatible pages. This
note does not re-fetch each remaining page in full: Chat, Responses,
File, and Conversations already establish the pattern (documented
resource paths; Models absent). None of the index titles is Models.

### 3. Compatible-mode BASE_URL variants (auth prefix, not a Models host)

These URLs are the OpenAI-compatible **inference** prefix. They are
listed here because a probe of `GET {base}/models` would be constructed
from them. First-party docs never attach a Models path to them.

#### 3.1 Chat page (SDK form)

From CN/EN Chat (§2.1):

| Region | SDK `BASE_URL` |
|---|---|
| China (Beijing) | `https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1` |
| US (Virginia) | `https://dashscope-us.aliyuncs.com/compatible-mode/v1` |
| Singapore | `https://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1` |
| Japan (Tokyo) | `https://{WorkspaceId}.ap-northeast-1.maas.aliyuncs.com/compatible-mode/v1` |

Chat also says existing shared domains still work, with a migration
nudge:

- Beijing: `https://dashscope.aliyuncs.com` →
  `https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com`
- Singapore: `https://dashscope-intl.aliyuncs.com` →
  `https://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com`

EN Chat additionally mentions Hong Kong (China) migration
`https://cn-hongkong.dashscope.aliyuncs.com` →
`https://{WorkspaceId}.cn-hongkong.maas.aliyuncs.com`. That sentence is
on the EN Chat page; the CN Chat BASE_URL block quoted in §2.1 does not
list a Hong Kong SDK URL. Do not invent a CN Chat Hong Kong row from the
EN migration note.

#### 3.2 Base URL overview (pay-as-you-go + plans)

CN [Base URL总览](https://help.aliyun.com/zh/model-studio/base-url)
("Base URL 是模型 API 的调用地址。Base URL必须与同一计费方案的 API Key
配套使用，否则会报错 401。各地域的 API Key 相互独立、不能跨地域使用。"):

**Dashscope shared domains — OpenAI 兼容 column:**

| Region | OpenAI compatible |
|---|---|
| 华北2（北京） | `https://dashscope.aliyuncs.com/compatible-mode/v1` |
| 新加坡 | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1` |
| 美国（弗吉尼亚） | `https://dashscope-us.aliyuncs.com/compatible-mode/v1` |

Native is the same host with path `/api/v1` instead of
`/compatible-mode/v1` ("如需使用 DashScope 原生 API，请将上表 URL
中域名后的路径替换为`/api/v1`").

**Workspace-dedicated domains — OpenAI 兼容 column:**

| Region | OpenAI compatible |
|---|---|
| 华北2（北京） | `https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1` |
| 新加坡 | `https://{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1` |
| 日本（东京） | `https://{WorkspaceId}.ap-northeast-1.maas.aliyuncs.com/compatible-mode/v1` |
| 德国（法兰克福） | `https://{WorkspaceId}.eu-central-1.maas.aliyuncs.com/compatible-mode/v1` |
| 美国（弗吉尼亚） | `https://{WorkspaceId}.us-east-1.maas.aliyuncs.com/compatible-mode/v1` |

Chat's Virginia SDK URL is the **shared** `dashscope-us.aliyuncs.com`
form; Base URL overview also documents a **workspace-dedicated**
Virginia host `{WorkspaceId}.us-east-1.maas.aliyuncs.com`. Both are
first-party; they are different host classes. Responses documents
Virginia as the dedicated form
`https://{WorkspaceId}.us-east-1.maas.aliyuncs.com/compatible-mode/v1`.

**Trial domains** (cross-workspace keys, smaller rate limits):

| Region | OpenAI compatible |
|---|---|
| 华北2（北京） | `https://trial.cn-beijing.maas.aliyuncs.com/compatible-mode/v1` |
| 新加坡 | `https://trial.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1` |

**Token Plan** (interactive tools only, not backend services):

| Region | OpenAI compatible |
|---|---|
| 华北2（北京） | `https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1` |

**Coding Plan** (interactive tools only):

| Region | OpenAI compatible |
|---|---|
| 华北2（北京） | `https://coding.dashscope.aliyuncs.com/v1` |

Coding Plan's OpenAI-compatible column is `/v1`, **not**
`/compatible-mode/v1`. Do not assume Coding Plan speaks the same
compatible-mode resource tree as pay-as-you-go Chat.

None of these tables include a Models path.

Get API Key CN [获取与配置 API Key](https://help.aliyun.com/zh/model-studio/get-api-key)
repeats the shared compatible BASE_URL for third-party tools:

- 中国大陆版：`https://dashscope.aliyuncs.com/compatible-mode/v1`
- 国际版：`https://dashscope-intl.aliyuncs.com/compatible-mode/v1`

and tells the operator to type a **model name** (`qwen-plus`,
`qwen3-8b`, `deepseek-r1`, …), not to call list-models.

### 4. Authentication (compatible-mode inference; not a Models auth scheme)

Compatible-mode HTTP examples use:

```http
Authorization: Bearer $DASHSCOPE_API_KEY
```

Chat curl:

```
curl --location 'https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/chat/completions' \
--header "Authorization: Bearer $DASHSCOPE_API_KEY" \
--header 'Content-Type: application/json' \
--data '{ ... }'
```

File and Conversations GET examples use the same Bearer header.

Keys are **region-bound** and **billing-plan-bound**:

- Chat CN 「跨地域调用」: 「百炼 API Key 按地域绑定。调用某个地域的
  `base_url` 时，必须使用在同一地域创建的 API Key；使用其他地域的 API
  Key 调用该地域 endpoint 会被鉴权拒绝。」 Beijing key against Virginia
  endpoint → HTTP 401, `Incorrect API key provided`,
  `invalid_api_key`. 「该错误表示 API Key 与 endpoint 所属地域不匹配，
  而非 API Key 失效或权限不足。」
- Base URL overview: mismatch of Base URL and billing-plan key → **401**.
  「各地域的 API Key 相互独立、不能跨地域使用。」

Chat error table (compatible-mode Chat, not Models):

| Code | Meaning |
|---|---|
| 400 Invalid Request Error | bad input |
| 401 Invalid API-key provided / Incorrect API key provided | wrong or region-mismatched key |
| 429 Rate limit reached for requests | QPS/QPM |
| 429 You exceeded your current quota… | quota / arrears |
| 500 | server error |
| 503 | overloaded, retry |

A third-party client HTTP 400 with
`error.message` = `current user api does not support http call` means
**that model** does not support OpenAI-compatible HTTP (example:
`qvq-max`), not that Models is missing.

There is **no** documented auth scheme, error body, or status code for
`GET /compatible-mode/v1/models`, because the operation is not
documented.

### 5. Response schema, pagination: **not applicable** (undocumented)

Because no Models operation is documented on compatible-mode:

- There is no official compatible-mode list schema (`object`, `data`,
  `id`, `created`, `owned_by`, …).
- There is no official pagination (`limit` / `after` / `page_no` /
  `page_size`) for compatible-mode Models.
- Do not import OpenAI's `listModels` schema
  ([`openai-list-models-contract.md`](openai-list-models-contract.md))
  as a DashScope fact.
- Do not import native `output.models[]` / `page_no` / `page_size`
  ([`dashscope-native-model-discovery.md`](dashscope-native-model-discovery.md))
  as a compatible-mode fact. Native list is a different path, host
  table, and envelope.

The closest documented "which models can I name?" surface on
compatible-mode pages is a **static supported-models paragraph** on Chat
(Qwen commercial/open-source, Qwen-VL, Qwen-Coder, Qwen-Omni, Qwen-Math,
DeepSeek / Kimi / GLM / MiniMax with supply-channel notes; third-party
direct-supply only on China-site Beijing after console enablement).
That is documentation, not a runtime enum.

### 6. Official Python SDK: Models is native; Chat is compatible

Official SDK commit
[`39c16c758a46b6145ef2c1e13e076c15b869f281`](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/common/env.py)
splits the two bases:

```python
base_http_api_url = os.environ.get(
    "DASHSCOPE_HTTP_BASE_URL",
    f"https://dashscope.aliyuncs.com/api/{api_version}",
)
base_compatible_api_url = os.environ.get(
    "DASHSCOPE_COMPATIBLE_BASE_URL",
    f"https://dashscope.aliyuncs.com/compatible-mode/{api_version}",
)
```

[`dashscope/models.py`](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/models.py)
`class Models(ListMixin, GetMixin)`:

- `SUB_PATH = "models"`
- `get()` comment: `API endpoint: /api/v1/models?model={name}&page_no=1&page_size=1`
- URL: `join_url(dashscope.base_http_api_url, cls.SUB_PATH.lower())` —
  **native** `base_http_api_url`, not `base_compatible_api_url`
- If `output.models` is missing or empty, the SDK **rewrites the HTTP
  status to 404** and message `Model '{name}' not found`. That 404 is an
  SDK client-side mapping on the **native** list response, not a
  compatible-mode contract.
- `list(page=1, page_size=10, …)` delegates to `ListMixin.list`

[`dashscope/model.py`](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/model.py)
(`class Model`) is the same `SUB_PATH = "models"` on the mixin GET/LIST
path (native).

[`ListMixin.list`](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/client/base_api.py)
builds `url = _get_url(custom_base_url, cls.SUB_PATH.lower(), path)`
where `_get_url` defaults to `dashscope.base_http_api_url`, then GET
with `params = {"page_no": page_no, "page_size": page_size}`. SDK
defaults: `page_no=1`, `page_size=10`. Official list-models HTTP docs
default `page_size` to **20**. The SDK default and the HTTP default
differ; both are native. Compatible-mode has neither.

GitHub code search of `dashscope/dashscope-sdk-python` for the literal
`compatible-mode/v1/models` returned **no results**.

List-models docs themselves show `from dashscope import Models` /
`Models.list(page=1, page_size=20)` next to curl against
`/api/v1/models`, and warn that `Models.list()` only accepts `page` /
`page_size` (no `capabilities` / `providers` filters). That is native
SDK coverage of the native API.

### 7. Permission visibility is native, not compatible-mode

CN [查询模型授权](https://help.aliyun.com/zh/model-studio/list-model-permissions):

```
GET https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api/v1/models/permissions
Authorization: Bearer {API_KEY}
```

Query: `authorization_scope=AUTHORIZABLE|AUTHORIZED` (default
`AUTHORIZABLE`), `action=INFERENCE`, `page_no` default 1, `page_size`
default 20 max 200. Envelope is native (`output.permissions[]` with
`inference` / `fine_tune` / `deploy` booleans). Path is `/api/v1/…`,
not `/compatible-mode/v1/…`.

CN [子业务空间的模型调用](https://help.aliyun.com/zh/model-studio/model-calling-in-sub-workspace):

> 默认业务空间的 API Key 可调用所有模型（权限过大）。如需管控 RAM 用户
> 可调用的模型，可将其添加至某个子业务空间，仅授权必要模型，并要求使用
> 该空间的 API Key 调用。

Get API Key FAQ:

> **默认业务空间下的 API Key：**可调用所有[标准模型]
> **子业务空间下的 API Key：**可调用该子业务空间已获得模型调用授权的标准模型

「可调用所有模型」 is a **calling-page** sentence about default-workspace
keys. It is **not** documented as a filter on compatible-mode
list-models (there is no such operation), and it is **not** a substitute
for native `GET /api/v1/models` / `GET /api/v1/models/permissions`.

Sub-workspace compatible-mode examples still POST
`/compatible-mode/v1/chat/completions` with that workspace's API key.
They do not GET `/models`. Fine-tuned-then-deployed models: 「仅支持通过
DashScope 调用，不支持通过OpenAI兼容方式调用。」

### 8. Unsupported / undocumented behavior

| Behavior | First-party status |
|---|---|
| `GET /compatible-mode/v1/models` | **Not documented** on toolkit index, Chat, Responses, File, Conversations, Base URL, More, or SDK |
| `GET /compatible-mode/v1/models/{id}` | **Not documented** |
| OpenAI SDK `client.models.list()` against a DashScope compatible `base_url` | **Not documented** by Alibaba; OpenAI client would issue GET `{base}/models` by OpenAI's contract, which DashScope does not publish |
| HTTP status of that GET (200 vs 404 vs 405 vs 401) | **Unproven in this research.** Live HTTP was out of scope. Do **not** cite 404 or 200. |
| Secondary write-ups claiming `GET /compatible-mode/v1/models` | **Not first-party.** Discard. |
| Native `GET /api/v1/models` | **Documented.** Different protocol family. See sibling native note. |
| Compatible-mode GET on File / Conversations | **Documented.** Different resources. |

Chat's troubleshooting line ("If a call through the OpenAI compatible
interface fails with a 404, 401, 403, or connection error, check the
following configurations") is about **misconfigured Chat calls** (wrong
host / key / path), not a Models contract.

### 9. Regional differences that matter for this question

- Compatible-mode **inference** hosts differ by region and by host class
  (shared DashScope domain vs `{WorkspaceId}.*.maas.aliyuncs.com` vs
  trial vs Token Plan vs Coding Plan). See §3.
- Native **list-models** hosts differ again (Singapore/HK list-models
  currently document shared DashScope `/api/v1/models`; Beijing
  list-models documents the maas host). That split is native-note
  material. It does not create a compatible-mode Models host.
- Third-party direct-supply models on Chat's supported list: China-site
  Beijing only, after console enablement. Still a static paragraph, not
  a GET `/models` feed.
- Coding Plan OpenAI-compatible URL is `/v1` not `/compatible-mode/v1`.

## Implications

Isolated from findings. These are design inferences for omugw, not
Alibaba documentation.

1. **Do not treat `GET /compatible-mode/v1/models` as a contract.** It is
   undocumented. A gateway that probes it is guessing. First-party
   listing for DashScope is native `GET /api/v1/models`
   ([`dashscope-native-model-discovery.md`](dashscope-native-model-discovery.md)).
2. **Do not create routes from any DashScope list.** Product rule
   unchanged: discovery only augments authenticated gateway
   `GET /v1/models`.
3. **If `dashscope.compatible` auto-discovery needs a runtime enum,
   the documented API is native, not compatible-mode.** Using native
   list while the configured outbound is compatible-mode is a **cross-
   family** control-plane call: different path (`/api/v1/models` vs
   `/compatible-mode/v1/…`), different envelope, and host tables that
   are not 1:1 (Chat Virginia shared `dashscope-us` vs list-models
   Virginia `{WorkspaceId}.us-east-1.maas.aliyuncs.com/api/v1/models`).
   That mapping is a product decision; this note does not bless a host
   rewrite. Compatible-mode Chat's "supported models" paragraph is
   documentation, not a live catalog, and excludes Qwen-Audio.
4. **Do not assume OpenAI `client.models.list()` works** against a
   DashScope compatible `base_url` just because Chat Completions works
   on that same base. File and Conversations show that DashScope
   documents GET only for named resources.
5. **Do not claim a runtime 404 (or 200) for compatible GET `/models`.**
   Undocumented ≠ proven absent on the wire. If a later fixture hits the
   path, record the status then; until then, fail closed on "no
   first-party contract."
6. **Auth/region/plan 401 still applies** to whatever host is used.
   Pair key, region, and billing plan. Sub-workspace keys do not inherit
   "all models"; default-workspace "can call all models" is not a
   list-models filter.
7. **Permissions and quotas stay on `/api/v1/`.** They are not
   compatible-mode resources.

**Downstream decision this note supports:** compatible-mode does **not**
officially support OpenAI `GET /v1/models`. Skip compatible-mode
list-models as a discovery source. Native list (sibling note) is the
DashScope first-party enum if the product wants a runtime catalog.
Static Chat "supported models" text is not a substitute enum.

## Sources

- [工具包/框架 (ZH)](https://help.aliyun.com/zh/model-studio/toolkits-and-frameworks/) — OpenAI-compatible index; no Models page
- [Toolkit/Framework (EN)](https://help.aliyun.com/en/model-studio/toolkits-and-frameworks/) — same index in English
- [OpenAI Chat接口兼容 (ZH)](https://help.aliyun.com/zh/model-studio/compatibility-of-openai-with-dashscope) — BASE_URL; POST `/chat/completions` only; Bearer auth; region-bound keys → 401 `invalid_api_key`; supported-models paragraph; Qwen-Audio excluded; third-party client pastes model name
- [OpenAI compatible - Chat (EN)](https://help.aliyun.com/en/model-studio/compatibility-of-openai-with-dashscope) — same; EN Hong Kong migration sentence
- [OpenAI Responses接口兼容 (ZH)](https://help.aliyun.com/zh/model-studio/compatibility-with-openai-responses-api) — POST `/compatible-mode/v1/responses`; no Models
- [OpenAI文件接口兼容 (ZH)](https://help.aliyun.com/zh/model-studio/openai-file-interface) — GET `/compatible-mode/v1/files` and `/files/{id}` exist; list envelope is files, not models
- [OpenAI Conversations接口兼容 (ZH)](https://help.aliyun.com/zh/model-studio/openai-compatible-conversations) — GET `/conversations/{id}` and items; no Models
- [查询模型列表 (ZH)](https://help.aliyun.com/zh/model-studio/list-models) — official list is native `GET /api/v1/models`
- [List models (EN)](https://help.aliyun.com/en/model-studio/list-models) — same; SDK `Models.list()` on native path
- [更多 (ZH)](https://help.aliyun.com/zh/model-studio/more-about-models/) — list models sits under More, not Toolkit/Framework
- [More (EN)](https://help.aliyun.com/en/model-studio/more-about-models/) — same
- [Base URL总览 (ZH)](https://help.aliyun.com/zh/model-studio/base-url) — compatible vs native path swap; shared / dedicated / trial / Token Plan / Coding Plan hosts; 401 on key/plan mismatch
- [获取与配置 API Key (ZH)](https://help.aliyun.com/zh/model-studio/get-api-key) — third-party tools get compatible BASE_URL + typed model name; default vs sub-workspace key visibility
- [查询模型授权 (ZH)](https://help.aliyun.com/zh/model-studio/list-model-permissions) — native `GET /api/v1/models/permissions`
- [子业务空间的模型调用 (ZH)](https://help.aliyun.com/zh/model-studio/model-calling-in-sub-workspace) — default-workspace keys "can call all models"; sub-workspace needs grants; compatible examples are POST chat; fine-tunes not on compatible-mode
- [dashscope-sdk-python `env.py` @ 39c16c758a46b6145ef2c1e13e076c15b869f281](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/common/env.py) — `base_http_api_url` vs `base_compatible_api_url`
- [dashscope-sdk-python `models.py` @ same SHA](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/models.py) — `SUB_PATH = "models"` on native base; get() 404 if `output.models` empty
- [dashscope-sdk-python `model.py` @ same SHA](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/model.py) — same native mixins
- [dashscope-sdk-python `base_api.py` ListMixin @ same SHA](https://github.com/dashscope/dashscope-sdk-python/blob/39c16c758a46b6145ef2c1e13e076c15b869f281/dashscope/client/base_api.py) — list GET uses `base_http_api_url`; default `page=1`, `page_size=10`
- GitHub code search `compatible-mode/v1/models` in `dashscope/dashscope-sdk-python` — no matches
- [`dashscope-native-model-discovery.md`](dashscope-native-model-discovery.md) — native list contract (do not duplicate)
- [`openai-list-models-contract.md`](openai-list-models-contract.md) — OpenAI list contract (do not copy onto DashScope)

