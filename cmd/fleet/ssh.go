package main

import (
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func sshAttach(host, port, machineID, apiKey string) error {
	cfg := &ssh.ClientConfig{
		User:            machineID,
		Auth:            []ssh.AuthMethod{ssh.Password(apiKey)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // v0: TOFU verification deferred
		Timeout:         10 * time.Second,
	}

	conn, err := ssh.Dial("tcp", net.JoinHostPort(host, port), cfg)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		state, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("raw mode: %w", err)
		}
		defer term.Restore(fd, state)

		w, h, err := term.GetSize(fd)
		if err != nil {
			w, h = 80, 24
		}
		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}
		if err := sess.RequestPty("xterm-256color", h, w, modes); err != nil {
			return fmt.Errorf("request pty: %w", err)
		}
	}

	sess.Stdin = os.Stdin
	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr

	if err := sess.Shell(); err != nil {
		return fmt.Errorf("shell: %w", err)
	}
	if err := sess.Wait(); err != nil {
		var exitErr *ssh.ExitError
		// io.EOF or clean exit shows up as nil; ignore non-zero remote exit.
		if _, ok := err.(*ssh.ExitMissingError); ok {
			return nil
		}
		_ = exitErr
	}
	return nil
}
