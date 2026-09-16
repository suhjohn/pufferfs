# Inline media and DeepSeek vision fallback

## Findings

The repository's Gemini key works for synchronous image inference and for
Batch requests containing inline image bytes. In a real four-item batch using
the same key and uploaded image, the text and inline-image items succeeded;
both camelCase and snake_case file-URI items returned permission-denied code 7.
The uploaded file remained ACTIVE. This isolates the observed failure to Batch
file-reference authorization, without establishing Google's internal cause.

Gemini batch JSONL now contains the actual base64 page/clip bytes. Each range of
up to 64 inputs needs one upload instead of up to 65. Per-media upload IDs,
readiness polling, temporary upload copies and upload concurrency are removed.
The JSONL still needs upload expiry, cleanup and crash-recovery tracking. Old
manifests remain readable so their existing uploads can be cleaned.

## DeepSeek experiment

[Modal's model listing](https://modal.com/library/deepseek/deepseek-v4-1-flash)
confirms native image input and a shared OpenAI-compatible endpoint. Shared
endpoints use a proxy token, not the Modal CLI's token pair. See
[shared endpoint setup](https://modal.com/docs/guide/shared-endpoints).

A standalone scratch script sent a synthetic inventory image as an inline PNG
data URL to `deepseek-ai/DeepSeek-V4.1-Flash`, with `reasoning_effort: none`.
It returned the heading and a Markdown table containing the correct values
17, 42 and 59. The response finished normally in 0.53 seconds: 627 input tokens,
46 output tokens, zero reasoning tokens. At the listed rates, that is about
$0.00024. This is one observation, not a latency or accuracy benchmark.

Only after that successful request was the collector fallback implemented.
Failed image items are regenerated from verified retained source bytes and
sent to Modal. Successful Gemini siblings are preserved. The manifest records
each result's provider and model; assembly retains original page/frame order.
Audio uses Gemini. Status retrieval failures and ambiguous submissions retain
the existing recovery path. See [configuration and limits](configuration.md#image-extraction-fallback)
and [deployment roles and queues](provider-batch-manifests.md#roles-and-deployment).

## Validation and deployment

The standalone provider probe, partial/whole-batch vision fallback and
root-deletion/submission-discovery E2Es passed. Deletion
recovered the accepted job, acknowledged deletion of the one JSONL upload and
verified no index resurrection. Discovery resumed its persisted cursor after a
collector restart, recovered the original job without reupload/resubmission,
verified public page read/search and deleted the one upload. Partial fallback
preserved the two successful Gemini pages, recovered exactly the two failed
pages on Modal, survived a collector kill before database publication, verified
all four pages through public read/search, and acknowledged deletion of the
one JSONL upload. Whole-batch fallback recovered all four pages after real
Google cancellation errors, with the same restart, public read/search and
cleanup checks. The original Gemini jobs were never resubmitted.

The broader media run timed out at its one-hour polling deadline. At the last
status check, 24 of 28 batches had completed on attempt one and four remained
pending at Google. Provider recovery also timed out waiting for its first 17-page batch
after the input-publication crash/restart. It did not reach the 65-page boundary,
lost accepted-response or partial-retry stages. Both suites exited with failure;
their isolated-resource cleanup phases passed. Neither suite is claimed as a
pass. A separate post-cleanup DELETE of one media run upload returned HTTP 403;
that is not proof of deletion. The completed fallback, deletion and discovery
suites did verify acknowledged upload deletion.

The full-corpus run also timed out and exited with failure; cleanup passed.
It passed handoff outage/recovery, native capture/transformation/replay, follow
backlog, corpus capture and multipart recovery. Seven of ten provider batches
completed (six on attempt one, one on attempt two), with acknowledged upload
cleanup. The three outstanding JPG, MP3 and PowerPoint jobs still reported
`BATCH_STATE_RUNNING` with every item pending and no error at 03:58 UTC, just
before the deadline. Evidence is in
`tests/e2e/artifacts/inline-corpus-final-provider-state.json`. The remaining
corpus content/authorization assertions, update/delete/outage and malformed
delivery phases were not reached. This is not a full-corpus pass.

At 03:38 UTC on September 16, direct Google status reads showed all 11
outstanding jobs as `BATCH_STATE_RUNNING`, with every item pending and no
operation error. This evidence is retained in
`tests/e2e/artifacts/inline-provider-wait.json`. Google's
[Batch API documentation](https://ai.google.dev/gemini-api/docs/batch-api)
allows a target turnaround of 24 hours; the local E2E polling deadline is one
hour. Slow nonterminal batches do not trigger vision fallback. A timeout of
that test deadline does not establish an inference failure.

The corpus run also exposed an audio contract bug: a 4.9206875-second clip was
described by the prompt as exactly 4.92069 seconds because Python's `:g`
formatter rounded it. Gemini returned that endpoint with a normal STOP result;
the validator correctly rejected it as beyond the real clip. The prompt now
preserves the full duration and both timestamp fields carry numeric minimum/
maximum bounds in the response schema, using Google's documented
[Schema constraints](https://ai.google.dev/api/generate-content#Schema).
The corpus collector was rebuilt and restarted with this fix; the ordinary
second attempt succeeded. A separate E2E driver verified publication of that
extraction, the expected transcript, exact timestamp bounds in the stored
chunks and public search. Both attempts' JSONL uploads had acknowledged
deletion. Evidence is in
`tests/e2e/artifacts/inline-audio-retry.json`. No timestamp tolerance or
fixture-specific exception was introduced.

Two initial vision runs ended before fallback: a ten-minute harness deadline
and a transient Google status-read timeout. Cleanup passed for both. The
harness now uses the suite's normal provider timeout while leaving status
polling and transport retries to the production collector.

The cancellation case also exposed an invalid test assumption: Google may
report the batch as SUCCEEDED while every item contains cancellation error
code 1. The collector recovered all four pages and public reads/search passed;
the original test then failed because it required the job-level CANCELLED
label. The corrected validator checks the expected cancellation code at the
operation or item level, consistent with Google's best-effort cancellation
contract; the rerun passed.

Modal's endpoint Usage page reported $0.01 during verification, below the
approved $1 test cap.

The authenticated Modal shared endpoint has been created, and local ignored
`.env` has the configuration. Production PufferFS workers have not been
deployed with this change. The deploy workflow now supports the required
endpoint URL/model variables and proxy-token secret. No CLI behavior or package
version changed.
Deploy the collector before transformation workers: it accepts both old and
new input manifests. The deployment workflow now follows that order.
