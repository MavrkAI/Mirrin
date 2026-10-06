# Mirrin installer for Windows. In PowerShell:
#   irm https://raw.githubusercontent.com/MavrkAI/Mirrin/main/install.ps1 | iex
# Downloads mirrin.exe for this PC from a GitHub release, checks it against the
# release's SHA256SUMS before installing anything, and puts it on your PATH.
# The Windows installer (Mirrin-<version>-windows-setup.exe) on the releases
# page does the same with a Start menu entry and sign-in startup.
#
# Optional settings (environment variables):
#   MIRRIN_BUILD=nowhatsapp   MIT without WhatsApp (default: GPL-3.0 with WhatsApp)
#   MIRRIN_VERSION=v0.3.0     a specific release instead of the latest
#   MIRRIN_BIN_DIR=DIR        where mirrin.exe goes (default %LOCALAPPDATA%\Programs\Mirrin)
#   MIRRIN_NO_MODIFY_PATH=1   leave PATH alone
#   MIRRIN_REPO=owner/name    install from a fork
#   MIRRIN_DOWNLOAD_URL=URL   where the releases live (a mirror)
# The same settings named ANTBOT_..., from before Mirrin was renamed, still
# work; the MIRRIN_ name wins when both are set.

# Everything runs inside this block, so a download cut off halfway runs nothing,
# and a failure never closes the window it was pasted into.
& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # the progress bar slows Windows PowerShell downloads badly
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    # A setting by its name after MIRRIN_, or under its name from before the rename.
    function Get-Setting([string]$name) {
        $v = [Environment]::GetEnvironmentVariable("MIRRIN_$name")
        if (-not $v) { $v = [Environment]::GetEnvironmentVariable("ANTBOT_$name") } # rename:keep
        return $v
    }

    $repo = if (Get-Setting 'REPO') { Get-Setting 'REPO' } else { 'MavrkAI/Mirrin' }
    $releases = if (Get-Setting 'DOWNLOAD_URL') { (Get-Setting 'DOWNLOAD_URL').TrimEnd('/') } else { "https://github.com/$repo/releases" }
    # @main until the first Mirrin release is tagged: @latest is still v0.2.0,
    # from before the module had this name.
    $fromSource = "go install github.com/$repo/cmd/mirrin@main"
    $issues = "https://github.com/$repo/issues"
    $server = try { ([Uri]$releases).Authority } catch { $null }
    if (-not $server) { $server = $releases }
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("mirrin-" + [Guid]::NewGuid().ToString('N'))

    # What went wrong with a download of $what that wasn't a missing file: $status is the HTTP status, or 0 when nothing answered.
    function Get-Trouble([string]$what, [int]$status) {
        if ($status -gt 0) { return "$server answered HTTP $status for $what, so it may be busy or limiting downloads. Try again in a minute." }
        return "couldn't reach $server for $what. Check your internet connection and try again."
    }

    # The HTTP status in a failed request's error, or 0 when nothing answered. Errors
    # from .NET calls arrive wrapped, with the response on an inner exception.
    function Get-Status($err) {
        $e = $err.Exception
        while ($e -and -not $e.Response -and $e.InnerException) { $e = $e.InnerException }
        if ($e -and $e.Response) { return [int]$e.Response.StatusCode }
        return 0
    }

    # Whether this is a window someone typed into, which exit would close: PowerShell
    # started without a command, or kept open with -NoExit ("Open PowerShell window here").
    function Test-OpenWindow {
        $a = @([Environment]::GetCommandLineArgs() | Select-Object -Skip 1)
        if ($a -match '^[-/]noe(xit)?$') { return $true }
        return -not ($a -match '^[-/](c|command|f|file|e|ec|encodedcommand|noni|noninteractive)$')
    }

    # The newest release tag, from where releases/latest redirects (no API token, no rate limit).
    # A renamed repository (AntBot's is now Mirrin's) first sends releases/latest to the new
    # name's, on the same server; that is followed, a few times at most.
    function Get-LatestTag {
        $url = [Uri]"$releases/latest"
        for ($moves = 0; $moves -le 3; $moves++) {
            $req = [Net.WebRequest]::Create($url)
            $req.Method = 'HEAD'
            $req.AllowAutoRedirect = $false
            $resp = $req.GetResponse()
            try { $location = $resp.Headers['Location'] } finally { $resp.Close() }
            if (-not $location) { return $null }
            $next = [Uri]::new($url, $location)
            if ($next.Host -eq $url.Host -and $next.Scheme -eq $url.Scheme -and $next.AbsolutePath.TrimEnd('/') -like '*/releases/latest' -and $next.AbsolutePath -ne $url.AbsolutePath) {
                $url = $next
                continue
            }
            $tag = $location.TrimEnd('/').Split('/')[-1]
            if (@('', 'latest', 'releases') -contains $tag) { return $null }
            return $tag
        }
        return $null
    }

    # Downloads a URL to a file, or stops with $missing when the release has no such file.
    function Get-File([string]$url, [string]$out, [string]$missing) {
        try {
            Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $out
        } catch {
            $status = Get-Status $_
            if ($status -eq 404) { throw $missing }
            throw (Get-Trouble ($url -split '/')[-1] $status)
        }
    }

    try {
        $suffix = ''
        $buildLabel = ''
        switch (Get-Setting 'BUILD') {
            { [string]::IsNullOrEmpty($_) -or $_ -eq 'default' } { }
            'nowhatsapp' { $suffix = '-nowhatsapp'; $buildLabel = 'MIT (without WhatsApp) ' }
            default { throw 'Unknown MIRRIN_BUILD. Choose default (GPL-3.0, with WhatsApp) or nowhatsapp (MIT).' }
        }
        $arch = $null
        try { $arch = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() } catch { }
        if (-not $arch) { $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE } }
        switch ($arch.ToUpper()) {
            { $_ -in 'X64', 'AMD64' } { $goarch = 'amd64' }
            'ARM64' { $goarch = 'arm64' }
            default { throw "there's no Mirrin build for Windows on $arch yet. You can build it from source: $fromSource" }
        }
        $asset = "mirrin-windows-${goarch}${suffix}.exe"

        $version = Get-Setting 'VERSION'
        if (-not $version) {
            $status = 404
            try { $version = Get-LatestTag } catch { $status = Get-Status $_ }
            if (-not $version) {
                if ($status -eq 404) { throw "couldn't find a published Mirrin release at $releases. You can build from source: $fromSource" }
                throw (Get-Trouble 'the latest release' $status)
            }
        }
        $base = "$releases/download/$version"
        Write-Host "installing Mirrin $version for windows/$goarch"

        New-Item -ItemType Directory -Force -Path $tmp | Out-Null
        $sums = Join-Path $tmp 'SHA256SUMS'
        Get-File "$base/SHA256SUMS" $sums "Mirrin $version has no checksum list (SHA256SUMS), so its downloads can't be checked. Nothing was installed. Pick a newer release from $releases, or build from source: $fromSource"
        $download = Join-Path $tmp $asset
        Get-File "$base/$asset" $download "Mirrin $version has no ${buildLabel}build for windows/$goarch ($asset). Choose a release that includes this build. See $releases for what's available, or build from source: $fromSource"

        $want = $null
        foreach ($line in Get-Content $sums) {
            $f = $line -split '\s+', 2
            if ($f.Count -eq 2 -and $f[1].TrimStart('*') -eq $asset) { $want = $f[0].ToLower(); break }
        }
        if (-not $want) { throw "the $version checksum list doesn't include $asset, so it can't be checked. Nothing was installed." }
        # .NET's own SHA-256, not Get-FileHash: Windows PowerShell started from
        # PowerShell 7 inherits 7's module path and can't load Get-FileHash.
        $stream = [System.IO.File]::OpenRead($download)
        try { $hash = [System.Security.Cryptography.SHA256]::Create().ComputeHash($stream) } finally { $stream.Dispose() }
        $got = -join ($hash | ForEach-Object { $_.ToString('x2') })
        if ($got -ne $want) {
            throw "$asset doesn't match its published checksum, so it may be damaged or tampered with. Nothing was installed. Try again; if it keeps happening, please report it: $issues"
        }
        Write-Host 'checksum matches'

        $dest = if (Get-Setting 'BIN_DIR') { Get-Setting 'BIN_DIR' } else { Join-Path $env:LOCALAPPDATA 'Programs\Mirrin' }
        New-Item -ItemType Directory -Force -Path $dest | Out-Null
        $exe = Join-Path $dest 'mirrin.exe'
        $new = Join-Path $dest 'mirrin.new.exe'
        $old = Join-Path $dest 'mirrin.old.exe'
        # Try the new copy before replacing anything.
        Move-Item -Force $download $new
        $ok = $false
        try { & $new version *> $null; $ok = ($LASTEXITCODE -eq 0) } catch { }
        if (-not $ok) {
            Remove-Item -Force $new -ErrorAction SilentlyContinue
            throw "the downloaded mirrin won't start on this PC (windows/$goarch). Nothing was installed. Please report it: $issues"
        }
        # A running mirrin.exe can be renamed but not overwritten.
        Remove-Item -Force $old -ErrorAction SilentlyContinue
        if (Test-Path $exe) { Move-Item -Force $exe $old }
        Move-Item -Force $new $exe
        Remove-Item -Force $old -ErrorAction SilentlyContinue

        Write-Host ''
        Write-Host "Mirrin $version is installed: $exe"
        if (Test-Path $old) {
            Write-Host 'The previous Mirrin is still running; quit it from the tray and start it again to use this version.'
        }
        $onPath = @($env:Path -split ';' | Where-Object { $_.TrimEnd('\') -eq $dest.TrimEnd('\') }).Count -gt 0
        if (-not $onPath) {
            if (Get-Setting 'NO_MODIFY_PATH') {
                Write-Host "$dest isn't on your PATH. Add it in Settings > System > About > Advanced system settings > Environment Variables."
            } else {
                # Read and write the raw value so entries like %USERPROFILE%\bin stay as they are.
                $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
                $raw = $key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
                $entries = @($raw -split ';' | Where-Object { $_ })
                if ($entries -notcontains $dest) {
                    $key.SetValue('Path', (($entries + $dest) -join ';'), [Microsoft.Win32.RegistryValueKind]::ExpandString)
                }
                $key.Close()
                # Setting any user variable tells open programs that the environment changed.
                [Environment]::SetEnvironmentVariable('MIRRIN_INSTALLER', '1', 'User')
                [Environment]::SetEnvironmentVariable('MIRRIN_INSTALLER', $null, 'User')
                $env:Path = "$env:Path;$dest"
                Write-Host "added $dest to your PATH; new terminals pick it up."
            }
        }
        # AntBot, from before the rename, stays where it is: its sign-in entry may still start it.
        $oldExe = Get-Command antbot.exe -ErrorAction SilentlyContinue | Select-Object -First 1 # rename:keep
        $oldStartup = Join-Path ([Environment]::GetFolderPath('Startup')) 'AntBot.cmd' # rename:keep
        if ($oldExe -or (Test-Path $oldStartup)) {
            $where = if ($oldExe) { " ($($oldExe.Source))" } else { '' }
            Write-Host "note: the old AntBot is still installed$where. Mirrin doesn't use it; once ``mirrin service install`` has replaced its sign-in entry, you can delete it." # rename:keep
        }
        Write-Host ''
        Write-Host 'Next:'
        Write-Host '  $env:ANTHROPIC_API_KEY = "..."   # or OPENAI_API_KEY / GEMINI_API_KEY, or just have Ollama running'
        Write-Host '  mirrin chat                      # works with zero config; `mirrin init` if you''d rather be asked'
        Write-Host '  mirrin service install           # the tray icon, now and whenever you sign in (or just: mirrin tray)'
    } catch {
        Write-Host "mirrin install: $($_.Exception.Message)" -ForegroundColor Red
        # Scripts can tell it failed: run from a file or a command line (CI, tests,
        # powershell -c "irm ... | iex") it exits 1. Pasted into an open window, it
        # leaves the window open and sets $LASTEXITCODE.
        $global:LASTEXITCODE = 1
        if ($PSCommandPath -or -not (Test-OpenWindow)) { exit 1 }
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }
}
