package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

var autoYesFlag bool

func info(msg string) { fmt.Printf("  • %s\n", msg) }
func ok(msg string)   { fmt.Printf("  ✓ %s\n", msg) }
func warn(msg string) { fmt.Printf("  ! %s\n", msg) }

// fail prints msg to stderr and exits non-zero. Use for unrecoverable errors
// during install where there's nothing meaningful to clean up.
func fail(msg string) {
	fmt.Fprintf(os.Stderr, "\n  ✗ %s\n\n", msg)
	os.Exit(1)
}

// confirm asks the user a y/n question on stdin. Honors --yes.
func confirm(prompt string, defaultYes bool) bool {
	if autoYesFlag {
		fmt.Printf("\n  %s [auto: yes]\n", prompt)
		return true
	}
	suffix := " [y/N] "
	if defaultYes {
		suffix = " [Y/n] "
	}
	fmt.Printf("\n  %s%s", prompt, suffix)
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans == "" {
		return defaultYes
	}
	return ans == "y" || ans == "yes"
}
