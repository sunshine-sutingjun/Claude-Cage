#requires -Version 7.6
param([Parameter(Mandatory)][string]$RuntimeArchive)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$source = Split-Path $PSScriptRoot -Parent
$errors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path $source 'install-ccs.ps1'), [ref]$null, [ref]$errors)
if ($errors.Count) { throw $errors[0] }
foreach ($statement in $ast.EndBlock.Statements) {
    if (($statement -is [Management.Automation.Language.FunctionDefinitionAst] -and $statement.Name -in @('Assert-NoReparse','Assert-CCSTree')) -or
        ($statement -is [Management.Automation.Language.AssignmentStatementAst] -and $statement.Left.Extent.Text -in @('$archiveSha','$manifestSha'))) {
        . ([scriptblock]::Create($statement.Extent.Text))
    }
}
$code = Get-Content -LiteralPath (Join-Path $source 'CCSRuntime.cs') -Raw
Add-Type -CompilerOptions '/nullable:enable' -TypeDefinition ($code + @'
public static class RuntimeCheckFixture {
    public static void Verify(string manifest, string main, string sha) => CCSRuntime.Verify(manifest, main, sha);
}
'@)
if ((Get-FileHash -LiteralPath $RuntimeArchive).Hash -ne $archiveSha) { throw 'Incorrect release archive.' }
$fixture = Join-Path (Get-Location) ('cage-ccs-test-'+[Guid]::NewGuid().ToString('N'))
$root = [IO.Directory]::CreateDirectory($fixture).FullName
try {
    & tar -xf $RuntimeArchive -C $root
    if ($LASTEXITCODE -ne 0) { throw 'Failed to extract release archive.' }
    $manifest = Join-Path $root 'runtime-files.json'
    $main = Join-Path $root 'node_modules/@kaitranntt/ccs/dist/ccs.js'
    $mainSha = (Get-FileHash -LiteralPath $main).Hash.ToLowerInvariant()
    $checks = @({ Assert-CCSTree $root }, { [Cage.GuestClipboard.RuntimeCheckFixture]::Verify($manifest,$main,$mainSha) })
    foreach ($check in $checks) { & $check }
    function Assert-Rejected {
        foreach ($check in $checks) {
            $rejected = $false
            try { & $check } catch { $rejected = $true }
            if (-not $rejected) { throw 'Changed runtime accepted.' }
        }
    }
    $extra = Join-Path $root 'unlisted-fixture.txt'
    [IO.File]::WriteAllText($extra,'fixture')
    Assert-Rejected
    [IO.File]::Delete($extra)
    $package = Join-Path $root 'package.json'
    $original = [IO.File]::ReadAllBytes($package)
    [IO.File]::WriteAllText($package,'{}')
    Assert-Rejected
    [IO.File]::Delete($package)
    Assert-Rejected
    [IO.File]::WriteAllBytes($package,$original)
    [IO.File]::WriteAllText($manifest,'{}')
    Assert-Rejected
    Write-Output 'Both installer and bridge verified the release and rejected extra, changed or missing files and a replaced manifest.'
} finally { [IO.Directory]::Delete($root,$true) }
