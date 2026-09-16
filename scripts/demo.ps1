param(
    [string]$BaseUrl = "http://localhost:8080",
    [string]$ReceiverUrl = "http://receiver:9090",
    [string]$ReceiverHistoryUrl = "http://localhost:9090",
    [string]$ApiToken = "wr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
    [string]$ComposeProject = "",
    [string]$WSLDistro = ""
)

$ErrorActionPreference = "Stop"
$apiHeaders = @{ Authorization = "Bearer $ApiToken" }
foreach ($address in @($BaseUrl, $ReceiverHistoryUrl)) {
    if (-not ([uri]$address).IsLoopback) { throw "This demo only accepts local API and receiver-history URLs." }
}
if (([uri]$ReceiverUrl).Host -notin @("receiver", "localhost", "127.0.0.1")) {
    throw "ReceiverUrl must target the local synthetic receiver."
}

function Invoke-DemoAdmin {
    $dockerArgs = @('compose')
    if ($ComposeProject) { $dockerArgs += @('-p', $ComposeProject) }
    $dockerArgs += @('exec', '-T', 'api', 'admin') + $args
    if ($WSLDistro) { $result = & wsl.exe -d $WSLDistro -- docker @dockerArgs }
    else { $result = & docker @dockerArgs }
    if ($LASTEXITCODE -ne 0) { throw 'Local credential administration failed.' }
    $result | ConvertFrom-Json
}

function Send-DemoEvent([int]$Failures) {
    $endpoint = Invoke-RestMethod -Headers $apiHeaders -Method Post -Uri "$BaseUrl/v1/endpoints" -ContentType application/json -Body (@{
        url = "$ReceiverUrl/flaky?failures=$Failures"
        secret = "local-demo-secret-32-bytes-long"
        max_attempts = 3
        concurrency_limit = 1
        rate_limit = 5
    } | ConvertTo-Json)
    Invoke-RestMethod -Method Post -Uri "$BaseUrl/v1/endpoints/$($endpoint.id)/events" -ContentType application/json -Headers @{
        "X-Event-Type" = "demo.order"
        "Authorization" = "Bearer $ApiToken"
    } -Body '{"order_id":"demo-42","amount":1250}'
}

function Wait-ForState([string]$EventId, [string]$Expected) {
    $timer = [Diagnostics.Stopwatch]::StartNew()
    while ($timer.Elapsed.TotalSeconds -lt 30) {
        $event = Invoke-RestMethod -Headers $apiHeaders "$BaseUrl/v1/events/$EventId"
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
$history = (Invoke-RestMethod -Headers $apiHeaders "$BaseUrl/v1/events/$($eventual.id)/attempts").attempts
if ($history.Count -ne 3 -or ($history.response_status -join ",") -ne "500,500,204") {
    throw "Expected two failures followed by success."
}

$exhausted = Send-DemoEvent 3
$null = Wait-ForState $exhausted.id "dead_letter"
$deadHistory = (Invoke-RestMethod -Headers $apiHeaders "$BaseUrl/v1/events/$($exhausted.id)/attempts").attempts
if ($deadHistory.Count -ne 3) { throw "Expected three exhausted attempts." }
$replayBody = @{
    attempt_id = $deadHistory[-1].id
    request_id = [guid]::NewGuid().ToString()
    reason = "Synthetic receiver now accepts delivery"
} | ConvertTo-Json
$replay = Invoke-RestMethod -Headers $apiHeaders -Method Post -Uri "$BaseUrl/v1/events/$($exhausted.id)/replays" -ContentType application/json -Body $replayBody
$duplicate = Invoke-RestMethod -Headers $apiHeaders -Method Post -Uri "$BaseUrl/v1/events/$($exhausted.id)/replays" -ContentType application/json -Body $replayBody
if ($replay.attempt_id -ne $duplicate.attempt_id) { throw "Replay request was not idempotent." }
$null = Wait-ForState $exhausted.id "succeeded"
$replayed = (Invoke-RestMethod -Headers $apiHeaders "$BaseUrl/v1/events/$($exhausted.id)/attempts").attempts
if ($replayed.Count -ne 4 -or $replayed[-1].replay_of -ne $deadHistory[-1].id -or $replayed[-1].replay_actor -ne "demo-operator") {
    throw "Replay history or audit is incorrect."
}
if ($replayed[-1].replay_principal_id -ne '00000000-0000-4000-8000-000000000001') { throw 'Replay principal is missing.' }

$denied = Invoke-WebRequest -SkipHttpErrorCheck -Method Post -Uri "$BaseUrl/v1/events/$($exhausted.id)/replays" -ContentType application/json -Body $replayBody -Headers @{
    Authorization = 'Bearer wr_AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE'
}
if ($denied.StatusCode -ne 403) { throw 'Ingestion-only credential could replay.' }
$denied = Invoke-WebRequest -SkipHttpErrorCheck -Method Post -Uri "$BaseUrl/v1/endpoints" -Headers $apiHeaders -ContentType application/json -Body '{"url":"http://169.254.169.254/","secret":"synthetic-demo-secret"}'
if ($denied.StatusCode -ne 400) { throw 'Unapproved destination was accepted.' }

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
$metrics = (Invoke-WebRequest -Headers $apiHeaders "$BaseUrl/metrics").Content
if ($metrics -notmatch 'webhook_attempts_completed_total' -or $metrics -notmatch 'webhook_queue_depth') { throw "Delivery metrics are missing." }

$principal = Invoke-DemoAdmin create --name demo-revocation-check --kind service --permissions metrics
$issued = Invoke-DemoAdmin issue --principal $principal.id --ttl 1h
try {
    $probe = Invoke-WebRequest -SkipHttpErrorCheck "$BaseUrl/metrics" -Headers @{ Authorization = "Bearer $($issued.token)" }
    if ($probe.StatusCode -ne 200) { throw 'Issued credential was not accepted.' }
} finally {
    $null = Invoke-DemoAdmin revoke --credential $issued.credential_id
}
$probe = Invoke-WebRequest -SkipHttpErrorCheck "$BaseUrl/metrics" -Headers @{ Authorization = "Bearer $($issued.token)" }
if ($probe.StatusCode -ne 401) { throw 'Revoked credential was accepted.' }

@(
    [pscustomobject]@{ scenario = "retry recovery"; event_id = $eventual.id; state = "succeeded"; attempts = 3 }
    [pscustomobject]@{ scenario = "dead letter then replay"; event_id = $exhausted.id; state = "succeeded"; attempts = 4 }
) | Format-Table -AutoSize
Write-Output "Verified seven signed deliveries, unchanged bytes, traces, metrics, authenticated replay, permission denial, destination rejection, and credential revocation."
