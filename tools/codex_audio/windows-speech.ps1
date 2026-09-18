$ErrorActionPreference = "Stop"
# 固定本机语音引擎；文本仅经 stdin，音频仅经 stdout，不使用临时文件。
[Console]::InputEncoding = New-Object Text.UTF8Encoding($false)
$Request = [Console]::In.ReadToEnd() | ConvertFrom-Json
if ($Request.text -isnot [string] -or $Request.text.Length -lt 1 -or $Request.text.Length -gt 400) { exit 2 }
Add-Type -AssemblyName System.Speech
$Voice = New-Object System.Speech.Synthesis.SpeechSynthesizer
$Audio = New-Object IO.MemoryStream
try {
  $Chinese = $Voice.GetInstalledVoices() | Where-Object { $_.Enabled -and $_.VoiceInfo.Culture.Name -like "zh-*" } | Select-Object -First 1
  if ($Chinese) { $Voice.SelectVoice($Chinese.VoiceInfo.Name) }
  $Voice.SetOutputToWaveStream($Audio)
  $Voice.Speak($Request.text)
  $Voice.SetOutputToNull()
  $Bytes = $Audio.ToArray()
  [Console]::OpenStandardOutput().Write($Bytes, 0, $Bytes.Length)
} finally { $Voice.Dispose(); $Audio.Dispose() }
