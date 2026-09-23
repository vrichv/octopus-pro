#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://100.111.82.3:8080}"
API_KEY="${API_KEY:-}"
MODEL="${MODEL:-mimo-v2.6-flash-free}"
PROMPT="${PROMPT:-Reply with the single word ok}"
FIXTURE="${FIXTURE:-opencode-prefixed.json}"
OUTPUT="${OUTPUT:-/tmp/opencode-zen-${MODEL}.sse}"
SESSION="${OPENCODE_SESSION:-ses_f5081f13bffeEQAd7eIVRlYJYoz}"
REQUEST_ID="${OPENCODE_REQUEST:-msg_0af7e0f00001ZIkb8ZM6Z}"
CLIENT="${OPENCODE_CLIENT:-cli}"
USER_AGENT="${USER_AGENT:-opencode/1.18.32 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14}"
TIMEOUT="${TIMEOUT:-180}"
PROJECT="${OPENCODE_PROJECT:-global}"
if [[ -z "$API_KEY" ]]; then
    echo "API_KEY is required: this is the Octopus client API key (sk-octopus-...)." >&2
    echo "The upstream OpenCode key is configured in the Zen channel and may be empty/NULL." >&2
    exit 2
fi

if ! command -v curl >/dev/null 2>&1; then
    echo "curl is required" >&2
    exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
    echo "jq is required" >&2
    exit 1
fi
if [[ ! -f "$FIXTURE" ]]; then
    echo "fixture not found: $FIXTURE" >&2
    exit 1
fi

BASE_URL="${BASE_URL%/}"
if [[ "$BASE_URL" == */v1 ]]; then
    ENDPOINT="$BASE_URL/chat/completions"
else
    ENDPOINT="$BASE_URL/v1/chat/completions"
fi

BODY="$(mktemp)"
trap 'rm -f "$BODY"' EXIT

jq --arg model "$MODEL" --arg prompt "$PROMPT" \
    '.body
     | .model = $model
     | .messages = [.messages[0], {role: "user", content: $prompt}]
     | .stream = true
     | .stream_options = {include_usage: true}' \
    "$FIXTURE" >"$BODY"

HEADERS=(
    -H "Accept: */*"
    -H "Content-Type: application/json"
    -H "User-Agent: $USER_AGENT"
    -H "x-opencode-session: $SESSION"
    -H "x-opencode-request: $REQUEST_ID"
    -H "x-opencode-client: $CLIENT"
    -H "x-opencode-project: $PROJECT"
)
# API_KEY authenticates the request to Octopus. It is not the upstream Zen key.
HEADERS+=( -H "Authorization: Bearer $API_KEY" )

echo "POST $ENDPOINT" >&2
echo "model=$MODEL" >&2
echo "output=$OUTPUT" >&2

set +e
HTTP_STATUS="$(curl -sS -N --max-time "$TIMEOUT" \
    -o "$OUTPUT" -w '%{http_code}' \
    "$ENDPOINT" \
    "${HEADERS[@]}" \
    --data-binary "@$BODY")"
CURL_STATUS=$?
set -e

printf 'HTTP_STATUS=%s CURL_STATUS=%s\n' "$HTTP_STATUS" "$CURL_STATUS"
printf 'BYTES=%s DONE=%s STOP=%s\n' \
    "$(wc -c <"$OUTPUT")" \
    "$(grep -cF 'data: [DONE]' "$OUTPUT" || true)" \
    "$(grep -cF '"finish_reason":"stop"' "$OUTPUT" || true)"

cat "$OUTPUT"

if [[ "$CURL_STATUS" -ne 0 || "$HTTP_STATUS" != "200" ]]; then
    exit 1
fi
if ! grep -qF 'data: [DONE]' "$OUTPUT"; then
    echo "stream did not contain data: [DONE]" >&2
    exit 1
fi
