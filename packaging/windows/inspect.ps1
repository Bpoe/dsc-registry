[CmdletBinding()]
param(
    [Parameter(Mandatory)][string] $Version,
    [Parameter(Mandatory)][string] $PackageDirectory
)
$ErrorActionPreference = 'Stop'
$productVersion = & "$PSScriptRoot\msi\version.ps1" -Version $Version
$msi = (Resolve-Path -LiteralPath (Join-Path $PackageDirectory "dscregd-$Version-windows-amd64.msi")).Path
$installer = New-Object -ComObject WindowsInstaller.Installer
$database = $installer.OpenDatabase($msi, 0)
function Read-Row([string] $Query, [int] $Columns = 1) {
    $view = $database.OpenView($Query)
    try {
        [void]$view.Execute()
        $record = $view.Fetch()
        if (-not $record) { throw "Missing MSI row: $Query" }
        try {
            $values = @(for ($i = 1; $i -le $Columns; $i++) { $record.StringData($i) })
            if ($view.Fetch()) { throw "Unexpected extra MSI row: $Query" }
            return ,$values
        }
        finally { [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($record) }
    }
    finally {
        [void]$view.Close()
        [void][Runtime.InteropServices.Marshal]::FinalReleaseComObject($view)
    }
}
try {
    $value = Read-Row "SELECT ``Value`` FROM ``Property`` WHERE ``Property`` = 'ProductVersion'"
    if ($value[0] -cne $productVersion) { throw 'MSI product version mismatch.' }
    $cli = Read-Row "SELECT ``Component_``, ``FileName`` FROM ``File`` WHERE ``File`` = 'CliFile'" 2
    if ($cli[0] -cne 'CLI' -or $cli[1].Split('|')[-1] -cne 'dscreg.exe') {
        throw 'MSI must include the standalone CLI in its own component.'
    }
    $path = Read-Row "SELECT ``Name``, ``Value``, ``Component_`` FROM ``Environment`` WHERE ``Environment`` = 'CliPath'" 3
    if (-not $path[0].EndsWith('PATH') -or -not $path[0].Contains('*') -or
        $path[1] -cne '[~];[INSTALLFOLDER]' -or $path[2] -cne 'CLI') {
        throw 'MSI must append the CLI install directory to the machine PATH.'
    }
    $service = Read-Row 'SELECT `Name`, `ServiceType`, `StartType`, `StartName`, `Arguments` FROM `ServiceInstall`' 5
    if ($service[0] -cne 'dscregd' -or $service[1] -ne '16' -or $service[2] -ne '3' -or
        $service[3] -cne 'NT AUTHORITY\LocalService' -or $service[4] -cne '-config "[DATAFOLDER]config.json"') {
        throw 'MSI must register a manual-start LocalService SCM service with explicit configuration.'
    }
    $control = Read-Row 'SELECT `Name`, `Event`, `Wait` FROM `ServiceControl`' 3
    if ($control[0] -cne 'dscregd' -or $control[1] -ne '162' -or $control[2] -ne '1') {
        throw 'MSI must stop the service on install/uninstall, wait and remove on uninstall only.'
    }
    foreach ($folder in @('DATAFOLDER', 'REPORTSFOLDER')) {
        $permission = Read-Row "SELECT ``SDDLText`` FROM ``MsiLockPermissionsEx`` WHERE ``LockObject`` = '$folder'"
        if ($permission[0] -cne 'O:SYG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;LS)') {
            throw 'MSI data ACL must restrict access to SYSTEM, Administrators and LocalService.'
        }
    }
}
finally {
    [Runtime.InteropServices.Marshal]::FinalReleaseComObject($database) | Out-Null
    [Runtime.InteropServices.Marshal]::FinalReleaseComObject($installer) | Out-Null
}
Write-Output 'MSI version, CLI/PATH, daemon-only service registration and protected data ACL metadata passed.'
