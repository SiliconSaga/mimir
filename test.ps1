# test.ps1
# Runs Kuttl tests using Docker on Windows against a local cluster (Rancher Desktop/k3d)

$ErrorActionPreference = "Stop"

# Create a temporary kubeconfig file
$TempKubeConfig = [System.IO.Path]::GetTempFileName()

Write-Host "Preparing kubeconfig for Docker..." -ForegroundColor Cyan

# Flatten the current kubeconfig and save to temp file
# We use --minify to only get the current context
kubectl config view --minify --flatten | Out-File -FilePath $TempKubeConfig -Encoding UTF8

# Read the file content
$Content = Get-Content -Path $TempKubeConfig -Raw

# Replace localhost/127.0.0.1 with host.docker.internal
$Content = $Content -replace '127\.0\.0\.1', 'host.docker.internal'
$Content = $Content -replace 'localhost', 'host.docker.internal'

# Remove certificate-authority-data (since we are changing the host, the cert won't match usually if it's IP based, 
# and often local clusters use self-signed certs that Docker container won't trust)
$Content = $Content -replace 'certificate-authority-data:.*', 'insecure-skip-tls-verify: true'

# Save back to temp file
$Content | Set-Content -Path $TempKubeConfig -Encoding UTF8

Write-Host "Running Kuttl via Docker..." -ForegroundColor Cyan

$RepoRoot = Get-Location
$TestDir = Join-Path $RepoRoot "tests\e2e"

# Smart argument handling
$DockerArgs = @()
foreach ($arg in $args) {
    if ($arg -notmatch "^-") {
        # Check if it's a known test suite name
        $PotentialTestPath = Join-Path $TestDir $arg
        if (Test-Path $PotentialTestPath) {
            Write-Host "  Auto-detecting test suite: $arg -> --test $arg" -ForegroundColor Yellow
            $DockerArgs += "--test"
            $DockerArgs += $arg
            continue
        }
        
        # Check if it's a path, convert slashes
        $arg = $arg -replace '\\', '/'
    }
    $DockerArgs += $arg
}

# Run Docker container
# We map the temp Kubeconfig to /kubeconfig
# We map the repo root to /workspace
# To avoid polluting the host with a 'kubectl' symlink, we work in /tmp inside the container
docker run --rm `
    -v "${TempKubeConfig}:/kubeconfig" `
    -v "${RepoRoot}:/workspace" `
    -e KUBECONFIG=/kubeconfig `
    --add-host host.docker.internal:host-gateway `
    --entrypoint /bin/sh `
    kudobuilder/kuttl:latest `
    -c "mkdir -p /tmp/work && cp /workspace/kuttl-test.yaml /tmp/work/ && ln -s /workspace/tests /tmp/work/tests && ln -s /workspace/features /tmp/work/features && ln -s /usr/bin/kubectl /tmp/work/kubectl && cd /tmp/work && kubectl-kuttl test --config kuttl-test.yaml $DockerArgs"

# Cleanup (optional, as temp path cleans up eventually, but polite to do so)
Remove-Item -Path $TempKubeConfig -ErrorAction SilentlyContinue
