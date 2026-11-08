# powershell -File path.ps1 add|remove "<dir>;<dir>": the user's Path, read and written raw so
# %VAR% entries stay unexpanded and nothing is cut at NSIS's 1024-character strings
$ErrorActionPreference = 'Stop'
$op, $list = $args
$dirs = $list -split ';' | Where-Object { $_ }
$key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
$path = @($key.GetValue('Path', '', 'DoNotExpandEnvironmentNames') -split ';' | Where-Object { $_ -and $_ -notin $dirs })
if ($op -eq 'add') { $path += $dirs }
if ($path) { $key.SetValue('Path', $path -join ';', 'ExpandString') } else { $key.DeleteValue('Path', $false) }
