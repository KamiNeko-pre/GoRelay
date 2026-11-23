param([Parameter(ValueFromRemainingArguments = $true)][string[]]$DockerArguments)
$ErrorActionPreference = 'Stop'
& "$PSScriptRoot/assert-local-storage.ps1" -ApplyEnvironment | Out-Null
& docker @DockerArguments
exit $LASTEXITCODE
