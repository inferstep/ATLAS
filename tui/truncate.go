package main

// truncate cuts s to n characters, the last of them "…". It cuts between
// characters, never inside one: a cut inside a character would leave a broken
// byte after the text was marked (visibleControls).
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}
