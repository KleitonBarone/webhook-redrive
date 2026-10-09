param(
    [string]$WSLDistro = '',
    [switch]$HostBuildNetwork,
    [switch]$GoChecks,
    [string]$Subnet = '',
    [int]$TLSPort = 18443
)
$ErrorActionPreference = 'Stop'
if (($Subnet -and $Subnet -notmatch '^10\.\d{1,3}\.\d{1,3}\.0/24$') -or $TLSPort -lt 1024 -or $TLSPort -gt 65535) {
    throw 'Use an unused explicit private /24 subnet and a nonprivileged loopback TLS port.'
}
$project = "webhook-redrive-adoption-$([guid]::NewGuid().ToString('N').Substring(0,8))"
$local = Join-Path (Get-Location) "artifacts/adoption/$project"
$variables = @('ADOPTION_PROJECT','ADOPTION_LOCAL_DIRECTORY','ADOPTION_DATABASE_PASSWORD','ADOPTION_MASTER_KEY','ADOPTION_SUBNET','ADOPTION_TLS_PORT')
$previous = @{}
foreach ($name in $variables) { $previous[$name] = [Environment]::GetEnvironmentVariable($name) }

function Invoke-Docker {
    if ($WSLDistro) { & wsl.exe -d $WSLDistro --exec docker @args }
    else { & docker @args }
    if ($LASTEXITCODE -ne 0) { throw 'Adoption Docker command failed.' }
}
function Invoke-Compose {
    # WSL does not inherit arbitrary Windows environment variables. Interpolation
    # reads the private generated file; credentials never appear in CLI arguments.
    Invoke-Docker compose --env-file "$dockerLocal/compose.env" -p $project -f examples/typescript/compose.yml @args
}
function Invoke-Consumer([string]$phase) { Invoke-Compose exec -T consumer node client.ts $phase }
function Write-Private([string]$path, [string]$value) {
    [IO.File]::WriteAllText($path, $value, [Text.UTF8Encoding]::new($false))
    if (-not $IsWindows) { & chmod 600 $path }
}
function New-Random { [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)).ToLowerInvariant() }
function IPv4-Number([string]$address) {
    $bytes = [Net.IPAddress]::Parse($address).GetAddressBytes()
    return [long]$bytes[0]*16777216 + [long]$bytes[1]*65536 + [long]$bytes[2]*256 + $bytes[3]
}
function Invoke-Certificate {
    Invoke-Docker run --rm --network none -v "${dockerLocal}:/local" --entrypoint openssl golang:1.26.9-bookworm @args 2>$null
    if ($LASTEXITCODE -ne 0) { throw 'Local certificate generation failed.' }
}

$started = $false
try {
    if (-not $Subnet) {
        $networks = @(Invoke-Docker network ls -q)
        $occupied = @(Invoke-Docker network inspect --format '{{range .IPAM.Config}}{{println .Subnet}}{{end}}' @networks | Where-Object { $_ -match '^\d+\.' })
        foreach ($octet in 204..254) {
            $candidate = "10.231.$octet.0/24"
            $start = IPv4-Number "10.231.$octet.0"
            $overlaps = @($occupied | Where-Object {
                $parts = $_.Split('/')
                $low = IPv4-Number $parts[0]
                $high = $low + [Math]::Pow(2,32-[int]$parts[1])-1
                $start -le $high -and $start+255 -ge $low
            })
            if ($overlaps.Count -eq 0) { $Subnet = $candidate; break }
        }
        if (-not $Subnet) { throw 'No unused rehearsal subnet found; supply -Subnet explicitly.' }
    }
    $null = New-Item -ItemType Directory -Path $local
    $dockerLocal = $local
    $dockerRepo = (Get-Location).Path.Replace('\','/')
    if ($WSLDistro) {
        $dockerLocal = (& wsl.exe -d $WSLDistro --exec wslpath -a $local.Replace('\','/') | Out-String).Trim()
        if ($LASTEXITCODE -ne 0 -or $dockerLocal -notmatch '^/') { throw 'Unable to resolve the local WSL artifact path.' }
        $dockerRepo = (& wsl.exe -d $WSLDistro --exec wslpath -a $dockerRepo | Out-String).Trim()
    }
    $env:ADOPTION_PROJECT = $project
    $env:ADOPTION_LOCAL_DIRECTORY = $dockerLocal.Replace('\','/')
    $env:ADOPTION_DATABASE_PASSWORD = New-Random
    $env:ADOPTION_MASTER_KEY = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
    $env:ADOPTION_SUBNET = $Subnet
    $env:ADOPTION_TLS_PORT = "$TLSPort"
    Write-Private "$local/compose.env" (($variables | ForEach-Object { "$_=$([Environment]::GetEnvironmentVariable($_))" }) -join "`n")
    $secret = New-Random
    $control = New-Random
    Write-Private "$local/consumer.json" (@{ secret = $secret; control_token = $control } | ConvertTo-Json)
    Write-Private "$local/checks.env" "TEST_DATABASE_URL=postgres://webhook_redrive:$($env:ADOPTION_DATABASE_PASSWORD)@postgres:5432/webhook_redrive?sslmode=disable"
    Write-Private "$local/destinations.json" (ConvertTo-Json -InputObject @(@{ origin = 'https://consumer:8443'; private_networks = @($Subnet) }) -Depth 4)
    if (-not $IsWindows) { & chmod 644 "$local/destinations.json" }
    Invoke-Certificate req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=synthetic-adoption-ca -keyout /local/ca.key -out /local/ca.crt
    Invoke-Certificate req -newkey rsa:2048 -nodes -subj /CN=consumer -keyout /local/server.key -out /local/server.csr
    Write-Private "$local/server.ext" "subjectAltName=DNS:consumer,DNS:localhost`nbasicConstraints=CA:FALSE`nkeyUsage=digitalSignature,keyEncipherment`nextendedKeyUsage=serverAuth"
    Invoke-Certificate x509 -req -in /local/server.csr -CA /local/ca.crt -CAkey /local/ca.key -CAcreateserial -days 1 -extfile /local/server.ext -out /local/server.crt
    $buildNetwork = @()
    if ($HostBuildNetwork) { $buildNetwork = @('--network','host') }
    foreach ($service in @('api','worker')) {
        $null = Invoke-Docker build --pull @buildNetwork --target $service -t "${project}-$service" .
    }
    $started = $true
    $null = Invoke-Compose up -d postgres --pull always --wait
    $tables = (Invoke-Compose exec -T postgres psql -U webhook_redrive -d webhook_redrive -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'" | Out-String).Trim()
    if ($tables -ne '0') { throw 'Fresh adoption database was not empty.' }
    $null = Invoke-Compose up -d api --wait
    $credentials = @{}
    foreach ($identity in @(@{name='producer';permissions='ingest'},@{name='operator';permissions='inspect,endpoints,replay,metrics'})) {
        $principal = Invoke-Compose exec -T api admin create --name "adoption-$($identity.name)" --permissions $identity.permissions | ConvertFrom-Json
        $issued = Invoke-Compose exec -T api admin issue --principal $principal.id --ttl 1h | ConvertFrom-Json
        $credentials[$identity.name] = $issued.token
        if ($identity.name -eq 'operator') { $credentials.operator_principal = $principal.id }
    }
    Write-Private "$local/credentials.json" ($credentials | ConvertTo-Json)
    $null = Invoke-Compose up -d consumer worker --wait
    $null = Invoke-Consumer setup
    $null = Invoke-Consumer hold
    $worker = (Invoke-Compose ps -q worker | Out-String).Trim()
    $null = Invoke-Compose kill -s SIGKILL worker
    $exit = (Invoke-Docker inspect --format '{{.State.ExitCode}}' $worker | Out-String).Trim()
    if ($exit -ne '137') { throw 'Worker did not stop by SIGKILL.' }
    $null = Invoke-Compose restart consumer
    $null = Invoke-Compose up -d consumer --wait
    $null = Invoke-Compose start worker
    $null = Invoke-Consumer recovered
    $null = Invoke-Consumer queued
    $null = Invoke-Compose stop postgres
    $null = Invoke-Consumer unavailable
    $null = Invoke-Compose up -d postgres --wait
    $null = Invoke-Compose up -d api --wait
    $null = Invoke-Consumer database-recovered
    $null = Invoke-Consumer receiver-recovered
    $report = Invoke-Consumer report | ConvertFrom-Json
    if ($GoChecks) {
        Invoke-Docker run --rm --network host -v "${dockerRepo}:/src:ro" -v webhook-redrive-adoption-go-cache:/go -w /src golang:1.26.9-bookworm sh -ec 'go mod download; go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -version'
        Invoke-Docker run --rm --network "${project}_default" -v "${dockerRepo}:/src:ro" -v webhook-redrive-adoption-go-cache:/go --env-file "$dockerLocal/checks.env" -w /src golang:1.26.9-bookworm sh -ec 'go version; gofmt -l cmd internal migrations signature examples > /tmp/unformatted; test ! -s /tmp/unformatted; go vet ./...; go test -race -count=1 ./...; MEASURE_HISTORY=true go test -race -count=1 -v ./internal/store -run TestOperations; go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...'
    }
    $logs = Invoke-Compose logs --no-color | Out-String
    foreach ($sensitive in @($secret,$control,$env:ADOPTION_MASTER_KEY,$env:ADOPTION_DATABASE_PASSWORD,$credentials.producer,$credentials.operator,'SYNTHETIC_PAYLOAD_DO_NOT_LOG')) {
        if ($logs.Contains($sensitive)) { throw 'Sensitive value found in adoption logs.' }
    }
    $revision = 'unavailable'
    $modified = $null
    if (Get-Command git -ErrorAction SilentlyContinue) {
        $gitRevision = (git rev-parse HEAD 2>$null | Out-String).Trim()
        if ($LASTEXITCODE -eq 0) {
            $revision = $gitRevision
            $modified = ((git status --porcelain | Out-String).Trim().Length -gt 0)
        }
    }
    $result = [ordered]@{
        revision = $revision
        verified_at_utc = [DateTime]::UtcNow.ToString('o')
        worktree_modified = $modified
        project = $project
        subnet = $Subnet
        author_performed = $true
        fresh_database = $true
        public_demo_credential_rejected = $true
        worker_sigkill_exit_code = [int]$exit
        receiver_process_restarted = $true
        logs_redacted = $true
        go_checks = [bool]$GoChecks
        go_mod_sha256 = (Get-FileHash go.mod -Algorithm SHA256).Hash.ToLowerInvariant()
        api_image = (Invoke-Docker inspect --format '{{.Id}}' "${project}-api" | Out-String).Trim()
        worker_image = (Invoke-Docker inspect --format '{{.Id}}' "${project}-worker" | Out-String).Trim()
        contract = $report
    }
    $result | ConvertTo-Json -Depth 5 | Set-Content "$local/result.json"
    $result | ConvertTo-Json -Depth 5
} finally {
    if ($started) { $null = Invoke-Compose stop }
    foreach ($name in $variables) { [Environment]::SetEnvironmentVariable($name, $previous[$name]) }
}
