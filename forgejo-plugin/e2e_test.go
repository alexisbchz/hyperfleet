// e2e_test exercises the plugin end-to-end:
//
//	runner -> NewClientV2(plugin-binary) -> go-plugin -> our gRPC server
//	  -> hyperfleet REST -> microVM -> initd -> back through the wire
//
// Skipped unless HYPERFLEET_E2E=1 because it needs:
//   - hyperfleet daemon listening on $HYPERFLEET_API_URL with $HYPERFLEET_API_KEY
//   - bin/hyperfleet-init present in the daemon's repo (for rootfs injection)
//
// Run from the forgejo-plugin/ directory. The plugin binary must be built first
// (see ../Makefile target `plugin`) and its path passed via HYPERFLEET_PLUGIN_BIN.
package main

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	pluginsdk "code.forgejo.org/forgejo/runner/v12/act/plugin"
	"code.forgejo.org/forgejo/runner/v12/act/container"
)

func TestEndToEnd(t *testing.T) {
	if os.Getenv("HYPERFLEET_E2E") != "1" {
		t.Skip("set HYPERFLEET_E2E=1 to run")
	}
	binPath := os.Getenv("HYPERFLEET_PLUGIN_BIN")
	if binPath == "" {
		binPath = "../bin/hyperfleet-forgejo-plugin"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client, err := pluginsdk.NewClientV2(ctx, binPath, pluginsdk.WithLogLevel("debug"))
	if err != nil {
		t.Fatalf("NewClientV2: %v", err)
	}
	// t.Cleanup is LIFO: registering Close first means it runs LAST, after
	// every per-env Remove cleanup further down. Otherwise the plugin
	// process gets killed before we can issue Remove and the VM leaks.
	t.Cleanup(func() { client.Close() })

	caps := client.Capabilities()
	if caps.GetName() != "hyperfleet" {
		t.Fatalf("caps name = %q, want hyperfleet", caps.GetName())
	}
	t.Logf("capabilities: name=%s root=%s manages_net=%v", caps.GetName(), caps.GetRootPath(), caps.GetManagesOwnNetworking())

	var stdout, stderr bytes.Buffer
	env := client.NewEnvironment(&container.NewContainerInput{
		Image:      "docker.io/library/alpine:3.20",
		Name:       "hf-e2e",
		Env:        []string{"HF_TEST=1"},
		WorkingDir: "/shared/work",
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, map[string]string{})

	if err := env.Pull(false)(ctx); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if err := env.Create(nil, nil)(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		_ = env.Remove()(context.Background())
	})
	if err := env.Start(false)(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// echo -n hello → should produce exactly "hello" on stdout, no stderr.
	stdout.Reset()
	stderr.Reset()
	if err := env.Exec([]string{"sh", "-c", "echo -n hello; echo -n boom 1>&2; exit 0"},
		nil, "", "/")(ctx); err != nil {
		t.Fatalf("Exec hello: %v", err)
	}
	if got := stdout.String(); got != "hello" {
		t.Errorf("stdout = %q, want %q", got, "hello")
	}
	if got := stderr.String(); got != "boom" {
		t.Errorf("stderr = %q, want %q", got, "boom")
	}

	// Non-zero exit must surface as a *plugin.ExecError.
	stdout.Reset()
	stderr.Reset()
	if err := env.Exec([]string{"sh", "-c", "exit 9"}, nil, "", "/")(ctx); err == nil {
		t.Errorf("Exec exit-9: expected error, got nil")
	}

	// Copy a file in via the framework's Copy helper, then read it back via exec.
	if err := env.Copy("/shared/work", &container.FileEntry{Name: "hello.txt", Mode: 0o644, Body: "world\n"})(ctx); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	stdout.Reset()
	if err := env.Exec([]string{"cat", "/shared/work/hello.txt"}, nil, "", "/")(ctx); err != nil {
		t.Fatalf("Exec cat: %v", err)
	}
	if got := stdout.String(); got != "world\n" {
		t.Errorf("cat = %q, want %q", got, "world\n")
	}

	// IsHealthy should not error on a running env.
	if _, err := env.IsHealthy(ctx); err != nil {
		t.Errorf("IsHealthy: %v", err)
	}
}
