param(
    [ValidateSet('success','saturation')][string]$Scenario = 'success',
    [ValidateRange(40,10000)][int]$Events = 3000,
    [ValidateRange(0,1000)][int]$Rate = 50,
    [ValidateRange(1,10)][int]$SlowEndpoints = 1,
    [ValidateRange(1,10)][int]$SlowConcurrency = 10,
    [string]$OutputDirectory = 'artifacts/benchmark',
    [string]$WSLDistro = ''
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path $PSScriptRoot -Parent
Set-Location $repo
$revision = (git rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0) { throw 'Cannot identify source revision' }
if (git status --porcelain -- cmd internal migrations Dockerfile compose.yml compose.benchmark.yml scripts) {
    $revision += '-dirty'
}
$project = 'webhook-measure-' + [guid]::NewGuid().ToString('N').Substring(0,12)
New-Item -ItemType Directory -Force $OutputDirectory | Out-Null
$output = (Resolve-Path -LiteralPath $OutputDirectory).Path
$runDirectory = Join-Path $output $project
New-Item -ItemType Directory $runDirectory | Out-Null
$previousOutput = $env:BENCHMARK_OUTPUT_DIR
$env:BENCHMARK_OUTPUT_DIR = $runDirectory

function Invoke-Docker {
    if ($WSLDistro) { $result = & wsl.exe -d $WSLDistro -- env "BENCHMARK_OUTPUT_DIR=$env:BENCHMARK_OUTPUT_DIR" docker @args }
    else { $result = & docker @args }
    if ($LASTEXITCODE -ne 0) { throw "Docker command failed: $($args[0])" }
    $result
}

if ($WSLDistro) {
    $wslOutput = & wsl.exe -d $WSLDistro -- wslpath -a $runDirectory.Replace('\','/')
    if ($LASTEXITCODE -ne 0) { throw 'Cannot resolve WSL output directory' }
    $env:BENCHMARK_OUTPUT_DIR = $wslOutput.Trim()
}
$compose = @('compose','-p',$project,'-f','compose.yml','-f','compose.benchmark.yml')
$samplerJob = $null
try {
    Write-Host "Measuring on new local project $project; data will be preserved."
    Invoke-Docker @compose up --build -d --wait
    Invoke-Docker @compose build loadtest
    Invoke-Docker @compose exec -T postgres psql -U webhook_redrive -d webhook_redrive -v ON_ERROR_STOP=1 -c 'CREATE EXTENSION IF NOT EXISTS pg_stat_statements'
    $before = (Invoke-Docker @compose exec -T postgres psql -U webhook_redrive -d webhook_redrive -At -v ON_ERROR_STOP=1 -f /benchmark-stats.sql) | ConvertFrom-Json
    $containers = @(Invoke-Docker @compose ps -q postgres api worker receiver)
    # Resource sampling is observational and adds its own CPU/CLI overhead.
    $samplerJob = Start-Job -ArgumentList $WSLDistro,($containers -join ',') -ScriptBlock {
        param($distro,$ids)
        $targets = $ids.Split(',')
        while ($true) {
            $at = [DateTimeOffset]::UtcNow.ToString('o')
            if ($distro) { $lines = & wsl.exe -d $distro -- docker stats --no-stream --format '{{json .}}' @targets }
            else { $lines = & docker stats --no-stream --format '{{json .}}' @targets }
            if ($LASTEXITCODE -ne 0) { throw 'Resource sampling failed' }
            foreach ($line in $lines) {
                [pscustomobject]@{ captured_at=$at; container=($line | ConvertFrom-Json) }
            }
            Start-Sleep -Seconds 5
        }
    }
    Invoke-Docker @compose run --rm --no-deps loadtest -scenario $Scenario -events $Events -rate $Rate -slow-endpoints $SlowEndpoints -slow-concurrency $SlowConcurrency -sample-interval 1s -deadline 10m -revision $revision -output /evidence/load.json
    $after = (Invoke-Docker @compose exec -T postgres psql -U webhook_redrive -d webhook_redrive -At -v ON_ERROR_STOP=1 -f /benchmark-stats.sql) | ConvertFrom-Json
    Stop-Job $samplerJob
    $resourceSamples = @(Receive-Job $samplerJob -ErrorAction Stop | Select-Object captured_at,container)
    if ($samplerJob.State -eq 'Failed' -or $resourceSamples.Count -eq 0) { throw 'Resource evidence missing' }
    [ordered]@{ project=$project; revision=$revision; before=$before; after=$after; resources=$resourceSamples } |
        ConvertTo-Json -Depth 12 | Set-Content -LiteralPath (Join-Path $runDirectory 'database-and-resources.json')
    Write-Host "Evidence: $runDirectory"
} finally {
    if ($samplerJob) { Stop-Job $samplerJob; Remove-Job $samplerJob }
    Invoke-Docker @compose stop
    $env:BENCHMARK_OUTPUT_DIR = $previousOutput
}
