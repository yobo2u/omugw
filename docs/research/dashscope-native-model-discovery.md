# DashScope Native Model Discovery API Contract

**Date:** September 6, 2026
**Topic:** Whether Alibaba Cloud Model Studio / DashScope Native has an official
runtime HTTP API to enumerate callable models, how that API differs from the
OpenAI-compatible `/v1/models` surface and from the static product catalog, and
what that implies for omugw native auto-discovery.
**Trigger:** Design of model auto-discovery for omugw. Discovered models will
only augment authenticated `GET /v1/models`; routing remains explicit. This
note is the first-party contract the design must cite for `dashscope.native`.

**Scope:** First-party Alibaba Cloud / DashScope documentation on
help.aliyun.com and www.alibabacloud.com. No third-party write-ups. No live
API calls. Retrieved 2026-09-06. The Alibaba Cloud international copy of List
models is dated **Last Updated: Sep 04, 2026**.

## Findings

### 1. Native runtime list API exists

**Yes.** Model Studio documents a dedicated control-plane operation:

```http
GET /api/v1/models
Authorization: Bearer {API_KEY}
```

Official prose (EN List models): "Call the GET /api/v1/models endpoint to
retrieve the list of available models on Model Studio. You can filter by model
provider, modality type, model capability, and deployment mode, and get
information such as pricing and context length."

ZH: "调用 GET /api/v1/models 接口查询百炼平台上可用的模型列表，支持按模型作者、
模态类型、模型能力、部署模式等条件筛选，并获取模型的定价和上下文长度等信息。"

This is **not** an inference endpoint. It lives under API Reference (Models) →
More, alongside quotas and permissions, not under DashScope native generation
(`qwen-api-via-dashscope`). Native inference remains:

- `POST /api/v1/services/aigc/text-generation/generation`
- `POST /api/v1/services/aigc/multimodal-generation/generation`

omugw already models those two as `TextGenerationPath` and
`MultimodalGenerationPath` under `NamespacePrefix = "/api/v1/"`. List models
shares the Native `/api/v1` prefix but is a different resource.

The static catalog at `/en/model-studio/models` (ZH `/zh/model-studio/models`)
still exists. It is **not** the only official source: the runtime list API is
first-party and documented.

### 2. Authentication

Same Bearer API key as Native inference.

- Header: `Authorization: Bearer {API_KEY}`
- Prerequisite: create an API key and set env `DASHSCOPE_API_KEY`
- No separate Admin / list-only credential is documented on the list-models
  page
- Keys are **region-specific**. Pair the key with a Base URL from the same
  billing plan / region; a mismatch returns **401**. Base URL overview:
  "The Base URL is the API endpoint for model calls. Pair it with an API Key
  from the same billing plan — a mismatch returns a 401 error. API Keys are
  region-specific."
- OpenAI-compat page: an API key created in China (Beijing) used against the
  US (Virginia) endpoint returns HTTP 401 `Incorrect API key provided` /
  `invalid_api_key`. That is a region mismatch, not "the key is invalid."

Auth is therefore **not** different in scheme from Native generation. It **is**
different in host selection (see §3).

### 3. Hosts, workspace, and region

List-models documents these regional endpoints. Replace `{WorkspaceId}` with
the workspace ID where the table says so.

| Region | List-models host (as documented) | WorkspaceId in host? |
|---|---|---|
| China (Beijing) | `https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api/v1/models` | yes |
| Singapore | `https://dashscope-intl.aliyuncs.com/api/v1/models` | no (shared DashScope domain) |
| China (Hong Kong) | `https://cn-hongkong.dashscope.aliyuncs.com/api/v1/models` | no (shared DashScope domain) |
| Germany (Frankfurt) | `https://{WorkspaceId}.eu-central-1.maas.aliyuncs.com/api/v1/models` | yes |
| Japan (Tokyo) | `https://{WorkspaceId}.ap-northeast-1.maas.aliyuncs.com/api/v1/models` | yes |
| US (Virginia) | `https://{WorkspaceId}.us-east-1.maas.aliyuncs.com/api/v1/models` | yes |

Facts that follow from first-party pages, not from guessing:

1. **Beijing list-models is workspace-dedicated maas, not the shared
   DashScope domain.** The list-models table does **not** document
   `https://dashscope.aliyuncs.com/api/v1/models` as a list-models URL.
   Base URL overview *does* document Native inference on the shared domain
   (`https://dashscope.aliyuncs.com/api/v1`) and says to replace the path
   after the domain with `/api/v1`. That is the inference base, not a
   documented list-models URL for Beijing.
2. **Singapore and Hong Kong list-models examples use shared DashScope
   domains** (`dashscope-intl.aliyuncs.com`, `cn-hongkong.dashscope.aliyuncs.com`)
   without `{WorkspaceId}`. The same OpenAI-compat / Native inference pages
   now recommend migrating those regions to `{WorkspaceId}.….maas.aliyuncs.com`.
   List-models has not been updated to that recommendation on the pages
   fetched 2026-09-06.
3. **Germany and Japan have no DashScope shared domain** (regions page:
   DashScope domain "Not supported"). List-models for those regions is
   workspace-dedicated maas only.
4. **Regions page:** "Each region has its own access domain, API Key, and
   model list. **These cannot be used across regions.**"
5. **Workspace-dedicated vs DashScope vs trial** (regions + base-url):
   - Workspace-dedicated: `{WorkspaceId}.{region}.maas.aliyuncs.com` —
     "Access to the current workspace only"; recommended for production;
     timeout 3600s.
   - DashScope shared: `dashscope.aliyuncs.com` (Beijing),
     `dashscope-intl.aliyuncs.com` (Singapore), `dashscope-us.aliyuncs.com`
     (Virginia) — "Access to all workspaces"; timeout 600s; "legacy shared
     domain. Still available; migration to a workspace-dedicated domain is
     recommended."
   - Trial: `trial.{region}.maas.aliyuncs.com` — all workspaces, lower RPM.
6. **Sub-workspace:** "The API key for the default workspace can call all
   models." A sub-workspace key needs explicit model-call grants for
   standard models (e.g. `qwen-plus`). Fine-tuned / deployed models "do not
   require model call permissions" but "you can only call these models with
   the API key from their workspace."

A gateway probe must therefore: use a key from the same region as the
configured Native `base_url`; if the configured host is
`{WorkspaceId}.….maas.aliyuncs.com`, list against that host; do not assume
Beijing shared `dashscope.aliyuncs.com/api/v1/models` is a documented list
URL.

### 4. Request: query string, filters, pagination

All list-models parameters are query string. No request body.

| Parameter | Type | Required | Contract |
|---|---|---|---|
| `providers` | Array[String] | no | Model provider. Repeat: `providers=qwen&providers=deepseek`. Values include `qwen`, `deepseek`, `zhipu-ai`, `wan`, `moonshot-ai`, `mini-max`, `kling`, `vidu`, `pixverse`, `xiaomi`, `tripo`, `happyhorse`, `qwen-domain-model`. |
| `inference_providers` | Array[String] | no | Inference supplier. Values include `aliyun-bailian`, `alibaba-cloud-modelstudio`, `siliconflow`, `moonshot`, `mini-max`, `kling`, `vidu`, `pixverse`, `vanchin`, `xiaomi`, `zhipu-ai`, `tripo`. |
| `capabilities` | Array[String] | no | Modality / product type: `TG`, `Reasoning`, `VU`, `IG`, `VG`, `ASR`, `TTS`, `TR`, `ME`, `Multimodal-Omni`, `Realtime-Omni`, `Realtime-Text-to-Speech`, `Realtime-ASR`, `Realtime-Audio-Translate`, `3D-generation`, `Realtime-Chatting`. |
| `features` | Array[String] | no | Capability flags: `function-calling`, `structured-outputs`, `web-search`, `prefix-completion`, `cache`, `batch`, `fine-tuning`, `model-experience`. |
| `context_window` | Integer | no | Return models whose context length is **≥** this value. |
| `service_site` | String | no | Deployment mode. Omit = all. Values: `global`, `international`, `asia-pacific-china`, `cn-hongkong`, `european-union`, `united-states`, `japan`. |
| `supports` | Array[String] | no | **Default: `inference`.** Values: `inference` (models that support inference), `deploy` (models that support deployment). |
| `deployment_methods` | Array[String] | no | Currently `ptu` (Provisioned Throughput Unit). |
| `deployment_ptu_service_tiers` | Array[String] | no | PTU type; requires `deployment_methods` to include `ptu`. |
| `name` | String | no | Fuzzy search by model name. Example: `qwen`. |
| `model` | String | no | Exact match by model ID. Example: `qwen3-max`. |
| `language` | String | no | Response language: `zh-CN`, `en-US`. |
| `page_no` | Integer | no | Page number, starting at 1. Default **1**. |
| `page_size` | Integer | no | Items per page. Default **20**. |

Pagination facts:

- This **is** a page-number API (`page_no` / `page_size`), unlike OpenAI
  `GET /v1/models` which has no pagination parameters.
- Default page size is 20. Sample curl uses `page_size=20` and
  `page_size=100`. Sample response shows `"total": 168` with
  `"page_size": 10`.
- **List-models does not document a maximum `page_size`.** The sibling
  permissions API documents `page_size` ranging **1–200 (maximum 200)**.
  Do not treat 200 as a documented list-models max; it is documented only
  on permissions.
- Official Python SDK `dashscope.Models.list(page=1, page_size=20, …)`
  "currently supports only `page` and `page_size` pagination parameters. It
  does not support filtering by `capabilities`, `providers`, `features`,
  etc." HTTP curl is the documented way to filter.
- `supports` default `inference` is **on the list-models page itself**. It
  is first-party, not a search-grounded extra.

A gateway client that wants the full catalog must loop `page_no` until
`len(output.models) == 0` or `page_no * page_size >= output.total`. The
docs do not name a cursor; do not send OpenAI-style `after` / `limit`.

### 5. Response schema

Success envelope (from the documented sample and the response-parameter
table):

```json
{
  "code": null,
  "message": null,
  "success": true,
  "output": {
    "total": 168,
    "page_no": 1,
    "page_size": 10,
    "models": [ /* Model objects */ ]
  },
  "request_id": "d5f5201f-ee7a-9e3d-8569-bc0eedec21f9"
}
```

Documented fields:

| Field | Type | Contract |
|---|---|---|
| `request_id` | String | Troubleshooting id. |
| `code` | String (sample `null`) | Error code; `null` on success (permissions page states this explicitly; list-models sample matches). |
| `message` | String (sample `null`) | Error message; `null` on success. |
| `success` | Boolean (sample `true`) | Call succeeded. Listed as a response field on permissions; present in the list-models sample. |
| `output.total` | Number | Total matching models (not just this page). |
| `output.page_no` | Number | Current page. |
| `output.page_size` | Number | Page size. |
| `output.models[].model` | String | **Model ID, used to specify the model in API calls.** This is the wire id (`qwen3-max`, `qwen-plus`, …). |
| `output.models[].name` | String | Display name. |
| `output.models[].description` | String | Description. |
| `output.models[].provider` | String | Model provider / author, e.g. `qwen`. |
| `output.models[].inference_provider` | String | Inference supplier, e.g. `aliyun-bailian`. |
| `output.models[].capabilities` | Array[String] | Same vocabulary as the `capabilities` request filter. |
| `output.models[].features` | Array[String] | Same vocabulary as the `features` request filter. |
| `output.models[].published_time` | String or `null` | `yyyy-MM-dd HH:mm:ss`. May be `null`. |
| `output.models[].inference_metadata.request_modality` | Array[String] | Input modalities: `Text`, `Image`, `Audio`, `Video`. |
| `output.models[].inference_metadata.response_modality` | Array[String] | Output modalities; same enum. |
| `output.models[].model_info.context_window` | Integer or `null` | Total context window. `null` = no limit or not applicable. |
| `output.models[].model_info.max_input_tokens` | Integer or `null` | |
| `output.models[].model_info.max_output_tokens` | Integer or `null` | |
| `output.models[].model_info.max_reasoning_tokens` | Integer or `null` | |
| `output.models[].model_info.reasoning_max_input_tokens` | Integer or `null` | EN table; ZH table names 最大输入（思考）. |
| `output.models[].model_info.reasoning_max_output_tokens` | Integer or `null` | EN table; ZH table names 最大输出（思考）. |
| `output.models[].prices[].range_name` | String | `Default` or a tier such as `32k<Input<=128k`. |
| `output.models[].prices[].prices[].type` | String | Billing item, e.g. `input_token`, `output_token`, `image_number`. |
| `output.models[].prices[].prices[].price` | String | Unit price (sample uses strings `"2"`, `"0.075"`). |
| `output.models[].prices[].prices[].price_unit` | String | e.g. `"per million tokens"`. |
| `output.models[].prices[].prices[].price_name` | String | e.g. `"Input"`. |
| `output.models[].equivalent_snapshot` | String | **ZH response table only:** 对应的快照模型. Absent from the EN response table and from both samples. |

What the list object does **not** contain (do not invent):

- No OpenAI-shaped `{object: "list", data: [{id, object, created, owned_by}]}`.
- No `owned_by`, no unix `created`, no `shutdown_date`.
- No per-item "this API key may invoke this model" bit. That is the
  permissions API (`inference: true/false`).
- No quota / RPM / TPM. That is the quotas API.
- No statement that `output.models` is filtered to the current workspace's
  grants. Prose says "available models on Model Studio" / "百炼平台上可用的
  模型列表", not "models this workspace is authorized to call."

Parser guidance: fail-closed on missing `output.models[].model` (that is the
call id). Tolerate unknown fields. Treat `published_time` and `model_info.*`
nulls as documented. Do not require `equivalent_snapshot` (EN omits it).

### 6. Catalog list ≠ workspace-callable set

Documented separately, not as a field on list-models:

**Permissions** — `GET /api/v1/models/permissions`

- Prose: "query the list of authorizable or authorized models and their
  permission details in the current workspace."
- Host table on the permissions page: **China (Beijing) only**
  `https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api/v1/models/permissions`.
  No Singapore / HK / EU / JP / US row on that page.
- EN sample curl uses `https://dashscope.aliyuncs.com/api/v1/models/permissions`
  (shared Beijing DashScope domain). ZH sample curl uses the workspace maas
  host. Cite both; do not collapse them.
- Query: `name` (fuzzy on `name` and `model_id`), `model` (exact `model_id`),
  `page_no` (default 1), `page_size` (**1–200, max 200**, default 20),
  `authorization_scope` (`AUTHORIZABLE` default, or `AUTHORIZED`),
  `action` (`INFERENCE` only; filters only when
  `authorization_scope=AUTHORIZED`).
- Each row: `model`, `name`, `permissions.{inference, fine_tune|finetune, deploy}`
  booleans. EN table writes the example object with `"fine_tune"` and the
  field description as `finetune`; the sample body uses `"fine_tune"`. Wire
  sample is `fine_tune`.
- Default scope is **AUTHORIZABLE** (models you *could* grant), not
  AUTHORIZED (models already granted). A naive call does not return "what
  this key can call."

**Sub-workspace calling page:** default-workspace keys can call all models;
sub-workspace keys need grants for standard models. That is independent of
whether a model id appears in `GET /api/v1/models`.

**Quotas** — rate limits, not an inventory of callable ids.

- EN: `GET /api/v1/quotas` on workspace maas hosts for Beijing, Singapore
  (`{WorkspaceId}.ap-southeast-1.maas.aliyuncs.com`), Hong Kong
  (`{WorkspaceId}.cn-hongkong.maas.aliyuncs.com`), Germany, US. **No Japan
  row.** Singapore/HK here are **maas + WorkspaceId**, unlike list-models'
  shared DashScope hosts.
- ZH: `GET /api/v1/models/limits` and the Beijing host table only
  (`https://{WorkspaceId}.cn-beijing.maas.aliyuncs.com/api/v1/models/limits`).
  **EN and ZH disagree on the path.** Cite both. Do not pick one as "the"
  quotas URL without a fixture.
- Query: `name`, `model`, `page_no` (default 1), `page_size` (default 20).
  No max `page_size` on this page.
- Rows: `model`, `workspace_id`, `model_limit` (account cap),
  `workspace_limit` (null = unset). Fields include `request_limit` +
  `request_limit_period` (1 = QPS, 60 = RPM), `usage_limit` +
  `usage_limit_field` + `usage_limit_period` (TPM-style),
  `async_user_queue_limit`, `async_user_concurrency_limit`.
- Sample `"total": 453` — a quota list can be larger than a single
  list-models page.

Implication: **listing, authorization, and quota are three APIs.** Treating
`GET /api/v1/models` as "what this key can call" is not a documented
equivalence.

### 7. OpenAI-compatible `/v1/models` is not a documented list API

OpenAI-compatible BASE_URL is `/compatible-mode/v1` (chat completions:
`POST …/compatible-mode/v1/chat/completions`). Fetched first-party pages
(OpenAI compatible - Chat, Base URL overview, List models, More index) do
**not** document `GET /compatible-mode/v1/models` as a dedicated list-models
operation.

Do not treat OpenAI's `GET /v1/models` envelope as DashScope Compatible's
documented contract. Compatible chat tells clients to change API key, base
URL, and model name; model names are taken from the static catalog /
"Supported models" section, not from a documented compatible list call.

If a gateway later observes that Compatible `GET /models` works on the
wire, that observation is **undocumented on the pages fetched 2026-09-06**
and must not be cited as first-party contract.

### 8. Errors

List-models, permissions, and quotas all defer to the shared Error messages
page (`/en/model-studio/error-code`, ZH `/zh/model-studio/error-code`).
They do not attach a Models-specific status table.

OpenAI-compat documents 400 / 401 / 429 / 500 / 503 for chat. Base URL
documents 401 on key/plan/region mismatch. Regions document that keys,
domains, and model lists cannot cross regions. None of that is restated as
a list-models status matrix.

SDK note: `Models.list()` is documented on the list-models page as
pagination-only. Using it does not expand the HTTP contract.

### 9. omugw today (read-only; not a doc claim)

- `Router.Models()` returns exact-match config names "供 /v1/models 端点使用"
  and refuses to invent a list from prefix/`*` rules.
- `Router.Resolve` error text deliberately does not enumerate models;
  callers who want a list are told to use authenticated `/v1/models`.
- `gateway.Build` registers `GET /healthz` plus Native POST doors
  (text-generation, multimodal-generation) and `POST /api/v1/` 501 /
  methodless `/api/v1/` 404 fallbacks. There is **no** `GET /v1/models`
  handler in `internal/gateway`.
- Native `GET /api/v1/models` would today hit the methodless `/api/v1/`
  fallback and return framework **404**, not a catalog. That is gateway
  behavior, not upstream contract.
- `config.example.yaml` routes are static `models[].match` → targets.
  Discovery must not create those routes.

## Implications for omugw auto-discovery

Facts above; design inference isolated here.

1. **Support Native discovery as an official upstream probe**, not as a
   static scrape of `/model-studio/models`. The runtime API is
   `GET /api/v1/models` with Bearer auth, page-number pagination, and a
   rich filter/metadata schema.
2. **Do not create routes from the list.** Product rule unchanged:
   discovery only augments authenticated gateway `GET /v1/models`.
   `Router.Models()` remaining the exact-match config set is consistent
   with "prefix/`*` are infinite."
3. **Do not treat list rows as callable.** Intersect with
   `GET /api/v1/models/permissions?authorization_scope=AUTHORIZED&action=INFERENCE`
   if the product wants "this workspace may call," and only where that
   API is documented (Beijing host table). Sub-workspace grants are a
   separate control plane. Default-workspace keys "can call all models"
   is a calling-page sentence, not a list-models filter.
4. **Host selection must follow the configured Native endpoint**, not a
   hardcoded `dashscope.aliyuncs.com`. Beijing list-models is documented
   as `{WorkspaceId}.cn-beijing.maas.aliyuncs.com`. Shared
   `dashscope.aliyuncs.com/api/v1/models` is **not** on the list-models
   table. Singapore/HK list-models currently document shared DashScope
   hosts; quotas for those regions document maas hosts. Do not assume one
   host table applies to all three APIs.
5. **Paginate.** Default 20, sample totals 168 (list) / 453 (quotas). One
   GET is not the full catalog. `page_size` max is undocumented on
   list-models; permissions max 200 is not transferable without a
   fixture.
6. **Map Native `output.models[].model` → gateway list `id`.** Do not
   require OpenAI's `created` / `owned_by` / `shutdown_date`; they are
   not in this schema. Extra Native fields (prices, modalities,
   `model_info`) may be dropped or passed through as unknown properties
   depending on whether the gateway's `/v1/models` aims at OpenAI client
   compatibility (see `docs/research/openai-list-models-contract.md`).
7. **`supports` defaults to `inference`.** A probe that omits the
   parameter is documented to return inference-capable models, not
   deploy-only SKUs. If the gateway wants deployable-only rows it must
   pass `supports=deploy` explicitly.
8. **Do not implement Compatible discovery from these pages.** Compatible
   `GET /compatible-mode/v1/models` is undocumented here. Native list is
   the first-party enum for DashScope.
9. **Quotas EN `/api/v1/quotas` vs ZH `/api/v1/models/limits` is a real
   doc conflict.** A gateway that needs RPM/TPM must fixture the path
   against the region it calls; do not encode one spelling as if the
   other were a typo without evidence.
10. **Fail-closed on region/key mismatch (401)** rather than falling
    back to another region's catalog. Regions page: keys, domains, and
    model lists cannot be used across regions.

**Downstream decision this note supports:** Native auto-discovery is
officially supportable via `GET /api/v1/models` (optional sibling
permissions / quotas). Skip is not required by missing docs. Delegate to
another control-plane API only if the product wants grants (`permissions`)
or rate-limit rows (`quotas` / `models/limits`), not because a list API
is absent. Static catalog is not the only official source. Catalog list
is not documented as equal to the workspace-callable set.

## Sources

- [List models (EN, help.aliyun.com)](https://help.aliyun.com/en/model-studio/list-models) — GET `/api/v1/models`; hosts; query filters including `supports` default `inference`; response fields; pagination defaults; curl + `Models.list()` SDK limitation; sample envelope
- [查询模型列表 (ZH)](https://help.aliyun.com/zh/model-studio/list-models) — same operation; ZH filter labels; extra response field `equivalent_snapshot`
- [List models (EN, alibabacloud.com)](https://www.alibabacloud.com/help/en/model-studio/list-models) — Last Updated: Sep 04, 2026; Singapore curl examples on `dashscope-intl.aliyuncs.com`
- [List model permissions (EN)](https://help.aliyun.com/en/model-studio/list-model-permissions) — GET `/api/v1/models/permissions`; Beijing maas host table; `page_size` 1–200; `AUTHORIZABLE` / `AUTHORIZED`; sample uses shared `dashscope.aliyuncs.com`
- [查询模型授权 (ZH)](https://help.aliyun.com/zh/model-studio/list-model-permissions) — same; ZH sample uses `{WorkspaceId}.cn-beijing.maas.aliyuncs.com`
- [List model quotas (EN)](https://help.aliyun.com/en/model-studio/list-quotas) — GET `/api/v1/quotas`; maas hosts including Singapore `ap-southeast-1` and Hong Kong `cn-hongkong`; account vs workspace limits
- [查询模型限额 (ZH)](https://help.aliyun.com/zh/model-studio/list-quotas) — GET `/api/v1/models/limits`; Beijing-only host table (path conflict with EN)
- [More (EN API index)](https://help.aliyun.com/en/model-studio/more-about-models/) — list models / quotas / permissions sit under More, not under DashScope generation
- [Model calls in a sub-workspace](https://help.aliyun.com/en/model-studio/model-calling-in-sub-workspace) — default workspace key can call all models; sub-workspace needs grants; Native POST paths
- [Base URL overview](https://help.aliyun.com/en/model-studio/base-url) — DashScope vs workspace-dedicated vs trial; Native path `/api/v1`; 401 on key/plan mismatch; keys region-specific
- [Regions and access domains](https://help.aliyun.com/en/model-studio/regions) — keys / domains / model lists cannot cross regions; Germany/Japan have no DashScope shared domain
- [DashScope API Reference](https://help.aliyun.com/en/model-studio/qwen-api-via-dashscope) — Native inference POST paths (not list)
- [OpenAI compatible - Chat](https://help.aliyun.com/en/model-studio/compatibility-of-openai-with-dashscope) — BASE_URL `/compatible-mode/v1`; chat completions only; no documented GET models
- [Models catalog](https://help.aliyun.com/en/model-studio/models) — static catalog; still official, not the only source
- omugw `internal/router/router.go` — `Resolve` / `Models()` comments (read-only)
- omugw `internal/protocol/dashscopenative/wire.go` — `NamespacePrefix`, generation paths (read-only)
- omugw `internal/gateway/build.go` — Native doors + `/api/v1/` 404/501 fallbacks; no GET `/v1/models` (read-only)
