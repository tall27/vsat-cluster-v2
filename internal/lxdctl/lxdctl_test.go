package lxdctl

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeRunner records calls and returns scripted output.
type fakeRunner struct {
	listJSON string
	listErr  error
	calls    [][]string
	failOn   string // substring of first arg to fail
	// kmsgFailures makes the kmsg-fix exec fail this many times before succeeding,
	// simulating a container whose init isn't ready for `lxc exec` yet.
	kmsgFailures int
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	if f.kmsgFailures > 0 && len(args) > 0 && args[0] == "exec" && strings.Contains(joined, "kmsg") {
		f.kmsgFailures--
		return nil, errors.New("container not ready")
	}
	if f.failOn != "" && len(args) > 0 && strings.Contains(args[0], f.failOn) {
		return nil, errors.New("boom")
	}
	if len(args) > 0 && args[0] == "list" {
		return []byte(f.listJSON), f.listErr
	}
	return []byte(""), nil
}

// withZeroKmsgRetryDelay zeroes the kmsg retry backoff for the duration of a test.
func withZeroKmsgRetryDelay(t *testing.T) {
	t.Helper()
	orig := kmsgRetryDelay
	kmsgRetryDelay = 0
	t.Cleanup(func() { kmsgRetryDelay = orig })
}

const twoContainers = `[
  {"name":"vsat-a","status":"Running","state":{"network":{"eth0":{"addresses":[{"family":"inet","address":"10.115.1.10","scope":"global"},{"family":"inet6","address":"fe80::1","scope":"link"}]}}}},
  {"name":"vsat-b","status":"Stopped","state":{"network":{}}}
]`

func TestListParsesNameStatusIP(t *testing.T) {
	c := New(Options{Runner: &fakeRunner{listJSON: twoContainers}})
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 containers, got %d", len(got))
	}
	if got[0].Name != "vsat-a" || got[0].Status != "Running" || got[0].IPv4 != "10.115.1.10" {
		t.Errorf("unexpected first container: %+v", got[0])
	}
	if got[1].IPv4 != "" {
		t.Errorf("expected empty IP for stopped container, got %q", got[1].IPv4)
	}
}

func TestListSkipsLinkLocalIPv4(t *testing.T) {
	const j = `[{"name":"vsat-x","status":"Running","state":{"network":{"eth0":{"addresses":[{"family":"inet","address":"169.254.0.5","scope":"link"},{"family":"inet","address":"10.0.0.9","scope":"global"}]}}}}]`
	c := New(Options{Runner: &fakeRunner{listJSON: j}})
	got, _ := c.List(context.Background())
	if got[0].IPv4 != "10.0.0.9" {
		t.Errorf("expected global IPv4, got %q", got[0].IPv4)
	}
}

func TestValidateName(t *testing.T) {
	c := New(Options{Prefix: "vsat"})
	cases := map[string]bool{
		"vsat-a":       true,
		"vsat-test-01": true,
		"vsat-":        false, // nothing after prefix
		"other-a":      false, // wrong prefix
		"vsat-A":       false, // uppercase
		"vsat-a_b":     false, // underscore
		"":             false,
		"VSAT-a":       false,
	}
	for name, want := range cases {
		err := c.ValidateName(name)
		if (err == nil) != want {
			t.Errorf("ValidateName(%q): got err=%v, want valid=%v", name, err, want)
		}
	}
}

func TestAddEnforcesCap(t *testing.T) {
	c := New(Options{Runner: &fakeRunner{listJSON: twoContainers}, Max: 2})
	err := c.Add(context.Background(), "vsat-c")
	if err == nil || !strings.Contains(err.Error(), "limit reached") {
		t.Errorf("expected cap error, got %v", err)
	}
}

func TestAddRejectsDuplicate(t *testing.T) {
	c := New(Options{Runner: &fakeRunner{listJSON: twoContainers}, Max: 4})
	err := c.Add(context.Background(), "vsat-a")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected duplicate error, got %v", err)
	}
}

func TestAddLaunchesWithProfileAndKmsg(t *testing.T) {
	fr := &fakeRunner{listJSON: `[]`}
	c := New(Options{Runner: fr, Max: 4, Profile: "vsat-nested", Image: "ubuntu:24.04"})
	if err := c.Add(context.Background(), "vsat-a"); err != nil {
		t.Fatalf("add: %v", err)
	}
	// calls: list, launch, exec(kmsg)
	var sawLaunch, sawKmsg bool
	for _, call := range fr.calls {
		joined := strings.Join(call, " ")
		if strings.HasPrefix(joined, "launch ubuntu:24.04 vsat-a") && strings.Contains(joined, "-p vsat-nested") {
			sawLaunch = true
		}
		if call[0] == "exec" && strings.Contains(joined, "kmsg") {
			sawKmsg = true
		}
	}
	if !sawLaunch {
		t.Error("expected launch with profile")
	}
	if !sawKmsg {
		t.Error("expected kmsg workaround exec")
	}
}

func TestAddRetriesKmsgFixUntilContainerReady(t *testing.T) {
	withZeroKmsgRetryDelay(t)
	fr := &fakeRunner{listJSON: `[]`, kmsgFailures: 2}
	c := New(Options{Runner: fr, Max: 4, Profile: "vsat-nested", Image: "ubuntu:24.04"})
	if err := c.Add(context.Background(), "vsat-a"); err != nil {
		t.Fatalf("add: %v", err)
	}
	var kmsgCalls int
	for _, call := range fr.calls {
		if call[0] == "exec" && strings.Contains(strings.Join(call, " "), "kmsg") {
			kmsgCalls++
		}
	}
	if kmsgCalls != 3 {
		t.Errorf("expected 3 kmsg attempts (2 failures + 1 success), got %d", kmsgCalls)
	}
}

func TestAddFailsAfterKmsgRetriesExhausted(t *testing.T) {
	withZeroKmsgRetryDelay(t)
	fr := &fakeRunner{listJSON: `[]`, kmsgFailures: 99}
	c := New(Options{Runner: fr, Max: 4, Profile: "vsat-nested", Image: "ubuntu:24.04"})
	err := c.Add(context.Background(), "vsat-a")
	if err == nil || !strings.Contains(err.Error(), "kmsg fix") {
		t.Fatalf("expected kmsg fix error, got %v", err)
	}
	var kmsgCalls int
	for _, call := range fr.calls {
		if call[0] == "exec" && strings.Contains(strings.Join(call, " "), "kmsg") {
			kmsgCalls++
		}
	}
	if kmsgCalls != kmsgRetryAttempts {
		t.Errorf("expected %d kmsg attempts, got %d", kmsgRetryAttempts, kmsgCalls)
	}
}

func TestRemoveCallsDeleteForce(t *testing.T) {
	fr := &fakeRunner{}
	c := New(Options{Runner: fr})
	if err := c.Remove(context.Background(), "vsat-a"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	last := fr.calls[len(fr.calls)-1]
	if last[0] != "delete" || last[1] != "--force" || last[2] != "vsat-a" {
		t.Errorf("unexpected delete args: %v", last)
	}
}

func TestEnsureImageCopiesImage(t *testing.T) {
	fr := &fakeRunner{}
	c := New(Options{Runner: fr, Image: "ubuntu:24.04"})
	if err := c.EnsureImage(context.Background()); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}
	var copyCall []string
	for _, call := range fr.calls {
		if strings.Join(call, " ") != "" && call[0] == "image" && len(call) > 1 && call[1] == "copy" {
			copyCall = call
		}
	}
	if copyCall == nil {
		t.Fatalf("no image copy call found in %v", fr.calls)
	}
	if joined := strings.Join(copyCall, " "); !strings.Contains(joined, "image copy ubuntu:24.04 local:") {
		t.Errorf("unexpected image copy args: %q", joined)
	}
}

func TestShellArgs(t *testing.T) {
	c := New(Options{})
	got := strings.Join(c.ShellArgs("vsat-a"), " ")
	if got != "exec vsat-a -- bash -l" {
		t.Errorf("unexpected shell args: %q", got)
	}
}
