# Qwen-Audio-Turbo Billing & Availability Research

**Date:** August 28, 2026
**Topic:** `qwen-audio-turbo` HTTP 403 `Throttling.AllocationQuota` / `Free allocated quota exceeded` error analysis.

## Findings

### 1. The Error Meaning
The error `Throttling.AllocationQuota` / `Free allocated quota exceeded` for `qwen-audio-turbo` means exactly what it says, but with a critical caveat: **this model does not support paid usage at all.**

According to the official Alibaba Cloud Model Studio (Bailian) documentation:
> "For free trial only: The Qwen-Audio model is currently available for free trial only. After your free quota is used up, you can no longer call the model. Payment is not supported."

### 2. Postpaid / Billing Support
**`qwen-audio-turbo` DOES NOT support postpaid billing.**
Even if a user enables postpaid billing or tops up their account, it will not unlock further usage for this specific model. The documentation explicitly states:
> "The Qwen-Audio model is available for free trial only, has calling quota limits, and does not have a paid option."

The free quota is:
- 100,000 tokens
- Valid for 90 days after activating Model Studio

Once this is exhausted, the model is permanently locked for that account/workspace.

### 3. Model Status & Replacements
The `qwen-audio-turbo` model is effectively a legacy/trial-only model. Alibaba Cloud explicitly recommends migrating to the **Qwen-Omni** family for production environments.

> "If you plan to use the audio understanding feature in a production environment, we recommend that you migrate to the Qwen-Omni model."

Specifically, the documentation notes that the older `qwen-omni-turbo` is also no longer updated, and recommends:
- **`qwen3.5-omni`** series
- **`qwen3-omni-flash`** series

### 4. Region Restrictions
`qwen-audio-turbo` is only available in the China (Beijing) region.

## Diagnosis & Next Steps

**Diagnosis:** The user has exhausted the 100,000 token free trial for `qwen-audio-turbo`. Because the model has no paid tier, enabling postpaid billing has no effect on this specific model. The API key works for `qwen-plus` and `qwen-vl-max` because those models *do* support postpaid billing.

**Action Required:**
Do not wait for billing propagation; it will never work for this model.
We must request the user to switch to a supported Omni model (e.g., `qwen3.5-omni` or `qwen3-omni-flash`) for audio input tasks.

## Sources
- [Audio understanding (Qwen-Audio) - Aliyun Help Center](https://help.aliyun.com/en/model-studio/audio-language-model)
- [多种音频内容的理解识别分析-通义千问Audio - Aliyun Help Center](https://help.aliyun.com/zh/model-studio/audio-language-model)
