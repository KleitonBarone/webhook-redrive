param(
    [string]$BaseUrl = "http://localhost:8080",
    [string]$ReceiverUrl = "http://receiver:9090",
    [string]$ReceiverHistoryUrl = "http://localhost:9090"
)

$ErrorActionPreference = "Stop"
foreach ($address in @($BaseUrl, $ReceiverHistoryUrl)) {
    if (-not ([uri]$address).IsLoopback) { throw "This demo only accepts local API and receiver-history URLs." }
}
if (([uri]$ReceiverUrl).Host -notin @("receiver", "localhost", "127.0.0.1")) {
    throw "ReceiverUrl must target the local synthetic receiver."
}

function Send-DemoEvent([int]$Failures) {
    $endpoint = Invoke-RestMethod -Method Post -Uri "$BaseUrl/v1/endpoints" -ContentType application/json -Body (@{
        url = "$ReceiverUrl/flaky?failures=$Failures"
        secret = "local-demo-secret-32-bytes-long"
        max_attempts = 3
        concurrency_limit = 1
        rate_limit = 5
    } | ConvertTo-Json)
    Invoke-RestMethod -Method Post -Uri "$BaseUrl/v1/endpoints/$($endpoint.id)/events" -ContentType application/json -Headers @{
        "X-Event-Type" = "demo.order"
    } -Body '{"order_id":"demo-42","amount":1250}'
}

function Wait-ForState([string]$EventId, [string]$Expected) {
    $timer = [Diagnostics.Stopwatch]::StartNew()
    while ($timer.Elapsed.TotalSeconds -lt 30) {
        $event = Invoke-RestMethod "$BaseUrl/v1/events/$EventId"
        if ($event.state -eq $Expected) { return $event }
        if ($event.state -in @("succeeded", "failed", "dead_letter")) {
            throw "Event $EventId reached $($event.state), expected $Expected."
        }
        Start-Sleep -Milliseconds 200
    }
    throw "Timed out waiting for $EventId to reach $Expected."
}

$eventual = Send-DemoEvent 2
$null = Wait-ForState $eventual.id "succeeded"
$history = (Invoke-RestMethod "$BaseUrl/v1/events/$($eventual.id)/attempts").attempts
if ($history.Count -ne 3 -or ($history.response_status -join ",") -ne "500,500,204") {
    throw "Expected two failures followed by success."
}

$exhausted = Send-DemoEvent 3
$null = Wait-ForState $exhausted.id "dead_letter"
$deadHistory = (Invoke-RestMethod "$BaseUrl/v1/events/$($exhausted.id)/attempts").attempts
if ($deadHistory.Count -ne 3) { throw "Expected three exhausted attempts." }
$replayBody = @{
    attempt_id = $deadHistory[-1].id
    request_id = [guid]::NewGuid().ToString()
    actor = "demo-operator"
    reason = "Synthetic receiver now accepts delivery"
} | ConvertTo-Json
$replay = Invoke-RestMethod -Method Post -Uri "$BaseUrl/v1/events/$($exhausted.id)/replays" -ContentType application/json -Body $replayBody
$duplicate = Invoke-RestMethod -Method Post -Uri "$BaseUrl/v1/events/$($exhausted.id)/replays" -ContentType application/json -Body $replayBody
if ($replay.attempt_id -ne $duplicate.attempt_id) { throw "Replay request was not idempotent." }
$null = Wait-ForState $exhausted.id "succeeded"
$replayed = (Invoke-RestMethod "$BaseUrl/v1/events/$($exhausted.id)/attempts").attempts
if ($replayed.Count -ne 4 -or $replayed[-1].replay_of -ne $deadHistory[-1].id -or $replayed[-1].replay_actor -ne "demo-operator") {
    throw "Replay history or audit is incorrect."
}

$received = (Invoke-RestMethod "$ReceiverHistoryUrl/deliveries").deliveries |
    Where-Object { $_.event_id -in @($eventual.id, $exhausted.id) }
if (@($received).Count -ne 7 -or @($received | Where-Object { -not $_.signature_valid }).Count -gt 0) {
    throw "Expected seven deliveries with valid signatures."
}
$digest = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData(
    [Text.Encoding]::UTF8.GetBytes('{"order_id":"demo-42","amount":1250}')
)).ToLowerInvariant()
if (@($received | Where-Object { $_.body_sha256 -ne $digest }).Count -gt 0) { throw "Body bytes changed in transit." }
if (@($received | Where-Object { $_.trace_id -notmatch '^[0-9a-f]{32}$' }).Count -gt 0) { throw "Delivery trace context is missing." }
$metrics = (Invoke-WebRequest "$BaseUrl/metrics").Content
if ($metrics -notmatch 'webhook_attempts_completed_total' -or $metrics -notmatch 'webhook_queue_depth') { throw "Delivery metrics are missing." }

@(
    [pscustomobject]@{ scenario = "retry recovery"; event_id = $eventual.id; state = "succeeded"; attempts = 3 }
    [pscustomobject]@{ scenario = "dead letter then replay"; event_id = $exhausted.id; state = "succeeded"; attempts = 4 }
) | Format-Table -AutoSize
Write-Output "Verified seven signed deliveries, unchanged body bytes, trace context, metrics, and one audited replay."
