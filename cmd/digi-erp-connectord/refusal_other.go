//go:build !windows

package main

// reportRefusal: off Windows the early log already went to stderr.
func reportRefusal([]string, error) {}

func clearRefusalState() {}
