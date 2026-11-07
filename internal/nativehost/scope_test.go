package nativehost

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type captureCommand struct {
	binary string
	args   []string
	fail   bool
}

func (c *captureCommand) Run(_ context.Context, binary string, args ...string) ([]byte, error) {
	c.binary = binary
	c.args = append([]string{}, args...)
	if c.fail {
		return nil, errors.New("user manager unavailable")
	}
	return []byte("ok"), nil
}
func TestPodmanLaunchUsesIndependentSessionScope(t *testing.T) {
	c := &captureCommand{}
	p := Podman{Binary: "/usr/bin/podman", SystemdRun: "/usr/bin/systemd-run", executor: c}
	args := []string{"run", "-d", "--name", "burrow-native-" + sid, "--pull=never", image}
	if _, err := p.Run(context.Background(), args...); err != nil {
		t.Fatal(err)
	}
	expected := []string{"--user", "--scope", "--collect", "--quiet", "--slice=app.slice", "--unit=burrow-native-" + sid + ".scope", "--", "/usr/bin/podman", "--cgroup-manager=systemd"}
	expected = append(expected, args...)
	if c.binary != "/usr/bin/systemd-run" || !reflect.DeepEqual(c.args, expected) {
		t.Fatalf("launch escaped independent scope: %q %q", c.binary, c.args)
	}
}
func TestScopeFailureNeverFallsBackToHostCgroup(t *testing.T) {
	c := &captureCommand{fail: true}
	p := Podman{executor: c}
	if _, err := p.Run(context.Background(), "run", "-d", "--name", "burrow-native-"+sid, image); err == nil {
		t.Fatal("scope failure accepted")
	}
	if c.binary != "systemd-run" {
		t.Fatalf("unscoped fallback: %s", c.binary)
	}
}
func TestScopeRejectsNonSessionUnitAndLeavesInspectRemoveUnscoped(t *testing.T) {
	c := &captureCommand{}
	p := Podman{executor: c}
	if _, err := p.Run(context.Background(), "run", "-d", "--name", "arbitrary.service", image); err == nil || c.binary != "" {
		t.Fatal("foreign unit executed")
	}
	for _, verb := range []string{"inspect", "rm", "ps"} {
		if _, err := p.Run(context.Background(), verb, "container"); err != nil {
			t.Fatal(err)
		}
		if c.binary != "podman" || !reflect.DeepEqual(c.args, []string{"--cgroup-manager=systemd", verb, "container"}) {
			t.Fatalf("unexpected %s: %q %q", verb, c.binary, c.args)
		}
	}
}
