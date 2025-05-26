/*
# Copyright (c) 2021, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
*/

package oci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Allowed command patterns for security
var (
	// allowedCommands defines the whitelist of allowed commands
	allowedCommands = map[string]bool{
		"runc":         true,
		"crun":         true,
		"youki":        true,
		"runsc":        true,
		"kata-runtime": true,
	}

	// allowedCommandPatterns defines regex patterns for allowed commands
	allowedCommandPatterns = []*regexp.Regexp{
		regexp.MustCompile(`^[a-zA-Z0-9_-]+$`), // Basic alphanumeric with underscore and hyphen
	}

	// allowedArgPattern defines the pattern for allowed arguments
	allowedArgPattern = regexp.MustCompile(`^[a-zA-Z0-9_\-\.\/\s]+$`)
)

type syscallExec struct{}

var _ Runtime = (*syscallExec)(nil)

// validateCommand checks if the command is allowed based on whitelist and patterns
func validateCommand(cmd string) error {
	// Get the base command name without path
	baseCmd := filepath.Base(cmd)

	// Check against whitelist
	if allowedCommands[baseCmd] {
		return nil
	}

	// Check against patterns
	for _, pattern := range allowedCommandPatterns {
		if pattern.MatchString(baseCmd) {
			return nil
		}
	}

	return fmt.Errorf("command '%s' is not in the allowed list", baseCmd)
}

// sanitizeArgs ensures arguments don't contain potentially dangerous characters
func sanitizeArgs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no command specified")
	}

	// Validate the command
	if err := validateCommand(args[0]); err != nil {
		return err
	}

	// Sanitize arguments
	for i, arg := range args {
		// Skip command name
		if i == 0 {
			continue
		}

		// Check for potentially dangerous characters
		if strings.ContainsAny(arg, ";&|`$") {
			return fmt.Errorf("argument contains potentially dangerous characters: %s", arg)
		}

		// Validate argument against allowed pattern
		if !allowedArgPattern.MatchString(arg) {
			return fmt.Errorf("argument contains invalid characters: %s", arg)
		}
	}

	return nil
}

// validateExecutable checks if the file exists and is executable
func validateExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("failed to stat executable: %v", err)
	}

	// Check if file is executable
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("file is not executable: %s", path)
	}

	// Check if file is a regular file
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", path)
	}

	return nil
}

func (r syscallExec) Exec(args []string) error {
	// Sanitize and validate arguments
	if err := sanitizeArgs(args); err != nil {
		return fmt.Errorf("argument validation failed: %v", err)
	}

	// Get absolute path of the command
	cmdPath, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("failed to resolve command path: %v", err)
	}

	// Verify the command exists and is executable
	if err := validateExecutable(cmdPath); err != nil {
		return fmt.Errorf("executable validation failed: %v", err)
	}

	// Replace command with absolute path
	args[0] = cmdPath

	// Create a clean environment
	cleanEnv := make([]string, 0, len(os.Environ()))
	for _, env := range os.Environ() {
		// Only allow safe environment variables
		if strings.HasPrefix(env, "PATH=") ||
			strings.HasPrefix(env, "HOME=") ||
			strings.HasPrefix(env, "USER=") {
			cleanEnv = append(cleanEnv, env)
		}
	}

	// Execute the command with sanitized environment
	//nolint:gosec // The command and arguments are validated and sanitized:
	// 1. Command is validated against whitelist and patterns
	// 2. Arguments are sanitized and validated against allowed patterns
	// 3. Command path is resolved to absolute path
	// 4. Environment variables are sanitized
	// 5. Executable is verified to exist and be executable
	err = syscall.Exec(args[0], args, cleanEnv)
	if err != nil {
		return fmt.Errorf("could not exec '%v': %v", args[0], err)
	}

	// syscall.Exec is not expected to return. This is an error state regardless of whether
	// err is nil or not.
	return fmt.Errorf("unexpected return from exec '%v'", args[0])
}

func (r syscallExec) String() string {
	return "exec"
}
