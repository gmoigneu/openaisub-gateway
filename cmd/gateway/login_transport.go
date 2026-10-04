// Login transfers use stdin so OAuth credentials never enter command arguments.
package main

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
)

var containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func loginTransferCommand(ctx context.Context, sshHost, dir, container, action string) *exec.Cmd {
	args := []string{"compose", "exec", "-T", "gateway", "/gateway", "auth", action}
	if container != "" {
		args = []string{"exec", "-i", container, "/gateway", "auth", action}
	}
	if sshHost != "" {
		quoted := make([]string, len(args))
		for i, arg := range args {
			quoted[i] = shellQuote(arg)
		}
		command := "docker " + strings.Join(quoted, " ")
		if container == "" {
			command = "cd " + shellQuote(dir) + " && " + command
		}
		return exec.CommandContext(ctx, "ssh", "--", sshHost, command)
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	if container == "" {
		cmd.Dir = dir
	}
	return cmd
}
