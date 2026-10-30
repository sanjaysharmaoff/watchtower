# Prepares a local minikube so WatchTower can read real Kubernetes events.
#
#   pwsh deploy/k8s/wire-minikube.ps1
#   docker compose -f docker-compose.yml -f docker-compose.minikube.yml up -d
#
# Idempotent: safe to re-run whenever the service account token expires.

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $root

if (-not (minikube status --format "{{.Host}}" 2>$null)) {
    Write-Host "starting minikube..." -ForegroundColor Cyan
    minikube start --driver=docker --cpus=2 --memory=3000
}

Write-Host "applying RBAC and the deliberately broken workloads..." -ForegroundColor Cyan
kubectl apply -f deploy/k8s/watchtower-reader.yaml | Out-Null
kubectl apply -f deploy/k8s/failing-workload.yaml | Out-Null

$ca = Join-Path $env:USERPROFILE ".minikube\ca.crt"
if (-not (Test-Path $ca)) { throw "cluster CA not found at $ca" }
Copy-Item $ca "deploy/k8s/minikube-ca.crt" -Force

$token = (kubectl -n watchtower create token watchtower --duration=24h).Trim()
if (-not $token) { throw "could not mint a service account token" }

# Compose reads .env for ${WT_K8S_TOKEN}. Keep any other keys already in there.
$lines = @()
if (Test-Path ".env") {
    $lines = Get-Content ".env" | Where-Object { $_ -notmatch "^WT_K8S_TOKEN=" }
}
$lines += "WT_K8S_TOKEN=$token"
Set-Content -Path ".env" -Value $lines -Encoding ascii

Write-Host "ready. token valid 24h, CA copied to deploy/k8s/minikube-ca.crt" -ForegroundColor Green
Write-Host "next: docker compose -f docker-compose.yml -f docker-compose.minikube.yml up -d" -ForegroundColor Green
