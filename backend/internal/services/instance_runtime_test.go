package services

import "testing"

func TestDefaultImagePullPolicy_Default(t *testing.T) {
	t.Setenv("IMAGE_PULL_POLICY", "")
	got := defaultImagePullPolicy()
	if got != "Always" {
		t.Fatalf("expected Always, got %q", got)
	}
}

func TestDefaultImagePullPolicy_HonoursEnvOverride(t *testing.T) {
	for _, envValue := range []string{"Always", "Never", "IfNotPresent"} {
		t.Run(envValue, func(t *testing.T) {
			t.Setenv("IMAGE_PULL_POLICY", envValue)
			got := defaultImagePullPolicy()
			if got != envValue {
				t.Fatalf("expected %q, got %q", envValue, got)
			}
		})
	}
}

func TestBuildRuntimeConfig_HermesUsesWebtopDefaults(t *testing.T) {
	config := buildRuntimeConfig("hermes", "hermes", "latest", nil, nil)

	if config.Port != 3001 {
		t.Fatalf("expected Hermes port 3001, got %d", config.Port)
	}
	if config.MountPath != "/config/.hermes" {
		t.Fatalf("expected Hermes mount path /config/.hermes, got %q", config.MountPath)
	}
	if config.Env["SUBFOLDER"] != "/" {
		t.Fatalf("expected Hermes default SUBFOLDER /, got %q", config.Env["SUBFOLDER"])
	}
	if !usesWebtopImage("hermes") {
		t.Fatalf("expected Hermes to use webtop proxy behavior")
	}
}

func TestResolveInitContainerSettingsOpenClaw(t *testing.T) {
	t.Setenv("OPENCLAW_SEED_IMAGE", "registry.example.com/openclaw-seed:v1")
	t.Setenv("HERMES_SEED_IMAGE", "")

	image, script, mountPath := resolveInitContainerSettings("openclaw", "/config")
	if image != "registry.example.com/openclaw-seed:v1" {
		t.Fatalf("expected OpenClaw seed image, got %q", image)
	}
	if script == "" {
		t.Fatalf("expected OpenClaw inline bootstrap script to be set")
	}
	if mountPath != "/config" {
		t.Fatalf("expected OpenClaw init mount /config, got %q", mountPath)
	}
}

func TestResolveInitContainerSettingsHermes(t *testing.T) {
	t.Setenv("OPENCLAW_SEED_IMAGE", "")
	t.Setenv("HERMES_SEED_IMAGE", "registry.example.com/hermes-seed:v1")

	image, script, mountPath := resolveInitContainerSettings("hermes", "/config/.hermes")
	if image != "registry.example.com/hermes-seed:v1" {
		t.Fatalf("expected Hermes seed image, got %q", image)
	}
	if script != "" {
		t.Fatalf("expected Hermes to use image ENTRYPOINT (empty script), got %q", script)
	}
	if mountPath != "/config/.hermes" {
		t.Fatalf("expected Hermes init mount /config/.hermes, got %q", mountPath)
	}
}

func TestResolveInitContainerSettingsDisabledWhenEnvMissing(t *testing.T) {
	t.Setenv("OPENCLAW_SEED_IMAGE", "")
	t.Setenv("HERMES_SEED_IMAGE", "")

	for _, instanceType := range []string{"openclaw", "hermes", "ubuntu"} {
		image, _, _ := resolveInitContainerSettings(instanceType, "/config")
		if image != "" {
			t.Fatalf("expected init container to be disabled for %q, got image %q", instanceType, image)
		}
	}
}

func TestResolveInitContainerSettingsUnknownType(t *testing.T) {
	t.Setenv("OPENCLAW_SEED_IMAGE", "ignored")
	t.Setenv("HERMES_SEED_IMAGE", "ignored")

	image, _, _ := resolveInitContainerSettings("ubuntu", "/config")
	if image != "" {
		t.Fatalf("expected ubuntu to have no init container, got %q", image)
	}
}
