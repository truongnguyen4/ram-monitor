// Package adb wraps the adb command-line tool.
package adb

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Client runs adb commands using the binary at Path.
type Client struct {
	Path string
}

// Device is one entry of `adb devices -l`.
type Device struct {
	Serial string `json:"serial"`
	State  string `json:"state"`
	Model  string `json:"model"`
}

// Find locates the adb binary: explicit path, then PATH, then common SDK locations.
func Find(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("adb not found at %q: %w", explicit, err)
		}
		return explicit, nil
	}
	name := "adb"
	if runtime.GOOS == "windows" {
		name = "adb.exe"
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	var dirs []string
	for _, env := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := os.Getenv(env); v != "" {
			dirs = append(dirs, filepath.Join(v, "platform-tools"))
		}
	}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		dirs = append(dirs, d, filepath.Join(d, "platform-tools"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		switch runtime.GOOS {
		case "windows":
			if la := os.Getenv("LOCALAPPDATA"); la != "" {
				dirs = append(dirs, filepath.Join(la, "Android", "Sdk", "platform-tools"))
			}
		case "darwin":
			dirs = append(dirs, filepath.Join(home, "Library", "Android", "sdk", "platform-tools"))
		default:
			dirs = append(dirs, filepath.Join(home, "Android", "Sdk", "platform-tools"))
		}
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("adb not found: add platform-tools to PATH or use -adb <path>")
}

// Devices lists attached devices.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	out, err := c.run(ctx, "devices", "-l")
	if err != nil {
		return nil, err
	}
	return parseDevices(out), nil
}

func parseDevices(out []byte) []Device {
	var devs []Device
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		d := Device{Serial: f[0], State: f[1]}
		for _, tok := range f[2:] {
			if strings.HasPrefix(tok, "model:") {
				d.Model = strings.ReplaceAll(strings.TrimPrefix(tok, "model:"), "_", " ")
			}
		}
		devs = append(devs, d)
	}
	return devs
}

// Shell runs a one-shot shell command on the device and returns stdout.
func (c *Client) Shell(ctx context.Context, serial, script string) ([]byte, error) {
	return c.run(ctx, "-s", serial, "shell", script)
}

// Reboot restarts the device.
func (c *Client) Reboot(ctx context.Context, serial string) error {
	_, err := c.run(ctx, "-s", serial, "reboot")
	return err
}

// Stream starts a long-running shell command and returns its stdout.
// The command stops when ctx is cancelled; call Wait on the returned cmd.
func (c *Client) Stream(ctx context.Context, serial, script string) (io.ReadCloser, *exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, c.Path, "-s", serial, "shell", script)
	hideWindow(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, cmd, nil
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Path, args...)
	hideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("adb %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}
