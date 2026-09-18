#requires -Version 7.0
param(
  [ValidateSet("Install", "Start", "Stop", "Rollback", "Status", "EnableStartup", "DisableStartup")]
  [string]$Mode = "Install",
  [string]$GatewayOrigin = "",
  [int]$ControlPort = 19846,
  [int]$SidebandPort = 19847,
  [switch]$EnableLive
)
$ErrorActionPreference = "Stop"
if ($Mode -eq "Install") {
  $GatewayOrigin = $GatewayOrigin.Trim()
  if (-not $GatewayOrigin) { throw "安装时必须显式指定可信的 -GatewayOrigin" }
}
# 独立用户运行目录；不覆盖安装包，不重启桌面，不把凭据放命令行。
$Runtime = Join-Path $env:LOCALAPPDATA "ConcordRouteAudio"
$Node = (Get-Command node.exe).Source
$ScriptPath = Join-Path $env:APPDATA "CodexElves\user_scripts\concord-audio.js"
$Config = Join-Path $env:USERPROFILE ".codex\config.toml"
$StatePath = Join-Path $Runtime "runtime.json"
$StartupPath = Join-Path ([Environment]::GetFolderPath("Startup")) "ConcordRoute Audio.lnk"
$StartupStatePath = Join-Path $Runtime "startup.json"
# 同一用户的安装/启动/停止串行执行，避免登录启动与手动修复争用状态文件。
$Sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$Mutex = [Threading.Mutex]::new($false, "Local\ConcordRouteAudio-$Sid")
$Locked = $false
try {
try { $Locked = $Mutex.WaitOne(10000) } catch [Threading.AbandonedMutexException] { $Locked = $true }
if (-not $Locked) { throw "音频维护已在运行" }
function Read-State {
  if (Test-Path -LiteralPath $StatePath) {
    return Get-Content -LiteralPath $StatePath -Raw | ConvertFrom-Json
  }
  return $null
}
function Remove-StaleState {
  $State = Read-State
  if ($null -eq $State) { return }
  try {
    $Result = Invoke-RestMethod -Uri "$($State.controlUrl)/audio/status" `
      -Headers @{Authorization="Bearer $($State.bootstrap)"} -TimeoutSec 3
    if (-not $Result.ready -or ($Result.pid -and $Result.pid -ne $State.pid)) { throw "音频宿主状态不匹配" }
  } catch {
    # 不依据旧 PID 杀进程；仅在 PID 已不存在时移除确切的旧状态文件。
    if (Get-Process -Id $State.pid -ErrorAction SilentlyContinue) { throw "现有音频进程状态待确认；保留进程及文件" }
    Remove-Item -LiteralPath $StatePath -Force
  }
}
function Disable-Startup {
  if (-not (Test-Path -LiteralPath $StartupStatePath)) { return }
  $Saved = Get-Content -LiteralPath $StartupStatePath -Raw | ConvertFrom-Json
  if ($Saved.path -ne $StartupPath) { throw "启动项路径不匹配" }
  if (Test-Path -LiteralPath $StartupPath) {
    if ((Get-FileHash -LiteralPath $StartupPath -Algorithm SHA256).Hash -ne $Saved.hash) { throw "启动项已被后续编辑，保留原件" }
    Remove-Item -LiteralPath $StartupPath -Force
  }
  Remove-Item -LiteralPath $StartupStatePath -Force
}
if ($Mode -eq "EnableStartup") {
  $Installed = Join-Path $Runtime "install.ps1"
  if (-not (Test-Path -LiteralPath $Installed)) { throw "请先安装音频宿主" }
  if (Test-Path -LiteralPath $StartupPath) {
    if (-not (Test-Path -LiteralPath $StartupStatePath)) { throw "保留已有同名启动项" }
    $Saved = Get-Content -LiteralPath $StartupStatePath -Raw | ConvertFrom-Json
    if ($Saved.path -ne $StartupPath -or (Get-FileHash -LiteralPath $StartupPath -Algorithm SHA256).Hash -ne $Saved.hash) { throw "保留已修改启动项" }
    Write-Output "AUDIO_STARTUP_ALREADY_ENABLED"; exit 0
  }
  $Shortcut = (New-Object -ComObject WScript.Shell).CreateShortcut($StartupPath)
  $Shortcut.TargetPath = (Get-Command pwsh.exe).Source
  $Shortcut.Arguments = '-NoLogo -NoProfile -WindowStyle Hidden -File "' + $Installed + '" -Mode Start'
  $Shortcut.WorkingDirectory = $Runtime
  $Shortcut.WindowStyle = 7
  $Shortcut.Description = "ConcordRoute 本机音频宿主；仅监听 localhost，不自动录音或访问上游"
  $Shortcut.Save()
  @{path=$StartupPath;hash=(Get-FileHash -LiteralPath $StartupPath -Algorithm SHA256).Hash} |
    ConvertTo-Json | Set-Content -LiteralPath $StartupStatePath -Encoding utf8
  Write-Output "AUDIO_STARTUP_ENABLED"; exit 0
}
if ($Mode -eq "DisableStartup") { Disable-Startup; Write-Output "AUDIO_STARTUP_DISABLED"; exit 0 }
Remove-StaleState
function Stop-Host {
  $State = Read-State
  if ($null -ne $State) {
    try {
      $null = Invoke-RestMethod -Uri "$($State.controlUrl)/audio/stop" -Method Post `
        -Headers @{Authorization="Bearer $($State.bootstrap)"} -TimeoutSec 5
    } catch { throw "音频宿主停止尚未确认；保留文件以便检查" }
    for ($i=0; $i -lt 30 -and (Test-Path -LiteralPath $StatePath); $i++) { Start-Sleep -Milliseconds 100 }
    if (Test-Path -LiteralPath $StatePath) { throw "音频宿主仍在运行" }
  }
}
if ($Mode -eq "Status") {
  $State = Read-State
  if ($null -eq $State) { Write-Output "AUDIO_HOST_STOPPED"; exit 0 }
  $Result = Invoke-RestMethod -Uri "$($State.controlUrl)/audio/status" `
    -Headers @{Authorization="Bearer $($State.bootstrap)"} -TimeoutSec 5
  $Result | ConvertTo-Json -Depth 4
  exit 0
}
if ($Mode -eq "Stop" -or $Mode -eq "Rollback") {
  Stop-Host
  if ($Mode -eq "Rollback") {
    Disable-Startup
    if (Test-Path -LiteralPath (Join-Path $Runtime "config-change.json")) {
      & python (Join-Path $PSScriptRoot "config-patch.py") restore $Config $Runtime
      if ($LASTEXITCODE -ne 0) { throw "音频配置回滚待处理" }
    }
    if (Test-Path -LiteralPath $ScriptPath) {
      # 仅移除本次确切文件；不递归移动用户脚本目录。
      $Expected = [IO.Path]::GetFullPath((Join-Path $env:APPDATA "CodexElves\user_scripts\concord-audio.js"))
      if ([IO.Path]::GetFullPath($ScriptPath) -ne $Expected) { throw "回滚路径不符" }
      Move-Item -LiteralPath $ScriptPath -Destination (Join-Path $Runtime "audio-user-script.disabled.js") -Force
    }
    # 卸载撤销安装能力；普通 Stop/Start 保留能力，以免已加载脚本失效。
    $BootstrapPath = Join-Path $Runtime "bootstrap.secret"
    if (Test-Path -LiteralPath $BootstrapPath) { Remove-Item -LiteralPath $BootstrapPath -Force }
    Write-Output "AUDIO_ROLLBACK_COMPLETE_RELOAD_DESKTOP"
  } else { Write-Output "AUDIO_HOST_STOPPED" }
  exit 0
}
if ($Mode -eq "Install") {
  if (Read-State) { throw "请先停止现有音频宿主再更新" }
  & $Node -e "if(Number(process.versions.node.split('.')[0])<24)process.exit(1);require(process.argv[1]).verifyDesktopArchive(process.argv[2]);console.log('DESKTOP_CONTRACT_VERIFIED')" `
    (Join-Path $PSScriptRoot "verify-desktop.cjs") `
    "C:\Program Files\WindowsApps\OpenAI.Codex_26.903.8094.0_x64__2p2nqsd0c76g0\app\resources\app.asar"
  if ($LASTEXITCODE -ne 0) { throw "桌面构建或 Node 版本不匹配" }
  # 先收紧目录，再存放任何备份或引导能力。
  & $Node -e "require(process.argv[1]).privateDirectory(process.argv[2])" (Join-Path $PSScriptRoot "run-host.cjs") $Runtime
  if ($LASTEXITCODE -ne 0) { throw "私有运行目录初始化失败" }
  foreach ($File in Get-ChildItem -LiteralPath $PSScriptRoot -Filter "*.cjs") {
    if ($File.Name -notlike "*.test.cjs") { Copy-Item -LiteralPath $File.FullName -Destination (Join-Path $Runtime $File.Name) -Force }
  }
  Copy-Item -LiteralPath (Join-Path $PSScriptRoot "config-patch.py") -Destination $Runtime -Force
  Copy-Item -LiteralPath (Join-Path $PSScriptRoot "windows-speech.ps1") -Destination $Runtime -Force
  if ([IO.Path]::GetFullPath($PSScriptRoot) -ne [IO.Path]::GetFullPath($Runtime)) {
    Copy-Item -LiteralPath $PSCommandPath -Destination (Join-Path $Runtime "install.ps1") -Force
  }
  if (Test-Path -LiteralPath $ScriptPath) { throw "同名用户脚本已存在，保留原脚本" }
  @{
    gatewayOrigin=$GatewayOrigin; controlPort=$ControlPort; sidebandPort=$SidebandPort; userScriptPath=$ScriptPath; liveEnabled=[bool]$EnableLive
  } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $Runtime "settings.json") -Encoding utf8
}
if (Read-State) { Write-Output "AUDIO_HOST_ALREADY_RUNNING"; exit 0 }
$Arguments = '--use-env-proxy "' + (Join-Path $Runtime "run-host.cjs") + '" "' + $Runtime + '"'
# 沿用当前用户显式配置的网络代理；本地能力绝不经代理转发。
$OldNoProxy = $env:NO_PROXY
$env:NO_PROXY = (@($OldNoProxy, "localhost", "127.0.0.1", "::1") | Where-Object { $_ }) -join ","
try {
$Process = Start-Process -FilePath $Node -ArgumentList $Arguments -WindowStyle Hidden -PassThru `
  -RedirectStandardOutput (Join-Path $Runtime "host.stdout.log") -RedirectStandardError (Join-Path $Runtime "host.stderr.log")
} finally { $env:NO_PROXY = $OldNoProxy }
for ($i=0; $i -lt 50 -and -not (Test-Path -LiteralPath $StatePath); $i++) {
  if ($Process.HasExited) { throw "音频宿主启动失败，原桌面保持运行" }
  Start-Sleep -Milliseconds 100
}
if (-not (Test-Path -LiteralPath $StatePath)) { throw "音频宿主启动待确认" }
if ($Mode -eq "Install" -and $EnableLive) {
  # helper 已独占绑定端口后才写 app-server 选路；失败则停止本次宿主。
  & python (Join-Path $Runtime "config-patch.py") apply $Config $Runtime "ws://127.0.0.1:$SidebandPort/v1"
  if ($LASTEXITCODE -ne 0) { Stop-Host; throw "音频配置未变更" }
}
Write-Output "AUDIO_HOST_RUNNING_SCRIPT_INSTALLED_RELOAD_REQUIRED"
} finally {
  if ($Locked) { $Mutex.ReleaseMutex() }
  $Mutex.Dispose()
}
