[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $Version,
    [Parameter(Mandatory)][string] $Commit,
    [Parameter(Mandatory)][string] $OutputDirectory
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if (-not $IsWindows) { throw 'MSI builds require Windows and PowerShell 7.' }
$productVersion = & "$PSScriptRoot\msi\version.ps1" -Version $Version
if ($Commit -cnotmatch '\A[0-9a-f]{40}\z') { throw 'Commit must be the full source commit.' }
$output = [IO.Path]::GetFullPath($OutputDirectory)
$stage = Join-Path ([IO.Path]::GetTempPath()) ('dscregd-package-' + [guid]::NewGuid().ToString('N'))
$savedEnvironment = @{}
foreach ($name in @('CGO_ENABLED', 'GOOS', 'GOARCH')) {
    $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
Push-Location "$PSScriptRoot\..\.."
try {
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $package = Join-Path $stage "dscregd-$Version-windows-amd64"
    New-Item -ItemType Directory -Path "$package\bin", "$stage\msi", $output -Force | Out-Null
    $binary = Join-Path $package 'bin\dscregd.exe'
    go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=$Version -X main.commit=$Commit" -o $binary .\cmd\dscregd
    if ($LASTEXITCODE -ne 0) { throw 'Go build failed.' }
    if ((& $binary --version) -cne "dscregd $Version ($Commit)" -or $LASTEXITCODE -ne 0) {
        throw 'Binary version/commit mismatch.'
    }
    Copy-Item README.md, LICENSE -Destination $package
    Copy-Item docs -Destination $package -Recurse
    Copy-Item "$PSScriptRoot\config.example.json" -Destination $package
    Compress-Archive -Path $package -DestinationPath "$output\dscregd-$Version-windows-amd64.zip"
    dotnet build "$PSScriptRoot\msi\dscregd.wixproj" --configuration Release --nologo --no-incremental --verbosity diagnostic `
        "-p:ReleaseVersion=$Version" "-p:ProductVersion=$productVersion" "-p:BinaryPath=$binary" `
        "-p:BaseIntermediateOutputPath=$stage\obj\" "-p:MSBuildProjectExtensionsPath=$stage\obj\" `
        "-p:OutputPath=$stage\msi\"
    if ($LASTEXITCODE -ne 0) { throw 'WiX MSI build/validation failed.' }
    Copy-Item "$stage\msi\dscregd-$Version-windows-amd64.msi" -Destination $output
    & "$PSScriptRoot\inspect.ps1" -Version $Version -PackageDirectory $output
    $lines = foreach ($extension in @('zip', 'msi')) {
        $name = "dscregd-$Version-windows-amd64.$extension"
        $hash = (Get-FileHash -LiteralPath "$output\$name" -Algorithm SHA256).Hash.ToLowerInvariant()
        "$hash  $name"
    }
    Set-Content -LiteralPath "$output\SHA256SUMS" -Value $lines -Encoding ascii
}
finally {
    Pop-Location
    foreach ($name in $savedEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process')
    }
    if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
}
