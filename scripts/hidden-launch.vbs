' Hidden launcher for scheduled tasks under Interactive logon: wscript is a
' GUI-subsystem host, so no console ever flashes. Usage:
'   wscript.exe hidden-launch.vbs <program> <args...>
Dim args, i, cmd
Set args = WScript.Arguments
If args.Count < 1 Then WScript.Quit 1
cmd = """" & args(0) & """"
For i = 1 To args.Count - 1
  cmd = cmd & " """ & args(i) & """"
Next
CreateObject("WScript.Shell").Run cmd, 0, False
