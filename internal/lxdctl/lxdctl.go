// Package lxdctl manages LXD containers on the local host by shelling out to the
// `lxc` CLI. It lists, adds and removes VSAT containers and builds the argv used
// to open an interactive shell inside one.
package lxdctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

// kmsgRetry* bound how long Add waits for a freshly launched container's init
// to be ready for `lxc exec` before giving up on the /dev/kmsg workaround.
// `lxc launch` returns once the container starts booting, not once it can be
// exec'd into, so a one-shot attempt can race a slow boot and leave the
// container created but without the fix (vars so tests can zero the delay).
var (
	kmsgRetryAttempts = 15
	kmsgRetryDelay    = 2 * time.Second
)

// k9sVersion pins the k9s release installed into every new container, so
// installs are reproducible and don't depend on GitHub's releases API from
// inside the container (rate limits, drift). Bump deliberately when needed.
const k9sVersion = "v0.51.0"

// Container is the trimmed view of an LXD instance the UI needs.
type Container struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	IPv4   string `json:"ipv4"`
}

// rawContainer mirrors the subset of `lxc list --format json` we parse.
type rawContainer struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	State  struct {
		Network map[string]struct {
			Addresses []struct {
				Family  string `json:"family"`
				Address string `json:"address"`
				Scope   string `json:"scope"`
			} `json:"addresses"`
		} `json:"network"`
	} `json:"state"`
}

// CommandRunner executes an lxc command and returns combined stdout.
type CommandRunner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Client manages containers via lxc.
type Client struct {
	runner  CommandRunner
	Profile string
	Image   string
	Prefix  string
	Max     int
}

// Options configures a Client.
type Options struct {
	// Bin is the lxc binary (default "lxc").
	Bin string
	// Sudo prefixes commands with sudo -n when true.
	Sudo bool
	// Profile is the LXD profile applied to new containers.
	Profile string
	// Image is the base image for new containers.
	Image string
	// Prefix is the required container-name prefix.
	Prefix string
	// Max is the container cap.
	Max int
	// Runner overrides command execution (used in tests).
	Runner CommandRunner
}

// New builds a Client from Options, applying sensible defaults.
func New(opts Options) *Client {
	if opts.Profile == "" {
		opts.Profile = "vsat-nested"
	}
	if opts.Image == "" {
		opts.Image = "ubuntu:24.04"
	}
	if opts.Prefix == "" {
		opts.Prefix = "vsat"
	}
	if opts.Max <= 0 {
		opts.Max = 4
	}
	runner := opts.Runner
	if runner == nil {
		bin := opts.Bin
		if bin == "" {
			bin = "lxc"
		}
		runner = &execRunner{bin: bin, sudo: opts.Sudo}
	}
	return &Client{
		runner:  runner,
		Profile: opts.Profile,
		Image:   opts.Image,
		Prefix:  opts.Prefix,
		Max:     opts.Max,
	}
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}$`)

// ValidateName checks that a container name is prefixed and LXD-legal.
func (c *Client) ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name is required")
	}
	if !strings.HasPrefix(name, c.Prefix+"-") {
		return fmt.Errorf("name must start with %q", c.Prefix+"-")
	}
	if strings.TrimPrefix(name, c.Prefix+"-") == "" {
		return fmt.Errorf("name needs a suffix after %q", c.Prefix+"-")
	}
	if !nameRe.MatchString(name) {
		return errors.New("name must be lowercase letters, digits and hyphens, starting with a letter")
	}
	return nil
}

// List returns the current containers, sorted by name.
func (c *Client) List(ctx context.Context) ([]Container, error) {
	out, err := c.runner.Run(ctx, "list", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("lxc list: %w", err)
	}
	return parseContainers(out)
}

// Add launches a new container with the configured profile, enforcing the cap,
// then applies post-launch fixes/tools. Calls Launch then PostLaunch
// sequentially; use them separately to run PostLaunch in the background.
func (c *Client) Add(ctx context.Context, name string) error {
	if err := c.Launch(ctx, name); err != nil {
		return err
	}
	return c.PostLaunch(ctx, name)
}

// Launch creates and starts the container, returning as soon as lxc launch
// completes (the container is booting). Call PostLaunch separately to apply
// in-container fixes and tools without blocking the caller.
func (c *Client) Launch(ctx context.Context, name string) error {
	if err := c.ValidateName(name); err != nil {
		return err
	}
	existing, err := c.List(ctx)
	if err != nil {
		return err
	}
	for _, ct := range existing {
		if ct.Name == name {
			return fmt.Errorf("container %q already exists", name)
		}
	}
	if len(existing) >= c.Max {
		return fmt.Errorf("container limit reached (%d/%d)", len(existing), c.Max)
	}
	if _, err := c.runner.Run(ctx,
		"launch", c.imageSource(ctx), name,
		"-p", c.Profile,
		"-c", "limits.cpu=2",
		"-c", "limits.memory=3GiB",
	); err != nil {
		return fmt.Errorf("lxc launch: %w", err)
	}
	return nil
}

// PostLaunch applies in-container fixes and tools after Launch. Safe to call
// in a goroutine — retries until the container's init is ready for exec.
//   - /dev/kmsg workaround (nested k3s prerequisite)
//   - journald watchdog disabled (avoids SIGABRT/apport churn under k3s log volume)
//   - k9s installed (pinned version) for in-container cluster inspection
func (c *Client) PostLaunch(ctx context.Context, name string) error {
	postLaunchCmd := fmt.Sprintf(
		`printf 'L /dev/kmsg - - - - /dev/console\n' > /etc/tmpfiles.d/kmsg.conf && `+
			`systemd-tmpfiles --create /etc/tmpfiles.d/kmsg.conf && `+
			`mkdir -p /etc/systemd/journald.conf.d && `+
			`printf '[Journal]\nWatchdogSec=0\n' > /etc/systemd/journald.conf.d/no-watchdog.conf && `+
			`systemctl restart systemd-journald && `+
			`curl -fsSL https://github.com/derailed/k9s/releases/download/%s/k9s_Linux_amd64.tar.gz | tar -xz -C /usr/local/bin k9s && `+
			`chmod +x /usr/local/bin/k9s`,
		k9sVersion,
	)
	var kmsgErr error
	for attempt := 0; attempt < kmsgRetryAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("lxc exec kmsg fix: %w", ctx.Err())
			case <-time.After(kmsgRetryDelay):
			}
		}
		if _, kmsgErr = c.runner.Run(ctx,
			"exec", name, "--",
			"bash", "-lc", postLaunchCmd,
		); kmsgErr == nil {
			return nil
		}
	}
	return fmt.Errorf("lxc exec kmsg fix: %w", kmsgErr)
}

// EnsureImage pre-caches the configured image in local LXD storage so that
// subsequent lxc launches don't block on a remote download. Returns nil
// immediately if the image is already present locally (idempotent).
func (c *Client) EnsureImage(ctx context.Context) error {
	if src := c.imageSource(ctx); src != c.Image {
		return nil // already cached
	}
	_, err := c.runner.Run(ctx, "image", "copy", c.Image, "local:", "--copy-aliases", "--auto-update")
	return err
}

// imageSource returns "local:<alias>" when the image is already cached locally,
// falling back to c.Image (remote) so Add never blocks on a re-download.
// The local alias is the portion of c.Image after the remote prefix (e.g.
// "ubuntu:24.04" → "24.04"), which is what --copy-aliases creates.
func (c *Client) imageSource(ctx context.Context) string {
	alias := c.Image
	if i := strings.LastIndex(c.Image, ":"); i >= 0 {
		alias = c.Image[i+1:]
	}
	out, err := c.runner.Run(ctx, "image", "list", "local:"+alias, "--format", "json")
	if err == nil {
		var imgs []struct{}
		if json.Unmarshal(out, &imgs) == nil && len(imgs) > 0 {
			return "local:" + alias
		}
	}
	return c.Image
}

// Remove force-deletes a container.
func (c *Client) Remove(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name is required")
	}
	if _, err := c.runner.Run(ctx, "delete", "--force", name); err != nil {
		return fmt.Errorf("lxc delete: %w", err)
	}
	return nil
}

// ShellArgs returns the lxc argv that opens an interactive root shell inside name.
func (c *Client) ShellArgs(name string) []string {
	return []string{"exec", name, "--", "bash", "-l"}
}

func parseContainers(data []byte) ([]Container, error) {
	var raw []rawContainer
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse lxc json: %w", err)
	}
	out := make([]Container, 0, len(raw))
	for _, r := range raw {
		out = append(out, Container{
			Name:   r.Name,
			Status: r.Status,
			IPv4:   firstGlobalIPv4(r),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func firstGlobalIPv4(r rawContainer) string {
	for _, iface := range []string{"eth0", "enp5s0"} {
		if net, ok := r.State.Network[iface]; ok {
			if ip := pickIPv4(net.Addresses); ip != "" {
				return ip
			}
		}
	}
	// Fall back to any non-loopback interface.
	for name, net := range r.State.Network {
		if name == "lo" {
			continue
		}
		if ip := pickIPv4(net.Addresses); ip != "" {
			return ip
		}
	}
	return ""
}

func pickIPv4(addrs []struct {
	Family  string `json:"family"`
	Address string `json:"address"`
	Scope   string `json:"scope"`
}) string {
	for _, a := range addrs {
		if a.Family == "inet" && a.Scope != "link" {
			return a.Address
		}
	}
	return ""
}

// execRunner is the production CommandRunner using the lxc CLI.
type execRunner struct {
	bin  string
	sudo bool
}

func (e *execRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	name := e.bin
	full := args
	if e.sudo {
		name = "sudo"
		full = append([]string{"-n", e.bin}, args...)
	}
	cmd := exec.CommandContext(ctx, name, full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(full, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
