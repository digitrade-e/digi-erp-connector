//go:build windows

package provision

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// winConsole reads the person at this machine's console (sql-setup, plan 090 D5).
type winConsole struct {
	in *bufio.Reader
	h  windows.Handle
}

// newConsole is the console when stdin is one; nil when it is a pipe or a file (WinRM, the link, a redirect).
func newConsole() Console {
	h := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return nil
	}
	return &winConsole{in: bufio.NewReader(os.Stdin), h: h}
}

func (c *winConsole) Say(line string) { fmt.Fprintln(os.Stdout, line) }

func (c *winConsole) Ask(prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	return c.line()
}

// AskSecret turns the console's echo off for one line: the password is not shown, and is not in the console's buffer.
func (c *winConsole) AskSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	var mode uint32
	if err := windows.GetConsoleMode(c.h, &mode); err != nil {
		return "", err
	}
	if err := windows.SetConsoleMode(c.h, mode&^windows.ENABLE_ECHO_INPUT); err != nil {
		return "", err
	}
	defer func() {
		_ = windows.SetConsoleMode(c.h, mode)
		fmt.Fprintln(os.Stdout)
	}()
	return c.line()
}

func (c *winConsole) line() (string, error) {
	s, err := c.in.ReadString('\n')
	if err != nil && s == "" {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}
