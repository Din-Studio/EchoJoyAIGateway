package webui

import (
	"strings"
	"testing"
)

func TestSelfHostedCIKeepsPlatformGatesAndLocalCaches(t *testing.T) {
	ci := readRepositoryFile(t, ".github/workflows/ci.yml")
	for job, runner := range map[string]string{
		"test":       "[self-hosted, macOS, ARM64]",
		"race-tests": "[self-hosted, Linux, ARM64]",
		"race-cpa":   "[self-hosted, Linux, ARM64]",
	} {
		block := workflowJobBlock(t, ci, job)
		if !strings.Contains(block, "runs-on: "+runner) {
			t.Errorf("%s is not assigned to %s", job, runner)
		}
	}
	for _, file := range []string{"ci.yml", "release.yml"} {
		content := readRepositoryFile(t, ".github/workflows/"+file)
		if strings.Contains(content, "cache: true") || strings.Contains(content, ".go-cache-scope") {
			t.Errorf("%s still restores remote Go caches over persistent local caches", file)
		}
	}
}

func TestReleaseIsolatesDockerCredentials(t *testing.T) {
	content := readRepositoryFile(t, ".github/workflows/release.yml")
	for _, job := range []string{
		"docker-smoke", "prebuilt-image-smoke", "publication-preflight", "publish-images",
		"post-publish-image-smoke", "post-publish-verify", "promote-image-channels", "reconcile-publication",
	} {
		block := workflowJobBlock(t, content, job)
		setup := workflowStepBlock(t, block, "Isolate Docker credentials")
		if !strings.Contains(setup, `mktemp -d "${RUNNER_TEMP}/docker-config.XXXXXX"`) ||
			!strings.Contains(setup, `echo "DOCKER_CONFIG=${docker_config}" >> "${GITHUB_ENV}"`) {
			t.Errorf("%s can overwrite the host Docker credentials", job)
		}
	}
	build := workflowJobBlock(t, content, "build-binaries")
	if !strings.Contains(build, "runs-on: ${{ matrix.runner }}") ||
		!strings.Contains(workflowStepBlock(t, build, "Build release binary"), "shell: bash") {
		t.Fatal("cross-platform binary build must select its runner and use an explicit bash shell")
	}
}

func TestReleaseUsesSelfHostedValidationAndHostedPublicationRunners(t *testing.T) {
	content := readRepositoryFile(t, ".github/workflows/release.yml")
	for job, runner := range map[string]string{
		"static-checks": "[self-hosted, macOS, ARM64]",
		"race-tests":    "[self-hosted, Linux, ARM64]",
		"race-cpa":      "[self-hosted, Linux, ARM64]",
	} {
		block := workflowJobBlock(t, content, job)
		if !strings.Contains(block, "runs-on: "+runner) {
			t.Errorf("%s is not assigned to %s", job, runner)
		}
	}
	if count := strings.Count(content, "self-hosted"); count != 3 {
		t.Fatalf("release workflow contains %d self-hosted runner assignments, want 3", count)
	}
	for _, job := range []string{
		"validate-tag", "verify-and-build-web", "package-metadata", "package-checksums", "docker-smoke",
		"publication-preflight", "publish-images", "publish-github", "post-publish-image-smoke",
		"post-publish-verify", "promote-image-channels", "reconcile-publication",
	} {
		block := workflowJobBlock(t, content, job)
		if !strings.Contains(block, "runs-on: ubuntu-24.04") {
			t.Errorf("%s is not assigned to the GitHub-hosted Ubuntu runner", job)
		}
	}
	for _, test := range []struct {
		job      string
		required []string
	}{
		{
			job: "build-binaries",
			required: []string{
				"runner: ubuntu-24.04\n            goarch: amd64",
				"runner: ubuntu-24.04\n            goarch: arm64",
			},
		},
		{
			job: "prebuilt-image-smoke",
			required: []string{
				"runner: ubuntu-24.04\n          - arch: arm64",
				"runner: ubuntu-24.04-arm",
			},
		},
	} {
		block := workflowJobBlock(t, content, test.job)
		for _, required := range test.required {
			if !strings.Contains(block, required) {
				t.Errorf("%s does not include %s", test.job, required)
			}
		}
	}
	publish := workflowJobBlock(t, content, "publish-images")
	qemu := workflowStepBlock(t, publish, "Set up QEMU")
	if !strings.Contains(qemu, "platforms: arm64") {
		t.Fatal("AMD64 hosted image publisher must enable ARM64 emulation for the other target")
	}
	publishGitHub := workflowStepBlock(t, workflowJobBlock(t, content, "publish-github"), "Create or update GitHub Release draft")
	for _, required := range []string{"preserve_order: true", "overwrite_files: false"} {
		if !strings.Contains(publishGitHub, required) {
			t.Errorf("GitHub Release asset upload does not contain %q", required)
		}
	}
}
