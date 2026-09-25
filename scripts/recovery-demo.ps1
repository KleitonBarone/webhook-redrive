param(
    [Parameter(Mandatory)][string]$SourceProject,
    [string]$WSLDistro = "",
    [switch]$HostBuildNetwork
)
$ErrorActionPreference = 'Stop'
if ($SourceProject -notmatch '^webhook-redrive-[a-z0-9-]+$') { throw 'Use an explicitly named webhook-redrive-* synthetic Compose project.' }
$target = "$SourceProject-restore-$([guid]::NewGuid().ToString('N').Substring(0,8))"
$headers = @{ Authorization = 'Bearer wr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' }
$oldKey = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='
$newKey = 'AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE='
$source = 'http://localhost:8080'
$restored = 'http://localhost:18080'

function Docker {
    if ($WSLDistro) { & wsl.exe -d $WSLDistro -- docker @args }
    else { & docker @args }
    if ($LASTEXITCODE -ne 0) { throw 'Recovery drill Docker command failed.' }
}
function SourceCompose { Docker compose -p $SourceProject @args }
function TargetCompose { Docker compose -p $target -f compose.yml -f compose.recovery.yml @args }
function Post([string]$url, $body) { Invoke-RestMethod -Method Post -Uri $url -Headers $headers -ContentType application/json -Body ($body | ConvertTo-Json -Depth 6) }
function Wait-Success([string]$base, [string]$id) {
    $watch = [Diagnostics.Stopwatch]::StartNew()
    while ($watch.Elapsed.TotalSeconds -lt 30) {
        $event = Invoke-RestMethod -Headers $headers "$base/v1/events/$id"
        if ($event.state -eq 'succeeded') { return }
        Start-Sleep -Milliseconds 200
    }
    throw 'Restored event did not succeed.'
}

$sourceStopped = $false
try {
    # Source must already be a running synthetic stack. This script never accepts a DB URL.
    $endpoint = Post "$source/v1/endpoints" @{ url = 'http://receiver:9090/success'; secret = 'local-demo-secret-32-bytes-long'; retry_profile = 'demo' }
    $body = '{ "order_id": "synthetic-restore", "amount": 123 }'
    $eventHeaders = $headers.Clone()
    $eventHeaders['X-Event-Type'] = 'synthetic.restore'
    $eventHeaders['Idempotency-Key'] = [guid]::NewGuid().ToString()
    $finished = Invoke-RestMethod -Method Post -Uri "$source/v1/endpoints/$($endpoint.id)/events" -Headers $eventHeaders -ContentType application/json -Body $body
    Wait-Success $source $finished.id
    $history = (Invoke-RestMethod -Headers $headers "$source/v1/events/$($finished.id)/attempts").attempts | ConvertTo-Json -Depth 6 -Compress
    $endpoint = Post "$source/v1/endpoints/$($endpoint.id)/pause" @{ expected_version = $endpoint.version; reason = 'Synthetic restore drill' }
    $eventHeaders['Idempotency-Key'] = [guid]::NewGuid().ToString()
    $pending = Invoke-RestMethod -Method Post -Uri "$source/v1/endpoints/$($endpoint.id)/events" -Headers $eventHeaders -ContentType application/json -Body $body
    $sourceStopped = $true
    $null = SourceCompose stop api worker
    $serviceQuery = "SELECT json_build_object('sequence_value',(SELECT last_value FROM endpoint_service_sequence),'sequence_called',(SELECT is_called FROM endpoint_service_sequence),'endpoints',coalesce((SELECT jsonb_object_agg(id,last_service_seq) FROM webhook_endpoints),'{}'::jsonb))"
    $serviceOrder = (SourceCompose exec -T postgres psql -U webhook_redrive -d webhook_redrive -At -v ON_ERROR_STOP=1 -c $serviceQuery | Out-String).Trim()
    $null = SourceCompose exec -T postgres pg_dump -U webhook_redrive -d webhook_redrive -Fc --no-owner --no-acl -f /tmp/synthetic-recovery.dump
    $null = TargetCompose up -d postgres --wait
    $sourceID = (SourceCompose ps -q postgres | Out-String).Trim()
    $targetID = (TargetCompose ps -q postgres | Out-String).Trim()
    # The target name is random and its database must still be empty.
    $tables = (TargetCompose exec -T postgres psql -U webhook_redrive -d webhook_redrive -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'" | Out-String).Trim()
    if ($tables -ne '0') { throw 'Refusing to restore into a nonempty database.' }
    $artifact = "artifacts/recovery/$target"
    $null = New-Item -ItemType Directory -Path $artifact
    Docker cp "${sourceID}:/tmp/synthetic-recovery.dump" "$artifact/backup.dump"
    Docker cp "$artifact/backup.dump" "${targetID}:/tmp/synthetic-recovery.dump"
    $null = TargetCompose exec -T postgres pg_restore -U webhook_redrive -d webhook_redrive --exit-on-error --no-owner --no-acl /tmp/synthetic-recovery.dump
    $restoredOrder = (TargetCompose exec -T postgres psql -U webhook_redrive -d webhook_redrive -At -v ON_ERROR_STOP=1 -c $serviceQuery | Out-String).Trim()
    if ($serviceOrder -ne $restoredOrder) { throw 'Restore changed endpoint service order or its sequence.' }
    # Verify the restored ciphertext with the backup's key, then atomically rotate
    # it before any restored service starts. Endpoint signing secrets do not change.
    foreach ($service in @('api','worker','receiver')) {
        $network = @()
        if ($HostBuildNetwork) { $network = @('--network','host') }
        $null = Docker build @network --target $service -t "${target}-$service" .
    }
    $preview = TargetCompose run --rm --no-deps -e "MASTER_KEY=$oldKey" -e "NEW_MASTER_KEY=$newKey" --entrypoint admin api rotate-master-key --offline | ConvertFrom-Json
    if ($preview.applied -or $preview.endpoints -lt 1) { throw 'Invalid rotation preview.' }
    $rotation = TargetCompose run --rm --no-deps -e "MASTER_KEY=$oldKey" -e "NEW_MASTER_KEY=$newKey" --entrypoint admin api rotate-master-key --offline --apply | ConvertFrom-Json
    if (-not $rotation.applied) { throw 'Rotation not applied.' }
    $null = TargetCompose up -d --wait --no-build
    $preserved = (Invoke-RestMethod -Headers $headers "$restored/v1/events/$($finished.id)/attempts").attempts | ConvertTo-Json -Depth 6 -Compress
    if ($history -ne $preserved) { throw 'Restore changed completed attempt history.' }
    $receipt = Invoke-WebRequest -Method Post -Uri "$restored/v1/endpoints/$($endpoint.id)/events" -Headers $eventHeaders -ContentType application/json -Body $body
    if ($receipt.Headers['Idempotency-Replayed'] -ne 'true' -or ($receipt.Content | ConvertFrom-Json).id -ne $pending.id) { throw 'Restore lost the ingestion receipt.' }
    $null = Post "$restored/v1/endpoints/$($endpoint.id)/resume" @{ expected_version = $endpoint.version; reason = 'Synthetic restore verified' }
    Wait-Success $restored $pending.id
    $deliveries = @((Invoke-RestMethod 'http://localhost:19090/deliveries').deliveries | Where-Object event_id -eq $pending.id)
    $digest = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($body))).ToLowerInvariant()
    if ($deliveries.Count -ne 1 -or -not $deliveries[0].signature_valid -or $deliveries[0].body_sha256 -ne $digest) { throw 'Restored delivery changed bytes or signing key.' }
    $afterOrder = (TargetCompose exec -T postgres psql -U webhook_redrive -d webhook_redrive -At -v ON_ERROR_STOP=1 -c $serviceQuery | Out-String).Trim() | ConvertFrom-Json
    if ($afterOrder.endpoints.($endpoint.id) -le ($serviceOrder | ConvertFrom-Json).sequence_value) { throw 'Restored delivery did not advance service order beyond the backup sequence.' }
    [pscustomobject]@{ source_project = $SourceProject; restore_project = $target; history_preserved = $true; keyed_receipt_preserved = $true; wrapping_key_rotated = $true; pending_delivered = $true; signature_valid = $true; service_order_preserved = $true; service_sequence_advanced = $true; dump = "$artifact/backup.dump" } | ConvertTo-Json
} finally {
    $null = TargetCompose stop
    if ($sourceStopped) { $null = SourceCompose start api worker }
}
