[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $Version,
    [Parameter(Mandatory)][string] $Commit,
    [Parameter(Mandatory)][string] $PackageDirectory
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ($env:GITHUB_ACTIONS -cne 'true' -or -not $IsWindows) {
    throw 'Requires an elevated disposable Windows CI runner.'
}
$root = Join-Path $env:ProgramData 'dscregd'
$installed = Join-Path $env:ProgramFiles 'dscregd'
if ((Get-Service dscregd -ErrorAction SilentlyContinue) -or
    (Test-Path -LiteralPath $root) -or (Test-Path -LiteralPath $installed)) {
    throw 'Refusing to modify an existing installation.'
}
$msi = Join-Path $PackageDirectory "dscregd-$Version-windows-amd64.msi"
$archive = Join-Path $PackageDirectory "dscregd-$Version-windows-amd64.zip"
$log = Join-Path $env:RUNNER_TEMP 'dscregd-msi-install.log'
function Invoke-Msi([string] $Action) {
    $process = Start-Process msiexec.exe -ArgumentList "$Action `"$msi`" /qn /norestart /l*v `"$log`"" -Wait -PassThru
    if ($process.ExitCode -notin @(0, 3010)) { throw "MSI failed ($($process.ExitCode)); see $log" }
}
Invoke-Msi '/i'
try {
    if (Get-Service dscreg -ErrorAction SilentlyContinue) { throw 'CLI must not be installed as a service.' }
    $machinePath = [Environment]::GetEnvironmentVariable('PATH', 'Machine')
    if ($installed.TrimEnd('\') -notin @($machinePath.Split(';') | ForEach-Object { $_.TrimEnd('\') })) {
        throw 'MSI did not add the CLI directory to the machine PATH.'
    }
    $savedPath = $env:PATH
    try {
        $env:PATH = $machinePath
        if ((Get-Command dscreg.exe -CommandType Application).Source -ine "$installed\dscreg.exe") {
            throw 'CLI is not accessible through PATH.'
        }
    }
    finally { $env:PATH = $savedPath }
    $service = Get-CimInstance Win32_Service -Filter "Name='dscregd'"
    if ($service.StartName -ne 'NT AUTHORITY\LocalService' -or
        $service.StartMode -ne 'Manual' -or $service.State -ne 'Stopped') {
        throw 'Unexpected service identity, startup mode or state.'
    }
    foreach ($directory in @($root, "$root\reports")) {
        $acl = Get-Acl -LiteralPath $directory
        if (-not $acl.AreAccessRulesProtected) { throw 'Data ACL inheritance must be disabled.' }
        $allowed = @('S-1-5-18', 'S-1-5-32-544', 'S-1-5-19')
        foreach ($rule in $acl.Access) {
            $sid = $rule.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value
            if ($sid -notin $allowed) { throw "Unexpected data directory ACL: $sid" }
        }
    }
    python .\packaging\verify-installed.py --binary "$installed\dscregd.exe" --archive $archive `
        --root $root --config "$root\config.json" --version $Version --commit $Commit --service windows
    if ($LASTEXITCODE -ne 0) { throw 'Installed package smoke test failed.' }
    # Stop/remove must also work while the service is running.
    Start-Service dscregd
}
finally {
    Invoke-Msi '/x'
}
if ((Get-Service dscregd -ErrorAction SilentlyContinue) -or (Test-Path "$installed\dscregd.exe") -or
    (Test-Path "$installed\dscreg.exe")) {
    throw 'Uninstall left the service or executable behind.'
}
$machinePath = [Environment]::GetEnvironmentVariable('PATH', 'Machine')
if ($installed.TrimEnd('\') -in @($machinePath.Split(';') | ForEach-Object { $_.TrimEnd('\') })) {
    throw 'Uninstall left the CLI machine PATH entry behind.'
}
if (-not (Test-Path "$root\registry.db") -or -not (Test-Path "$root\config.json") -or
    -not (Test-Path "$root\reports")) { throw 'Uninstall removed operator data.' }
if (@(Get-ChildItem -LiteralPath "$root\reports" -File).Count -eq 0) { throw 'Uninstall removed stored reports.' }
Write-Output 'MSI service lifecycle, private data ACLs and uninstall preservation passed.'
