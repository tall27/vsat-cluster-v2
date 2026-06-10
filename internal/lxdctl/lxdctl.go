// Package lxdctl manages LXD containers on the local host by shelling out to the
// `lxc` CLI. It lists, adds and removes VSAT containers and builds the argv used
// to open an interactive shell inside one.
package lxdctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// warmupContainerName is the throwaway container WarmPool launches and
// deletes to materialize the per-storage-pool image volume ahead of time.
// Excluded from List so it never appears in the dashboard or counts toward
// the container cap.
const warmupContainerName = "vsat-warmup"

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
//   - /dev/kmsg provided via a oneshot ordered After=journald (nested k3s
//     prerequisite, without making journald busy-loop — see below)
//   - journald set to volatile storage + watchdog disabled
//   - unnecessary services masked, k9s installed, IPv6 disabled
func (c *Client) PostLaunch(ctx context.Context, name string) error {
	// Root cause of the per-container CPU spike: nested k3s needs /dev/kmsg, so
	// the long-standing workaround symlinks it to /dev/console. But
	// systemd-journald opens /dev/kmsg at startup to import kernel logs;
	// /dev/console returns EOF immediately while epoll keeps reporting it
	// readable, so journald busy-loops on read()=0 and pins a whole core
	// (observed 40-98% per container, host load >17 with three containers).
	//
	// Fix: don't create /dev/kmsg via tmpfiles.d (which runs before journald).
	// Instead provide it from a oneshot unit ordered After=systemd-journald so
	// journald always starts while /dev/kmsg is still absent (it then never
	// opens the console and never loops). Apply journald config and restart it
	// here while /dev/kmsg is absent; k3s still gets its symlink afterwards.
	// Reboot-safe: on every boot journald starts before the oneshot runs.
	postLaunchCmd := `mkdir -p /etc/systemd/journald.conf.d && ` +
		// journald: volatile storage (no disk writes/fsyncs), rate-limited.
		`printf '[Journal]\nStorage=volatile\nCompress=no\nRateLimitIntervalSec=30\nRateLimitBurst=200\nSystemMaxUse=32M\nRuntimeMaxUse=32M\n' ` +
		`> /etc/systemd/journald.conf.d/container.conf && ` +
		// belt-and-suspenders: disable the journald service watchdog too.
		`mkdir -p /etc/systemd/system/systemd-journald.service.d && ` +
		`printf '[Service]\nWatchdogSec=0\n' ` +
		`> /etc/systemd/system/systemd-journald.service.d/override.conf && ` +
		// kmsg provider, ordered After=journald so journald boots kmsg-less.
		`printf '[Unit]\nDescription=Provide /dev/kmsg for nested k3s\nAfter=systemd-journald.service\n[Service]\nType=oneshot\nExecStart=/bin/ln -sf /dev/console /dev/kmsg\nRemainAfterExit=yes\n[Install]\nWantedBy=sysinit.target\n' ` +
		`> /etc/systemd/system/vsat-kmsg.service && ` +
		`rm -f /etc/tmpfiles.d/kmsg.conf && ` +
		`systemctl daemon-reload && ` +
		`systemctl enable vsat-kmsg.service 2>/dev/null ; ` +
		// restart journald while /dev/kmsg is absent (this must be last — the
		// restart can swallow trailing chained commands at early boot).
		`rm -f /dev/kmsg && systemctl restart systemd-journald`

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
			// Remaining steps as separate execs: the journald restart above can
			// swallow trailing chained commands at early boot, so don't chain.
			c.finalizeContainer(ctx, name) // start kmsg unit + mask services
			if err := c.installK9s(ctx, name); err != nil {
				return err
			}
			c.disableIPv6(ctx, name)
			return nil
		}
	}
	return fmt.Errorf("lxc exec kmsg fix: %w", kmsgErr)
}

// finalizeContainer starts the kmsg provider (so /dev/kmsg exists for k3s now,
// not just on the next boot) and masks services that are useless inside a
// nested-k3s container — they only burn CPU/RAM and lengthen boot. Best-effort.
//   - apport: crash reporter (SIGABRT/coredump churn); rsyslog: duplicates the
//     journal; cron, polkit, udisks2, ModemManager, multipathd, lvm2-monitor,
//     open-vm-tools, vgauth: no hardware/VMware/LVM in the container; snapd*:
//     no snaps used; ufw: firewalling is done on the host; unattended-upgrades,
//     ubuntu-advantage, pollinate, apport, networkd-dispatcher, sysstat: misc.
func (c *Client) finalizeContainer(ctx context.Context, name string) {
	cmd := `systemctl start vsat-kmsg.service 2>/dev/null ; ` +
		`systemctl mask --now ` +
		`apport rsyslog cron polkit udisks2 ` +
		`ubuntu-advantage unattended-upgrades pollinate ` +
		`systemd-timedated systemd-hostnamed console-getty ` +
		`snapd snapd.socket snapd.seeded snapd.apparmor ` +
		`ModemManager multipathd lvm2-monitor networkd-dispatcher sysstat ` +
		`ufw open-vm-tools vgauth 2>/dev/null || true`
	c.runner.Run(ctx, "exec", name, "--", "bash", "-lc", cmd)
}

// disableIPv6 turns off IPv6 inside the container (best-effort). IPv6 is unused
// in this cluster — lxdbr0 hands out IPv4 only and all NAT is IPv4 — so dropping
// it removes the eth0 IPv6 link-local address and keeps
// systemd-networkd-wait-online from ever blocking on IPv6 at boot.
//
// Done at the netplan layer (the source of truth on these Ubuntu cloud images):
// `accept-ra: false` stops Router Advertisements and `link-local: []` (an empty
// sequence) disables both IPv4 and IPv6 link-local addressing. This merges with
// the cloud-init-generated dhcp4 config, so IPv4/DHCP is untouched. See
// https://netplan.readthedocs.io/ ("Properties for all device types").
func (c *Client) disableIPv6(ctx context.Context, name string) {
	cmd := `printf 'network:\n  version: 2\n  ethernets:\n    eth0:\n      accept-ra: false\n      link-local: []\n' ` +
		`> /etc/netplan/99-disable-ipv6.yaml && chmod 600 /etc/netplan/99-disable-ipv6.yaml && ` +
		`netplan apply`
	c.runner.Run(ctx, "exec", name, "--", "bash", "-lc", cmd)
}

// installK9s puts the pinned k9s binary at /usr/local/bin/k9s in the container.
// Prefers `lxc file push`-ing the host-side cache WarmPool populates — much
// cheaper than every container curl|tar-ing the release from GitHub, which
// was found to compete for CPU/network with concurrently-booting containers
// and slow their DHCP lease acquisition (see docs/test-report.md). Falls back
// to curl|tar if the cache is missing (e.g. WarmPool hasn't run yet).
func (c *Client) installK9s(ctx context.Context, name string) error {
	if cache := k9sCachePath(); fileExists(cache) {
		if _, err := c.runner.Run(ctx, "file", "push", cache, name+"/usr/local/bin/k9s"); err == nil {
			_, err := c.runner.Run(ctx, "exec", name, "--", "chmod", "+x", "/usr/local/bin/k9s")
			return err
		}
	}
	cmd := fmt.Sprintf(
		`curl -fsSL https://github.com/derailed/k9s/releases/download/%s/k9s_Linux_amd64.tar.gz | tar -xz -C /usr/local/bin k9s && chmod +x /usr/local/bin/k9s`,
		k9sVersion,
	)
	_, err := c.runner.Run(ctx, "exec", name, "--", "bash", "-lc", cmd)
	return err
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

// WarmPool launches and immediately deletes a throwaway container so LXD
// materializes the per-storage-pool image volume ahead of time. EnsureImage
// only caches the image into LXD's image store; the unpacked rootfs volume
// that COW clones are made from is a separate artifact LXD creates lazily on
// the first lxc launch against a given (image, pool) pair. Without this,
// the first real Add pays that one-time unpack cost while later Adds just
// clone the volume this creates.
func (c *Client) WarmPool(ctx context.Context) error {
	if _, err := c.runner.Run(ctx,
		"launch", c.imageSource(ctx), warmupContainerName,
		"-p", c.Profile,
	); err != nil {
		return fmt.Errorf("lxc launch (warmup): %w", err)
	}
	c.cacheK9s(ctx, warmupContainerName)
	_, err := c.runner.Run(ctx, "delete", "--force", warmupContainerName)
	return err
}

// k9sCachePath is the host-side path WarmPool populates and PostLaunch's
// installK9s reads from, so the pinned k9s release is downloaded once per
// host instead of once per container.
func k9sCachePath() string {
	return filepath.Join(os.TempDir(), "vsat-cluster-cache", "k9s_"+k9sVersion)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// cacheK9s downloads the pinned k9s binary inside the warm-up container and
// pulls it to the host cache (k9sCachePath) for installK9s to push into real
// containers. Best-effort: errors are ignored, PostLaunch falls back to
// curl|tar if the cache ends up missing.
func (c *Client) cacheK9s(ctx context.Context, container string) {
	cmd := fmt.Sprintf(
		`curl -fsSL https://github.com/derailed/k9s/releases/download/%s/k9s_Linux_amd64.tar.gz | tar -xz -C /usr/local/bin k9s`,
		k9sVersion,
	)
	var err error
	for attempt := 0; attempt < kmsgRetryAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(kmsgRetryDelay):
			}
		}
		if _, err = c.runner.Run(ctx, "exec", container, "--", "bash", "-lc", cmd); err == nil {
			break
		}
	}
	if err != nil {
		return
	}
	cache := k9sCachePath()
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		return
	}
	c.runner.Run(ctx, "file", "pull", container+"/usr/local/bin/k9s", cache)
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
		if r.Name == warmupContainerName {
			continue
		}
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
