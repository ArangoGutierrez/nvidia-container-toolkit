package oci

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateCommand(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		wantErr bool
	}{
		{
			name:    "valid runc command",
			cmd:     "runc",
			wantErr: false,
		},
		{
			name:    "valid crun command",
			cmd:     "crun",
			wantErr: false,
		},
		{
			name:    "valid youki command",
			cmd:     "youki",
			wantErr: false,
		},
		{
			name:    "valid runsc command",
			cmd:     "runsc",
			wantErr: false,
		},
		{
			name:    "valid kata-runtime command",
			cmd:     "kata-runtime",
			wantErr: false,
		},
		{
			name:    "valid command with path",
			cmd:     "/usr/bin/runc",
			wantErr: false,
		},
		{
			name:    "invalid command",
			cmd:     "malicious-command",
			wantErr: true,
		},
		{
			name:    "command with special characters",
			cmd:     "cmd;rm",
			wantErr: true,
		},
		{
			name:    "empty command",
			cmd:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCommand(tt.cmd)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSanitizeArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{
			name:    "valid runc command with args",
			args:    []string{"runc", "create", "container1"},
			wantErr: false,
		},
		{
			name:    "valid command with path and args",
			args:    []string{"/usr/bin/runc", "create", "container1"},
			wantErr: false,
		},
		{
			name:    "empty args",
			args:    []string{},
			wantErr: true,
		},
		{
			name:    "invalid command",
			args:    []string{"malicious-command", "arg1"},
			wantErr: true,
		},
		{
			name:    "args with dangerous characters",
			args:    []string{"runc", "create;rm", "container1"},
			wantErr: true,
		},
		{
			name:    "args with shell metacharacters",
			args:    []string{"runc", "create", "container1;rm -rf /"},
			wantErr: true,
		},
		{
			name:    "args with command substitution",
			args:    []string{"runc", "create", "$(rm -rf /)"},
			wantErr: true,
		},
		{
			name:    "args with invalid characters",
			args:    []string{"runc", "create", "container1@#$"},
			wantErr: true,
		},
		{
			name:    "args with valid special characters",
			args:    []string{"runc", "create", "container-1.2"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := sanitizeArgs(tt.args)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateExecutable(t *testing.T) {
	// Create a temporary directory for test files
	tmpDir := t.TempDir()

	// Create test files with different permissions
	execFile := filepath.Join(tmpDir, "executable")
	nonExecFile := filepath.Join(tmpDir, "non-executable")
	dirFile := filepath.Join(tmpDir, "directory")

	// Create executable file with minimal permissions
	err := os.WriteFile(execFile, []byte("#!/bin/sh\nexit 0"), 0600)
	require.NoError(t, err)
	err = os.Chmod(execFile, 0700)
	require.NoError(t, err)

	// Create non-executable file
	err = os.WriteFile(nonExecFile, []byte("not executable"), 0600)
	require.NoError(t, err)

	// Create directory
	err = os.Mkdir(dirFile, 0755)
	require.NoError(t, err)

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{
			name:    "valid executable",
			path:    execFile,
			wantErr: false,
		},
		{
			name:    "non-existent file",
			path:    filepath.Join(tmpDir, "nonexistent"),
			wantErr: true,
		},
		{
			name:    "non-executable file",
			path:    nonExecFile,
			wantErr: true,
		},
		{
			name:    "directory",
			path:    dirFile,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateExecutable(tt.path)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSyscallExec(t *testing.T) {
	// Create a temporary executable file for testing
	tmpDir := t.TempDir()
	testExec := filepath.Join(tmpDir, "test-exec")
	err := os.WriteFile(testExec, []byte("#!/bin/sh\nexit 0"), 0600)
	require.NoError(t, err)
	err = os.Chmod(testExec, 0700)
	require.NoError(t, err)

	// Add the test executable to allowed commands
	allowedCommands["test-exec"] = true
	defer delete(allowedCommands, "test-exec")

	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{
			name:    "valid command execution",
			args:    []string{testExec},
			wantErr: false,
		},
		{
			name:    "non-existent command",
			args:    []string{"/nonexistent/command"},
			wantErr: true,
		},
		{
			name:    "command with invalid args",
			args:    []string{testExec, "arg1;rm -rf /"},
			wantErr: true,
		},
		{
			name:    "command with invalid characters in args",
			args:    []string{testExec, "arg1@#$"},
			wantErr: true,
		},
	}

	r := syscallExec{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.Exec(tt.args)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				// Note: In a real test, syscall.Exec would replace the current process
				// so we can't actually verify successful execution
				require.Error(t, err)
				require.Contains(t, err.Error(), "unexpected return from exec")
			}
		})
	}
}

func TestSyscallExecString(t *testing.T) {
	r := syscallExec{}
	require.Equal(t, "exec", r.String())
}
