# OpenAI List Models API Contract

**Date:** September 6, 2026
**Topic:** Current official OpenAI model-listing HTTP contract (`GET /v1/models`),
including auth, response schema, pagination, ownership/permission visibility,
deletion/deprecation, and client-relevant timeout/error behavior.
**Trigger:** Design of model auto-discovery for omugw. Discovered models will only
augment authenticated `GET /v1/models`; routing remains explicit. This note is the
first-party contract the design must cite.

**Scope:** First-party OpenAI API docs, the published OpenAPI spec, and official
SDK sources generated from that spec. No third-party write-ups. Retrieved
2026-09-06.

## Findings

### 1. Endpoint

The list operation is HTTP **GET** on path **`/models`**. The published OpenAPI
server is `https://api.openai.com/v1`, so the wire URL is:

```http
GET https://api.openai.com/v1/models
Authorization: Bearer $OPENAI_API_KEY
```

Official docs and the spec use the same curl:

```http
curl https://api.openai.com/v1/models \
    -H "Authorization: Bearer $OPENAI_API_KEY"
```

Contract points:

- Operation id: `listModels`.
- Summary: "Lists the currently available models, and provides basic information
  about each one such as the owner and availability."
- No request body.
- No documented query parameters on this operation (contrast with paginated
  list operations such as Assistants, which declare `limit` / `order` / `after`
  / `before`).
- REST surface is currently `v1`. Response header `openai-version` is documented
  as `2020-10-01`.

Related operations on the same resource (not the list call, but part of the
Models contract):

| Method | Path | Operation | Purpose |
|---|---|---|---|
| GET | `/models/{model}` | `retrieveModel` | One model object |
| DELETE | `/models/{model}` | `deleteModel` | Delete a **fine-tuned** model |

The human-readable model catalog at `/api/docs/models` is a product catalog
(capabilities, pricing, aliases). It is **not** the list-API schema and must
not be treated as the wire contract for `GET /v1/models`.

### 2. Authentication

List models uses the same application authentication as the rest of the
non-Administration REST API.

- Credentials: HTTP Bearer. `Authorization: Bearer OPENAI_API_KEY_OR_ACCESS_TOKEN`.
- Accepted secrets: a standard API key, or a short-lived access token from
  [workload identity federation](https://developers.openai.com/api/docs/guides/workload-identity-federation).
- Administration endpoints require a separate Admin API key. List models is
  **not** an Administration endpoint; do not send an Admin key as if it were
  required here.
- OpenAPI `security` is global `ApiKeyAuth: []`, scheme `http` / `bearer`.
- Optional scoping headers when the caller belongs to more than one
  organization, or uses a legacy user API key:

```http
curl https://api.openai.com/v1/models \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "OpenAI-Organization: $ORGANIZATION_ID" \
  -H "OpenAI-Project: $PROJECT_ID"
```

Usage from those requests counts against the specified organization and
project. Organization and project IDs come from dashboard settings.

Auth-propagation caveats that apply to every authenticated call, including
list:

- API-key **revocation takes effect within a few seconds**.
- Most other updates that affect authentication results **propagate within 15
  minutes, but can take longer**.
- Keep total request header size under **64 KiB**. Oversized headers may fail
  before they reach the API, so the client may get neither a body nor
  `x-request-id`.
- `X-Client-Request-Id` is optional, ASCII, ≤ 512 characters; otherwise the
  request fails with **400**. OpenAI logs it for supported endpoints; list
  models is not named in that "including …" list (chat/completions, embeddings,
  responses, "and more").

### 3. Response schema

Successful list response (`200`) is `ListModelsResponse`:

| Field | Type | Required | Contract |
|---|---|---|---|
| `object` | `"list"` | yes | Envelope type. Always `"list"`. |
| `data` | array of `Model` | yes | Currently available models. |

Each `Model`:

| Field | Type | Required | Contract |
|---|---|---|---|
| `id` | string | yes | Model identifier; referenced in other API endpoints. |
| `created` | integer (`unixtime`) | yes | Unix timestamp **in seconds** when the model was created. |
| `object` | `"model"` | yes | Always `"model"`. |
| `owned_by` | string | yes | "The organization that owns the model." |
| `shutdown_date` | string (`format: date`) or `null` | no | "The date when the model will shut down, or null if not announced." |

Docs example (list):

```json
{
  "object": "list",
  "data": [
    {
      "id": "model-id-0",
      "object": "model",
      "created": 1686935002,
      "owned_by": "organization-owner",
      "shutdown_date": null
    },
    {
      "id": "model-id-1",
      "object": "model",
      "created": 1686935002,
      "owned_by": "organization-owner",
      "shutdown_date": null
    },
    {
      "id": "model-id-2",
      "object": "model",
      "created": 1686935002,
      "owned_by": "openai",
      "shutdown_date": "2026-10-23"
    }
  ]
}
```

Retrieve returns the same `Model` object (not wrapped in `data`). Docs example:

```json
{
  "id": "gpt-6-astra",
  "object": "model",
  "created": 1686935002,
  "owned_by": "openai",
  "shutdown_date": "2026-10-23"
}
```

What the current schema does **not** contain:

- No `permission` / `permissioning` array on `Model`. Retrieve's prose still
  says "owner and permissioning"; the published object has no permission
  field. Treat that sentence as leftover wording, not a wire field.
- No `has_more`, `first_id`, `last_id` on `ListModelsResponse`. Those fields
  exist on other list envelopes (Assistants, fine-tuning jobs, …) and are
  absent here.
- No per-item capability, pricing, context window, or modality metadata.
  Those live in the catalog docs, not this API.

Backwards-compatible API changes that a client **must** tolerate (overview):

- Adding new properties to JSON response objects.
- Changing the order of properties in a JSON response object.
- Changing the length or format of opaque strings (resource identifiers).

So a gateway parser should accept unknown fields on `Model` and on the list
envelope. `shutdown_date` itself is the kind of additive optional property
this policy covers.

### 4. Pagination

**`GET /models` is not a cursor-paginated list in the published contract.**

Evidence:

1. The list operation declares **no** `limit` / `after` / `before` / `order`
   parameters. Paginated OpenAI list operations declare those explicitly
   (Assistants is the contrast in the same spec).
2. `ListModelsResponse` required fields are only `object` and `data`. It does
   not include `has_more`. `ListPaginatedFineTuningJobsResponse` in the same
   spec **does** require `has_more`.
3. Official Node SDK, generated from the spec:

   ```ts
   list(options?: RequestOptions): PagePromise<ModelsPage, Model> {
     return this._client.getAPIList('/models', Page<Model>, { ...options, __security: { bearerAuth: true } });
   }
   // Note: no pagination actually occurs yet, this is for forwards-compatibility.
   export type ModelsPage = Page<Model>;
   ```

Implication for a gateway client: one GET is the documented way to obtain the
full `data` array. Do not send `limit`/`after` as if they were part of this
contract. Keep the SDK page iterator if using an official SDK (it is
forward-compatible), but do not design as if the HTTP API currently splits
the catalog across pages.

The docs do not state a maximum `data.length`. A client should not assume a
small fixed size.

### 5. Ownership and permission visibility

**What the list object exposes**

- `owned_by` is a string: "The organization that owns the model."
- Official examples use `"openai"` for platform models and
  `"organization-owner"` for org-owned rows. That is an example token, not a
  documented enum. Do not treat `owned_by` as a closed set.
- There is **no** per-model ACL, role, or "can this key invoke this model"
  field on the list or retrieve object.

**Who may call list**

RBAC names a distinct permission **List models**: "List models this
organization has access to." Permission identifier used in the Batch
implications table: `api.model.read` and `model.read`, targeting `/v1/models`.

Preset roles that include List models **Read**:

- Org owner, Org reader
- Project owner, Project member, **Project viewer**

List models is custom-role eligible. **Model capabilities Request** (actually
calling chat/audio/embeddings/images) is a **different** permission. Project
viewer has List models Read and does **not** have Model capabilities Request.

API-key evaluation (project key):

> If requesting with an API key within a project, we take the permissions
> assigned to the API key, and ensure that the user has some project role that
> grants them those permissions. For example, if requesting /v1/models, the
> API key must have api.model.read assigned to it and the user must have a
> project role with api.model.read.

Role/group changes: allow up to **30 minutes** to propagate.

**What "available" means (documented vs not)**

Documented:

- List returns models "currently available" and "this organization has access
  to."
- Optional `OpenAI-Organization` / `OpenAI-Project` headers select which
  org/project the request is billed and attributed to.
- 401 covers "API key that does not have the required permissions for the
  endpoint you are calling."

**Not documented** on the list operation (do not invent):

- Whether the array is filtered by project vs organization beyond the request
  headers.
- Whether a listed `id` is callable with the same key (listing ≠ Model
  capabilities Request).
- Whether fine-tuned models are always included. Fine-tune jobs return a
  `fine_tuned_model` id (`ft:…`); delete lives on `DELETE /models/{model}`
  with `ft:` examples; list examples include `owned_by: "organization-owner"`.
  That is consistent with org-owned (including fine-tuned) rows appearing in
  `data`, but the list page never says "includes your fine-tunes."

### 6. Deletion and deprecation

**Delete (API)**

- `DELETE /models/{model}`: "Delete a fine-tuned model. You must have the
  Owner role in your organization to delete a model."
- Path parameter example: `ft:gpt-4o-mini:acemeco:suffix:abc123`.
- `200` body `DeleteModelResponse` / `ModelDeleted`: `{ id, deleted, object }`,
  all required. Docs example uses `"object": "model"` and `"deleted": true`.
  The OpenAPI `object` field is an unconstrained string (not an enum).
- This is **not** how platform base models are retired. Clients must not
  DELETE `gpt-*` / `o*` platform ids.

**Deprecation vs shutdown vs legacy** (deprecations page, not the list
schema):

- **Deprecation**: announced retirement. The model or endpoint is immediately
  deprecated and **will have a shut-down date**. Until that date it remains
  accessible.
- **Sunset / shut down**: no longer accessible.
- **Legacy**: no longer receives updates; expected to be deprecated later.

Minimum notice, unless safety/compliance requires faster:

- Generally available models: at least **6 months**.
- Specialized variants of GA models: at least **3 months**.
- Preview models (`preview` in the name): much shorter, e.g. **2 weeks**.
  Not recommended for business-critical production unless the caller can
  migrate on short notice.

Notification: email to customers actively using the model, plus this
deprecations page (and blog posts for larger changes).

`shutdown_date` on the Model object is the machine-readable counterpart:
a calendar date (`YYYY-MM-DD` in examples) or `null` if unannounced. Example
value `"2026-10-23"` matches a documented upcoming GA snapshot shutdown
cluster on the deprecations page. The list API does **not** emit a separate
`deprecated: true` flag.

Fine-tuning platform wind-down (supervised fine-tuning guide + deprecations):

- New users can no longer access the fine-tuning platform; existing users can
  still create jobs for a documented window.
- "All fine-tuned models will remain available for inference until their base
  models are deprecated."
- Creating new jobs stops for remaining customers on **2027-01-06**; inference
  on existing fine-tunes is disabled only when the underlying base model is
  deprecated.

After shutdown, "the model or endpoint will no longer be accessible."
Dedicated capacity after a shutdown date is a sales conversation, not an API
field.

### 7. Timeout and error behavior relevant to clients

**HTTP API (server)**

The Models operations in OpenAPI document only `"200"` responses. They do
**not** attach the `429` / `503` response components that inference paths
(audio, etc.) declare. Clients should still handle the platform-wide error
envelope: failures are not Models-specific types.

Error envelope (`ErrorResponse`):

```json
{
  "error": {
    "type": "string",
    "message": "string",
    "param": "string or null",
    "code": "string or null"
  }
}
```

Platform error codes a list client can actually hit (auth, quota, rate,
availability) — none of these are Models-exclusive:

| HTTP | When it matters to a list client |
|---|---|
| 400 | Documented for invalid `X-Client-Request-Id` (ASCII, ≤ 512). `service_tier` 400 is an inference-parameter error, not list. |
| 401 | Invalid/revoked/wrong key; key lacks endpoint permission; not a member of an organization; IP not on allowlist. |
| 403 | Country/region/territory not supported. |
| 429 | Rate limit; `slow_down`; credit/spend/usage limits (`credit_balance_exhausted`, `organization_spend_limit_exceeded`, `project_spend_limit_exceeded`, `organization_usage_limit_exceeded`). Inspect `error.code`. Broader `error.type` may still be `insufficient_quota` for billing. **Do not retry** billing/spend/quota errors. |
| 500 | Server error; retry after a brief wait; check status page. |
| 503 | `service_unavailable_error` / `server_is_overloaded` — documented as **the requested model** being overloaded. List does not select a generation model; treat 503 as generic unavailability if it occurs. Follow `Retry-After` when present. |

Rate-limit headers that **may** appear on responses (overview + rate-limits
guide): `Retry-After`, `x-ratelimit-limit-requests`,
`x-ratelimit-limit-tokens`, `x-ratelimit-remaining-requests`,
`x-ratelimit-remaining-tokens`, `x-ratelimit-reset-requests`,
`x-ratelimit-reset-tokens`, plus project-token variants.
`Retry-After` is a **minimum** wait in seconds on temporary 429 and on 503
overload. It does **not** mean quota/billing errors become retryable.

Debug headers: `x-request-id` (log in production), `openai-organization`,
`openai-processing-ms`, `openai-version`.

Python SDK mapping (same HTTP codes, client-side): `401` →
`AuthenticationError`, `403` → `PermissionDeniedError`, `404` →
`NotFoundError` (retrieve/delete of unknown id), `429` → `RateLimitError`,
`>=500` → `InternalServerError`, connect failure → `APIConnectionError`,
client timeout → `APITimeoutError`.

**Timeouts**

The REST docs do **not** publish a server-side deadline specific to
`GET /models`. Official SDKs do publish a **client** default:

- Python: "By default requests time out after **10 minutes**." Override with
  `timeout=` (`float` or `httpx2.Timeout`). On timeout: `APITimeoutError`.
  Timed-out requests are retried twice by default.
- Node: "Requests time out after **10 minutes** by default." Override with
  `timeout` (milliseconds). On timeout: `APIConnectionTimeoutError`. Timed-out
  requests retried twice by default.
- Default retries (both): connection errors, 408, 409, 429, and >=500; default
  `max_retries` / `maxRetries` = 2.

A gateway calling OpenAI for discovery should set its **own** timeout. Ten
minutes is an SDK default aimed at long generation calls; it is not a
requirement of the list endpoint, and it is far longer than a catalog GET
should need. Follow `Retry-After` on 429/503; do not retry 401/403 or
billing 429s.

Retrieve of an unknown id is a `NotFoundError` (404) in the SDK table; the
Models OpenAPI path does not spell a 404 schema. Delete of a non-fine-tuned
or unauthorized model is not given a dedicated status in the Models spec
beyond the Owner-role requirement in prose.

### 8. Implications for omugw auto-discovery

Facts above; design inference isolated here.

1. **Wire shape to emit on the gateway's authenticated `GET /v1/models`** can
   follow OpenAI's envelope (`object: "list"` + `data[]` of `{id, object,
   created, owned_by}` plus optional `shutdown_date`) if the goal is client
   compatibility. Extra properties are explicitly backwards-compatible on
   OpenAI's side; the gateway may add fields only if it accepts that OpenAI
   clients must ignore unknowns (they should) or if it strips them for
   strict clients.
2. **Do not paginate** the upstream OpenAI fetch. One GET, whole `data`.
3. **Do not treat the list as a router.** Official list has no capability
   matrix and no "this key may call this model" bit. That matches the stated
   product rule: discovery **augments** `GET /v1/models`; routing stays
   explicit.
4. **`owned_by` is ownership, not authorization.** `"openai"` vs an org
   string is not an ACL. RBAC for the list call is `api.model.read` on both
   the key and the user.
5. **`shutdown_date` is advisory.** A listed id can already be deprecated and
   can disappear on that date. Discovery cache must be revalidated; a stale
   catalog will advertise shut-down models.
6. **Auth for the upstream probe is the same Bearer as any other OpenAI
   call**, plus optional org/project headers. Use a key that actually has
   List models. Do not use Admin keys. Expect 15-minute auth-update lag and
   near-immediate revocation.
7. **Fine-tunes** are first-class `Model` ids (`ft:…`) for retrieve/delete
   and inference. Whether every fine-tune appears in list is not an explicit
   list-endpoint sentence; do not promise a complete fine-tune inventory from
   `GET /models` without a fixture.
8. **Parser must be fail-open on unknown JSON fields** and fail-closed on
   missing required ones (`id`, `object`, `created`, `owned_by` on each
   item; `object`, `data` on the envelope). `created` is seconds, not
   milliseconds. `shutdown_date` is a date string or null, not a unix
   timestamp.

## Sources

- [List models](https://developers.openai.com/api/reference/resources/models/methods/list) — GET `/models`; summary; response fields including `shutdown_date`; curl; examples
- [Retrieve model](https://developers.openai.com/api/reference/resources/models/methods/retrieve) — GET `/models/{model}`; same `Model` object; "owner and permissioning" prose
- [Delete a fine-tuned model](https://developers.openai.com/api/reference/resources/models/methods/delete) — DELETE `/models/{model}`; Owner role; `ft:` example; `{id, deleted, object}`
- [Models resource](https://developers.openai.com/api/reference/resources/models) — combined list/retrieve/delete + domain types
- [API overview](https://developers.openai.com/api/reference/overview) — Bearer auth; org/project headers using `/v1/models` as the example; header size; `x-request-id` / rate-limit headers; `openai-version: 2020-10-01`; REST `v1`; backwards-compatible additive JSON properties; key revocation timing
- [Error codes](https://developers.openai.com/api/docs/guides/error-codes) — 401/403/429/500/503; `error.code` vs `error.type`; SDK exception map; timeout as `APITimeoutError`
- [Rate limits](https://developers.openai.com/api/docs/guides/rate-limits) — RPM/TPM; `Retry-After`; `slow_down` vs `server_is_overloaded`; do not retry quota/billing; SDK retries
- [Deprecations](https://developers.openai.com/api/docs/deprecations) — deprecation vs shutdown vs legacy; notice periods; shut-down means inaccessible; fine-tune inference until base-model deprecation
- [RBAC / permissions](https://developers.openai.com/api/docs/guides/rbac) — List models permission; `api.model.read` on `/v1/models`; key AND user must hold it; 30-minute role propagation; listing ≠ Model capabilities Request
- [Supervised fine-tuning](https://developers.openai.com/api/docs/guides/supervised-fine-tuning) — `ft:` model ids; platform wind-down; inference until base model deprecated
- [Production best practices](https://developers.openai.com/api/docs/guides/production-best-practices) — org header / default organization
- [OpenAPI spec (published)](https://github.com/openai/openai-openapi/blob/737397823478a9823937fe4ddf442a0446c379a8/openapi.yaml) — `servers[0].url = https://api.openai.com/v1`; `listModels` with no parameters ([L8740–L8819](https://github.com/openai/openai-openapi/blob/737397823478a9823937fe4ddf442a0446c379a8/openapi.yaml#L8740-L8819)); `ListModelsResponse` object+data only ([L43107–L43121](https://github.com/openai/openai-openapi/blob/737397823478a9823937fe4ddf442a0446c379a8/openapi.yaml#L43107-L43121)); `Model` + `shutdown_date` ([L44506–L44545](https://github.com/openai/openai-openapi/blob/737397823478a9823937fe4ddf442a0446c379a8/openapi.yaml#L44506-L44545)); `DeleteModelResponse` ([L38024–L38036](https://github.com/openai/openai-openapi/blob/737397823478a9823937fe4ddf442a0446c379a8/openapi.yaml#L38024-L38036)); `Error` / `ErrorResponse` ([L38382–L38427](https://github.com/openai/openai-openapi/blob/737397823478a9823937fe4ddf442a0446c379a8/openapi.yaml#L38382-L38427)); `ApiKeyAuth` bearer ([L88694–L88696](https://github.com/openai/openai-openapi/blob/737397823478a9823937fe4ddf442a0446c379a8/openapi.yaml#L88694-L88696))
- [openai-openapi README](https://github.com/openai/openai-openapi/blob/737397823478a9823937fe4ddf442a0446c379a8/README.md) — spec is the machine-readable REST description; SDKs generated from it
- [openai-python `Model`](https://github.com/openai/openai-python/blob/f38355ecdf69b231be218e572f591ce7f389a211/src/openai/types/model.py) — generated type: required `id`/`created`/`object`/`owned_by`, optional `shutdown_date`
- [openai-python README — Timeouts / Retries / Errors](https://github.com/openai/openai-python/blob/f38355ecdf69b231be218e572f591ce7f389a211/README.md) — 10-minute default timeout; retries; status→exception table
- [openai-node `models.ts`](https://github.com/openai/openai-node/blob/a3deafb5f91d21490ef25df2ffc05e6049a6820f/src/resources/models.ts) — list/retrieve/delete; `shutdown_date?: string | null`; comment "no pagination actually occurs yet"
- [openai-node README — Timeouts / Retries](https://github.com/openai/openai-node/blob/a3deafb5f91d21490ef25df2ffc05e6049a6820f/README.md) — 10-minute default timeout; retries

**Spec snapshot note:** Stainless's `openapi.documented.yml` (info.version
`2.3.0`, fetched the same day) described `Model` **without** `shutdown_date`.
The GitHub-published spec at
[`73739782`](https://github.com/openai/openai-openapi/blob/737397823478a9823937fe4ddf442a0446c379a8/openapi.yaml)
and both official SDKs include `shutdown_date`. Prefer the GitHub spec +
current API reference over that Stainless snapshot.
