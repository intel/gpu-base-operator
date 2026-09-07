/*
Copyright 2026 Intel Corporation. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"strings"
	"time"
)

// dash is printed for any field the CRD marks optional and the operator has not set,
// so that an empty column reads as "nothing recorded" rather than as a formatting bug.
const dash = "-"

// compactDuration renders d in the abbreviated style kubectl uses for AGE columns.
func compactDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// formatTime renders an RFC3339 timestamp from a status field as an age ("5m ago").
func formatTime(ts string) string {
	if ts == "" {
		return dash
	}

	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}

	return compactDuration(time.Since(t)) + " ago"
}

// truncate shortens s to max runes, ending in an ellipsis. Used for status.events[].stateMessage,
// which the CRD documents as truncated but still long enough to wrap a terminal.
func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return s
	}

	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}

	return string(r[:maxLen-1]) + "…"
}

// orDash returns s, or the placeholder when s is empty.
func orDash(s string) string {
	if s == "" {
		return dash
	}

	return s
}

// joinOrDash renders a status list ("namespace/name" entries) as one column value.
func joinOrDash(items []string) string {
	if len(items) == 0 {
		return dash
	}

	return strings.Join(items, ",")
}
