$ErrorActionPreference = 'Stop'
foreach ($pair in @(@('v0.0.0', '0.0.0'), @('v1.2.3', '1.2.3'), @('v255.255.65535', '255.255.65535'))) {
    if ((& "$PSScriptRoot\version.ps1" -Version $pair[0]) -cne $pair[1]) { throw 'Incorrect MSI version mapping.' }
}
foreach ($version in @('1.2.3', 'v01.2.3', 'v1.2.3-rc1', 'v256.0.0', 'v0.256.0', 'v0.0.65536', "v1.2.3`n")) {
    $rejected = $false
    try { & "$PSScriptRoot\version.ps1" -Version $version | Out-Null }
    catch { $rejected = $true }
    if (-not $rejected) { throw "Accepted invalid release tag: $version" }
}
