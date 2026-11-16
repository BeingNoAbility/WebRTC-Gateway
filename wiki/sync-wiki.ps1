# PowerShell script to push local wiki documents to GitHub Wiki repository
# Usage: .\wiki\sync-wiki.ps1 [optional_wiki_git_url]

param(
    [string]$WikiRepoUrl = "https://github.com/BeingNoAbility/WebRTC-Gateway.wiki.git"
)

$ErrorActionPreference = "Stop"
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("webrtc-wiki-" + [System.Guid]::NewGuid().ToString("N"))

Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host " Syncing Wiki documentation to: $WikiRepoUrl" -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan

# Test if the remote wiki repository is created and accessible
Write-Host "Checking if GitHub Wiki repository exists..." -ForegroundColor Yellow
$prevEAP = $ErrorActionPreference
$ErrorActionPreference = "SilentlyContinue"
$lsRemote = & git ls-remote $WikiRepoUrl 2>$null
$exitCode = $LASTEXITCODE
$ErrorActionPreference = $prevEAP

if ($exitCode -ne 0) {
    Write-Host ""
    Write-Host "[!] GitHub Wiki repository not initialized yet!" -ForegroundColor Red
    Write-Host "    Reason: GitHub creates the .wiki.git repository only after you click" -ForegroundColor Yellow
    Write-Host "    'Create the first page' on the web interface." -ForegroundColor Yellow
    Write-Host ""
    Write-Host "    Please follow these quick 2 steps:" -ForegroundColor Green
    Write-Host "    1. Open: https://github.com/BeingNoAbility/WebRTC-Gateway/wiki in your browser" -ForegroundColor White
    Write-Host "    2. Click 'Create the first page', type anything, and click 'Save Page'." -ForegroundColor White
    Write-Host "    3. Run this script again!" -ForegroundColor White
    Write-Host ""
    exit 1
}

try {
    Write-Host "Cloning wiki repository to temporary directory..." -ForegroundColor Yellow
    git clone $WikiRepoUrl $TempDir
    
    Write-Host "Copying documentation pages..." -ForegroundColor Yellow
    Get-ChildItem -Path $ScriptDir -Filter "*.md" | ForEach-Object {
        Copy-Item -Path $_.FullName -Destination $TempDir -Force
        Write-Host "  -> Synced: $($_.Name)" -ForegroundColor Green
    }

    Push-Location $TempDir
    git config user.name "Wiki Bot"
    git config user.email "wiki-bot@users.noreply.github.com"
    
    git add -A
    $status = git status --porcelain
    if (-not $status) {
        Write-Host "Wiki is already up to date. Nothing to commit." -ForegroundColor Cyan
    } else {
        git commit -m "docs(wiki): update wiki documentation from wiki/ folder"
        git push origin master
        Write-Host "Successfully pushed wiki documents to GitHub Wiki!" -ForegroundColor Green
    }
} finally {
    Pop-Location -ErrorAction SilentlyContinue
    if (Test-Path $TempDir) {
        Remove-Item -Path $TempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
