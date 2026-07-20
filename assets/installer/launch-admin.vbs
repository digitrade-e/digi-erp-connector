Set fso = CreateObject("Scripting.FileSystemObject")
appDir = fso.GetParentFolderName(WScript.ScriptFullName)
exePath = appDir & "\digi-erp-connector.exe"

Set shell = CreateObject("Shell.Application")
shell.ShellExecute exePath, "", appDir, "runas", 1

