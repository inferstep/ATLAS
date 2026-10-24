package main

import "testing"

// A `text` reply ends the turn, so a reply that announces a tool call instead
// of making one stops with the intent and no action. An answer that only
// mentions reading is not an announcement.
func TestAnnouncesImminentToolUse(t *testing.T) {
	for _, c := range []struct {
		text      string
		announces bool
	}{
		{"I need to read the `orders.py` file to explain what find_duplicates does.", true},
		{"I'll start by outlining the file to locate the function.", true},
		{"Let me look at the implementation first.", true},
		{"I'm going to check store.py before answering.", true},
		{"I'll look into the contact form's implementation to see how it handles submissions and where the data is sent.", true},
		{"Let me look through the source to find where that is set.", true},
		{"I'm going to investigate how routing works here.", true},
		{"First, I will read app.py to understand the structure.", true},
		{"I'll dig into the handler and report back.", true},
		{"find_duplicates is O(n^2): it reads the list inside a nested loop.", false},
		{"I looked at both files: routing is handled by Flask's @app.route decorators.", false},
		{"", false},
	} {
		t.Run(c.text, func(t *testing.T) {
			if got := announcesImminentToolUse(c.text); got != c.announces {
				t.Errorf("announcesImminentToolUse = %v, want %v", got, c.announces)
			}
		})
	}
}

// A reply that signs off promising the answer leaves the user half of one. A
// promise followed by what it promised is a complete answer.
func TestPromisesMoreContent(t *testing.T) {
	for _, c := range []struct {
		text     string
		promises bool
	}{
		{"The bug is in planning.py. I will now provide the specific location and the incorrect comparison as requested.", true},
		{"I've analysed the file. Let me give you the exact line.", true},
		{"Here's the summary so far. I'll now show the failing comparison.", true},
		{"planning.py breaks ties with n_steps > best_steps, so a tie keeps the longer plan.", false},
		{"I'll provide the details: the comparison on line 314 uses > where it should use <.", false},
		{"", false},
	} {
		t.Run(c.text, func(t *testing.T) {
			if got := promisesMoreContent(c.text); got != c.promises {
				t.Errorf("promisesMoreContent = %v, want %v", got, c.promises)
			}
		})
	}
}
