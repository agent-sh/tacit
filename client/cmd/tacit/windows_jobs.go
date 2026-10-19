package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/platform"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func encodedPowerShell(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, v := range u {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func decodePowerShell(encoded string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(b)%2 != 0 {
		return "", fmt.Errorf("invalid encoded PowerShell command")
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u)), nil
}

func hookCommand(exe, sub, goos string, extra ...string) string {
	var args []string
	if cfg := os.Getenv("TACIT_SETTINGS"); cfg != "" {
		args = append(args, "--config", cfg)
	}
	args = append(args, "hook", sub)
	args = append(args, extra...)
	if goos == "windows" {
		script := "& " + psQuote(exe)
		for _, a := range args {
			script += " " + psQuote(a)
		}
		return "powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand " + encodedPowerShell(script+"; exit $LASTEXITCODE")
	}
	cmd := shellQuote(exe)
	for _, a := range args {
		cmd += " " + shellQuote(a)
	}
	return cmd
}

// Install only writes an XML task. Registration runs under the current
// Windows account when the user executes the printed PowerShell command.
func installWindowsJob(home, exe, target, envFile string) (string, error) {
	dir := filepath.Join(home, ".config", "tacit", "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	var trigger, args string
	boundary := time.Now().Format("2006-01-02") + "T05:30:00"
	switch target {
	case "follow":
		trigger = "<LogonTrigger><Enabled>true</Enabled></LogonTrigger>"
		args = "follow --log " + psQuote(filepath.Join(platform.StateDir(home), "follow.log"))
	case "dream":
		trigger = "<TimeTrigger><Repetition><Interval>PT4H</Interval><StopAtDurationEnd>false</StopAtDurationEnd></Repetition><StartBoundary>" + boundary + "</StartBoundary><Enabled>true</Enabled></TimeTrigger>"
		args = "dream --env-file " + psQuote(envFile)
	case "curate":
		trigger = "<CalendarTrigger><StartBoundary>" + boundary + "</StartBoundary><Enabled>true</Enabled><ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay></CalendarTrigger>"
		args = "maintain --env-file " + psQuote(envFile)
	default:
		return "", fmt.Errorf("unknown Windows job %q", target)
	}
	script := ""
	for _, k := range []string{"TACIT_STORE", "TACIT_SETTINGS"} {
		if v := os.Getenv(k); v != "" {
			script += "$env:" + k + "=" + psQuote(v) + "; "
		}
	}
	script += "& " + psQuote(exe) + " " + args + "; exit $LASTEXITCODE"
	escape := func(s string) string { var b strings.Builder; xml.EscapeText(&b, []byte(s)); return b.String() }
	data := `<?xml version="1.0"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
<Triggers>` + trigger + `</Triggers>
<Principals><Principal id="Owner"><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
<Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><StartWhenAvailable>true</StartWhenAvailable><ExecutionTimeLimit>PT0S</ExecutionTimeLimit></Settings>
<Actions Context="Owner"><Exec><Command>powershell.exe</Command><Arguments>` + escape("-NoLogo -NoProfile -NonInteractive -EncodedCommand "+encodedPowerShell(script)) + `</Arguments></Exec></Actions></Task>
`
	path := filepath.Join(dir, "tacit-"+target+".xml")
	if err := writeFileMode(path, []byte(data), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func printWindowsJob(path, target string) {
	fmt.Println("wrote", path)
	fmt.Printf("Register in PowerShell: Register-ScheduledTask -TaskName %s -Xml (Get-Content -Raw %s) -User ([System.Security.Principal.WindowsIdentity]::GetCurrent().Name) -Force\n", psQuote("Tacit "+target), psQuote(path))
	fmt.Printf("Run now: Start-ScheduledTask -TaskName %s\n", psQuote("Tacit "+target))
}
