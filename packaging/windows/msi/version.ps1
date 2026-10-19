[CmdletBinding()]
param([Parameter(Mandatory)][string] $Version)
$ErrorActionPreference = 'Stop'

if ($Version -cnotmatch '\Av(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\z') {
    throw 'Expected a stable vMAJOR.MINOR.PATCH tag.'
}
$major = [System.Numerics.BigInteger]::Parse($Matches[1])
$minor = [System.Numerics.BigInteger]::Parse($Matches[2])
$patch = [System.Numerics.BigInteger]::Parse($Matches[3])
if ($major -gt 255 -or $minor -gt 255 -or $patch -gt 65535) {
    throw 'MSI releases require major/minor <= 255 and patch <= 65535.'
}
"$major.$minor.$patch"
