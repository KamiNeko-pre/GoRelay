param(
    [string]$DockerSettingsPath = "$env:APPDATA/Docker/settings-store.json",
    [string]$ExpectedDockerRoot = 'E:/DockerData',
    [double]$MinimumCFreeGiB = 20,
    [double]$MinimumDFreeGiB = 20,
    [double]$MinimumEFreeGiB = 30,
    [switch]$ApplyEnvironment
)

$ErrorActionPreference = 'Stop'
$settings = Get-Content -LiteralPath $DockerSettingsPath -Raw | ConvertFrom-Json
if ([string]::IsNullOrWhiteSpace($settings.CustomWslDistroDir)) {
    throw 'Docker WSL storage uses its default C-drive location. Startup refused.'
}
$expectedRoot = [IO.Path]::GetFullPath($ExpectedDockerRoot).TrimEnd('\', '/')
$actualRoot = [IO.Path]::GetFullPath($settings.CustomWslDistroDir).TrimEnd('\', '/')
if ($actualRoot -ne $expectedRoot -or [IO.Path]::GetPathRoot($actualRoot) -eq 'C:\') {
    throw "Unexpected Docker WSL storage: $actualRoot. Expected $expectedRoot."
}
$disk = Join-Path $actualRoot 'disk/docker_data.vhdx'
if (-not (Test-Path -LiteralPath $disk -PathType Leaf)) {
    throw "Existing Docker data disk missing: $disk. Startup refused."
}
foreach ($relative in @('disk/docker_data.vhdx', 'data/ext4.vhdx')) {
    $oldDisk = Join-Path "$env:LOCALAPPDATA/Docker/wsl" $relative
    if (Test-Path -LiteralPath $oldDisk -PathType Leaf) {
        throw "Docker data disk found on C drive: $oldDisk. Startup refused."
    }
}
$minimum = @{ C = $MinimumCFreeGiB; D = $MinimumDFreeGiB; E = $MinimumEFreeGiB }
$free = @{}
foreach ($drive in @('C', 'D', 'E')) {
    $free[$drive] = (Get-PSDrive -Name $drive).Free / 1GB
    if ($free[$drive] -lt $minimum[$drive]) {
        throw "$drive drive has $([math]::Round($free[$drive], 2)) GiB free; minimum is $($minimum[$drive]) GiB."
    }
}
if ($ApplyEnvironment) {
    $env:GOCACHE = 'D:/lua_spider/output/c-drive-recovery-20261010/go-build'
    $env:GOTMPDIR = 'D:/lua_spider/output/go-build-tmp'
    $env:GOPATH = 'D:/lua_spider/output/c-drive-recovery-20261010/go'
    $env:GOMODCACHE = "$env:GOPATH/pkg/mod"
    $env:GORELAY_CODEX_HOME = 'D:/GoRelayRuntime/codex-inference'
    $env:TEMP = 'D:/GoRelayRuntime/tmp'
    $env:TMP = $env:TEMP
    foreach ($directory in @($env:GOCACHE, $env:GOTMPDIR, $env:TEMP)) {
        New-Item -ItemType Directory -Path $directory -Force | Out-Null
    }
}
[pscustomobject]@{
    DockerDisk = $disk
    CFreeGiB = [math]::Round($free.C, 2)
    DFreeGiB = [math]::Round($free.D, 2)
    EFreeGiB = [math]::Round($free.E, 2)
}
