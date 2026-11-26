package queries

import "strings"

// EngineDirective marks a saved query that the connector answers with one of its own engines
// instead of running SQL. The query's whole text is one line, "-- digi-engine: <name>"; being a
// comment, it is also harmless SQL, so a store or a tool that does not know the directive breaks
// nothing (plan 099 P3: the SAP price engine reached through the saved-query route every caller
// already uses).
const EngineDirective = "-- digi-engine:"

// EngineName returns the engine a saved query names, and whether it names one. Only a text that is
// the directive and nothing else counts: SQL with the directive somewhere inside stays SQL.
func EngineName(sqlText string) (string, bool) {
	text := strings.TrimSpace(sqlText)
	if strings.ContainsAny(text, "\r\n") || !strings.HasPrefix(text, EngineDirective) {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimPrefix(text, EngineDirective))
	if name == "" || strings.ContainsAny(name, " \t") {
		return "", false
	}
	return name, true
}
