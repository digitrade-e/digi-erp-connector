//go:build windows

package provision

import (
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

// platformEvents reads source's newest Application-log entries (at most max, newest first, at or after since)
// through Get-WinEvent — a fixed script, its inputs in the environment. A source never registered, or with no
// entries, is no entries.
func platformEvents(source string, since time.Time, max int) ([]EventRecord, error) {
	sinceUnix := ""
	if !since.IsZero() {
		sinceUnix = fmt.Sprint(since.Unix())
	}
	out, err := powershell(`
$f = @{ LogName = 'Application'; ProviderName = $env:DIGI_SOURCE }
if ($env:DIGI_SINCE) { $f.StartTime = [DateTimeOffset]::FromUnixTimeSeconds([long]$env:DIGI_SINCE).LocalDateTime }
$ev = @(Get-WinEvent -FilterHashtable $f -MaxEvents ([int]$env:DIGI_MAX) -ErrorAction SilentlyContinue)
$out = foreach ($e in $ev) {
  $msg = [string]$e.Message
  if (-not $msg) { $msg = (@($e.Properties | ForEach-Object { [string]$_.Value }) -join ' ') }
  [ordered]@{ time = $e.TimeCreated.ToUniversalTime().ToString('o'); level = [string]$e.LevelDisplayName; id = [int]$e.Id; message = $msg }
}
ConvertTo-Json -Compress -Depth 3 -InputObject @($out)`, map[string]string{
		"DIGI_SOURCE": source, "DIGI_SINCE": sinceUnix, "DIGI_MAX": fmt.Sprint(max),
	})
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Time    string `json:"time"`
		Level   string `json:"level"`
		ID      int    `json:"id"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("the Event Log entries do not decode: %w", err)
	}
	events := make([]EventRecord, 0, len(raw))
	for _, r := range raw {
		t, _ := time.Parse(time.RFC3339Nano, r.Time)
		events = append(events, EventRecord{Time: t, Level: r.Level, ID: r.ID, Message: r.Message})
	}
	return events, nil
}

// diskSpace is the free and total bytes of the volume holding path.
func diskSpace(path string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var avail, tot, totFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &tot, &totFree); err != nil {
		return 0, 0, err
	}
	return avail, tot, nil
}
