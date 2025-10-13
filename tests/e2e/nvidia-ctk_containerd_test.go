/*
 * Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package e2e

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pelletier/go-toml"
)

const (
	// restartContainerdScript restarts containerd and waits for it to be ready
	restartContainerdScript = `
systemctl restart containerd
sleep 2
containerd --version
`
	// waitForContainerdScript waits for containerd to be ready
	waitForContainerdScript = `
for i in $(seq 1 10); do
	if ctr version > /dev/null 2>&1; then
		echo "containerd is ready"
		exit 0
	fi
	echo "Waiting for containerd to be ready..."
	sleep 1
done
echo "containerd failed to start"
exit 1
`
)

// containerdTestEnv defines the test environment for different containerd
// versions
type containerdTestEnv struct {
	name          string
	image         string
	configVersion int64
	pluginName    string
	// TODO: We could read this from the original config.
	cdiEnabledByDefault bool
}

// Define both containerd versions to test
var containerdEnvs = []*containerdTestEnv{
	{
		name:          "containerd-1.7",
		image:         "kindest/node:v1.30.0@sha256:047357ac0cfea04663786a612ba1eaba9702bef25227a794b52890dd8bcd692e",
		configVersion: 2,
		pluginName:    "io.containerd.grpc.v1.cri",
	},
	{
		name:          "containerd-2.1",
		image:         "docker.io/kindest/base:v20250521-31a79fd4",
		configVersion: 3,
		pluginName:    "io.containerd.cri.v1.runtime",
		// containerd >= 2.0 has CDI enabled by default
		cdiEnabledByDefault: true,
	},
}

type toolkitConfig struct {
	setAsDefault bool
	cdiEnabled   bool
}

var toolkitConfigVariants = []*toolkitConfig{
	{
		setAsDefault: true,
		cdiEnabled:   true,
	},
	{
		setAsDefault: false,
		cdiEnabled:   true,
	},
	{
		setAsDefault: false,
		cdiEnabled:   false,
	},
}

type testConfig struct {
	*containerdTestEnv
	*toolkitConfig
}

func (c *testConfig) name() string {
	return fmt.Sprintf("%s-default=%v-cdi=%v", c.containerdTestEnv.name, c.setAsDefault, c.cdiEnabled)
}

// Integration tests for containerd drop-in config functionality.
// These tests verify that nvidia-ctk runtime configure correctly applies
// configuration changes while preserving existing settings. The preservation
// validation is critical for containerd < 2.1 where plugin merge behavior
// (commit 598c632) requires duplicating the CRI plugin section.
var _ = Describe("containerd", Ordered, ContinueOnFailure, Label("container-runtime"), func() {
	var testsConfigs []*testConfig
	for _, env := range containerdEnvs {
		for _, variant := range toolkitConfigVariants {
			config := &testConfig{
				env,
				variant,
			}
			testsConfigs = append(testsConfigs, config)
		}
	}

	// Run all tests for each testConfig
	for _, tc := range testsConfigs {
		Context(tc.name(), Ordered, func() {
			var (
				nestedContainerRunner Runner
				containerName         = "nvctk-e2e-containerd-tests"
				baselineConfig        *toml.Tree
			)

			// restartContainerdAndWait restarts containerd and waits for it
			// to be ready
			restartContainerdAndWait := func(runner Runner) error {
				_, _, err := runner.Run(restartContainerdScript)
				if err != nil {
					return fmt.Errorf("failed to restart containerd: %w", err)
				}

				_, _, err = runner.Run(waitForContainerdScript)
				if err != nil {
					return fmt.Errorf("containerd did not become ready after restart")
				}
				return nil
			}

			BeforeAll(func(ctx context.Context) {
				var err error

				// Create the nested container with the global cache mounted
				nestedContainerRunner, err = NewNestedContainerRunner(runner, tc.image, false, containerName, localCacheDir, false)
				Expect(err).ToNot(HaveOccurred())

				// Backup original containerd configuration
				_, _, err = nestedContainerRunner.Run(`
			# Backup the original conf.d directory
			if [ -d /etc/containerd/conf.d ]; then
				cp -r /etc/containerd/conf.d /tmp/containerd-conf.d.backup
			fi

			# Backup the original config.toml
			if [ -f /etc/containerd/config.toml ]; then
				cp /etc/containerd/config.toml /tmp/containerd-config.toml.backup
			fi
		`)
				Expect(err).ToNot(HaveOccurred(), "Failed to backup containerd configuration")

				// Restart containerd to ensure clean state before capturing baseline
				err = restartContainerdAndWait(nestedContainerRunner)
				Expect(err).ToNot(HaveOccurred(), "Failed to restart containerd before baseline capture")

				// CAPTURE BASELINE: Get config BEFORE any modifications
				baselineOutput, _, err := nestedContainerRunner.Run("containerd config dump")
				Expect(err).ToNot(HaveOccurred(), "Failed to dump baseline configuration")

				baselineConfig, err = parseContainerdConfig(baselineOutput)
				Expect(err).ToNot(HaveOccurred(), "Failed to parse baseline configuration")

				// Install the NVIDIA Container Toolkit packages
				_, _, err = toolkitInstaller.Install(nestedContainerRunner)
				Expect(err).ToNot(HaveOccurred(), "Failed to install toolkit for containerd")
			})

			AfterAll(func(ctx context.Context) {
				// Cleanup: remove the container
				_, _, err := runner.Run(fmt.Sprintf("docker rm -f %s 2>/dev/null || true", containerName))
				if err != nil {
					GinkgoLogr.Error(err, "failed to cleanup container", "container", containerName)
				}
			})

			It("should preserve existing configuration and apply only expected changes", func(ctx context.Context) {
				// Apply nvidia-ctk configuration
				cmd := []string{"nvidia-ctk runtime configure --runtime=containerd"}

				if tc.setAsDefault {
					cmd = append(cmd, "--set-as-default")
				}
				if tc.cdiEnabled {
					cmd = append(cmd, "--cdi.enabled")
				}

				GinkgoLogr.Info("Applying nvidia-ctk configuration", "cmd", strings.Join(cmd, " "))
				_, _, err := nestedContainerRunner.Run(strings.Join(cmd, " "))
				Expect(err).ToNot(HaveOccurred(), "Failed to configure containerd")

				// Restart containerd to apply merged configuration
				err = restartContainerdAndWait(nestedContainerRunner)
				Expect(err).ToNot(HaveOccurred(), "Failed to restart containerd")

				// Get merged configuration
				mergedOutput, _, err := nestedContainerRunner.Run("containerd config dump")
				Expect(err).ToNot(HaveOccurred(), "Failed to dump merged configuration")

				mergedConfig, err := parseContainerdConfig(mergedOutput)
				Expect(err).ToNot(HaveOccurred(), "Failed to parse merged configuration")

				// VALIDATE PRESERVATION
				err = validateConfigPreservation(baselineConfig, mergedConfig, tc)
				Expect(err).ToNot(HaveOccurred())
			})
		})
	}
})

// validateConfigPreservation checks that existing config is preserved
// and only expected changes are made
func validateConfigPreservation(baseline, merged *toml.Tree, tc *testConfig) error {
	// Get plugin configs for comparison
	baselinePlugin, err := getPluginConfig(baseline, tc.pluginName)
	if err != nil {
		return fmt.Errorf("failed to get baseline plugin config: %w", err)
	}

	mergedPlugin, err := getPluginConfig(merged, tc.pluginName)
	if err != nil {
		return fmt.Errorf("failed to get merged plugin config: %w", err)
	}

	// Get runtimes configuration from both baseline and merged configs
	baselineRuntimes, err := getRuntimesConfig(baselinePlugin)
	if err != nil {
		return fmt.Errorf("failed to get baseline runtimes: %w", err)
	}

	mergedRuntimes, err := getRuntimesConfig(mergedPlugin)
	if err != nil {
		return fmt.Errorf("failed to get merged runtimes: %w", err)
	}

	// Check 1: Verify runc runtime is preserved (if it existed in baseline)
	runcInBaseline := false
	if _, exists := baselineRuntimes["runc"]; exists {
		runcInBaseline = true
	}

	// If runc was in baseline, it must still exist in merged
	if runcInBaseline {
		if _, exists := mergedRuntimes["runc"]; !exists {
			return fmt.Errorf("runc runtime was removed from configuration (regression: commit 598c632 logic broken for containerd plugin merge) - this typically affects containerd < 2.1")
		}
	}

	// Check 2: Verify all other baseline runtimes are preserved
	for runtimeName := range baselineRuntimes {
		if runtimeName == "nvidia" {
			continue // nvidia is expected to be added, so skip
		}
		if _, exists := mergedRuntimes[runtimeName]; !exists {
			return fmt.Errorf("runtime %q was removed from configuration - check if CRI plugin section is being properly preserved in drop-in file", runtimeName)
		}
	}

	// Check 3: NVIDIA runtime must be added
	nvidiaRuntime, exists := mergedRuntimes["nvidia"]
	if !exists {
		return fmt.Errorf("nvidia runtime was not added")
	}

	nvidiaRuntimeMap, ok := nvidiaRuntime.(map[string]interface{})
	if !ok {
		return fmt.Errorf("nvidia runtime is not a map[string]interface{}, got %T", nvidiaRuntime)
	}

	// Validate nvidia runtime structure
	if err := validateRuntimeConfig(nvidiaRuntimeMap, "io.containerd.runc.v2", map[string]interface{}{
		"BinaryName":    "/usr/bin/nvidia-container-runtime",
		"SystemdCgroup": true,
	}); err != nil {
		return fmt.Errorf("nvidia runtime config invalid: %w", err)
	}

	// Check 4: Verify expected state changes
	defaultRuntime, _ := getDefaultRuntime(mergedPlugin)
	expectedDefault := "runc"
	if tc.setAsDefault {
		expectedDefault = "nvidia"
	}
	// Only validate if a default runtime is set
	if defaultRuntime != "" && defaultRuntime != expectedDefault {
		return fmt.Errorf("default_runtime_name: expected %q, got %q", expectedDefault, defaultRuntime)
	}

	cdiEnabled, _ := getCDIEnabled(mergedPlugin)
	expectedCDI := tc.cdiEnabledByDefault || tc.cdiEnabled
	if cdiEnabled != expectedCDI {
		return fmt.Errorf("enable_cdi: expected %v, got %v", expectedCDI, cdiEnabled)
	}

	// Check 5: Verify important CRI plugin settings are preserved
	// (snapshotter, registry mirrors, etc. - if they existed in baseline)
	if err := validateCRIPluginSettingsPreserved(baselinePlugin, mergedPlugin); err != nil {
		return fmt.Errorf("CRI plugin settings not preserved: %w", err)
	}

	return nil
}

// validateCRIPluginSettingsPreserved checks that key CRI plugin settings
// are preserved from baseline to merged config. This is especially important
// for containerd < 2.1 where plugins are merged by key rather than by
// content (see https://github.com/containerd/containerd/issues/5837).
func validateCRIPluginSettingsPreserved(baseline, merged *toml.Tree) error {
	// Check snapshotter setting
	baselineSnapshotter := baseline.GetPath([]string{"containerd", "snapshotter"})
	mergedSnapshotter := merged.GetPath([]string{"containerd", "snapshotter"})

	if baselineSnapshotter != nil && baselineSnapshotter != mergedSnapshotter {
		return fmt.Errorf("snapshotter changed: %v -> %v", baselineSnapshotter, mergedSnapshotter)
	}

	// Check registry configuration exists if it existed in baseline
	baselineRegistry := baseline.Get("registry")
	if baselineRegistry != nil {
		mergedRegistry := merged.Get("registry")
		if mergedRegistry == nil {
			return fmt.Errorf("registry configuration was removed")
		}
		// Note: We don't deep-compare registry config as containerd may
		// normalize it, but at least the section should exist
	}

	// Check CNI configuration exists if it existed in baseline
	baselineCNI := baseline.Get("cni")
	if baselineCNI != nil {
		mergedCNI := merged.Get("cni")
		if mergedCNI == nil {
			return fmt.Errorf("cni configuration was removed")
		}
	}

	return nil
}

// parseContainerdConfig parses the containerd config dump output into a TOML
// tree
func parseContainerdConfig(output string) (*toml.Tree, error) {
	return toml.Load(output)
}

// getPluginConfig navigates to the appropriate plugin configuration based on
// containerd version
func getPluginConfig(tree *toml.Tree, pluginName string) (*toml.Tree, error) {
	plugins := tree.Get("plugins")
	if plugins == nil {
		return nil, fmt.Errorf("plugins section not found")
	}

	pluginTree := tree.GetPath([]string{"plugins", pluginName})
	if pluginTree == nil {
		return nil, fmt.Errorf("plugin %v not found", pluginName)
	}

	if pt, ok := pluginTree.(*toml.Tree); ok {
		return pt, nil
	}
	return nil, fmt.Errorf("plugin config is not a TOML tree")
}

// getRuntimesConfig gets the runtimes configuration from the plugin config
func getRuntimesConfig(pluginConfig *toml.Tree) (map[string]interface{}, error) {
	runtimes := pluginConfig.GetPath([]string{"containerd", "runtimes"})
	if runtimes == nil {
		return nil, fmt.Errorf("runtimes section not found")
	}

	// Handle both map and *toml.Tree types
	switch v := runtimes.(type) {
	case map[string]interface{}:
		return v, nil
	case *toml.Tree:
		return v.ToMap(), nil
	default:
		return nil, fmt.Errorf("runtimes is not a map or toml.Tree, got %T", runtimes)
	}
}

// getCDIEnabled checks if CDI is enabled in the plugin configuration
func getCDIEnabled(pluginConfig *toml.Tree) (bool, error) {
	cdiEnabled := pluginConfig.Get("enable_cdi")
	if cdiEnabled == nil {
		return false, nil // CDI not configured, default is false
	}

	if enabled, ok := cdiEnabled.(bool); ok {
		return enabled, nil
	}

	return false, fmt.Errorf("enable_cdi is not a boolean")
}

// getDefaultRuntime gets the default runtime name from the containerd
// configuration
func getDefaultRuntime(pluginConfig *toml.Tree) (string, error) {
	defaultRuntime := pluginConfig.GetPath([]string{"containerd", "default_runtime_name"})
	if defaultRuntime == nil {
		return "", nil // No default runtime set
	}

	if runtime, ok := defaultRuntime.(string); ok {
		return runtime, nil
	}

	return "", fmt.Errorf("default_runtime_name is not a string")
}

// validateRuntimeConfig validates a specific runtime configuration
func validateRuntimeConfig(runtime map[string]interface{}, expectedType string, expectedOptions map[string]interface{}) error {
	// Check runtime type only if expectedType is specified
	if expectedType != "" {
		runtimeType, ok := runtime["runtime_type"].(string)
		if !ok {
			return fmt.Errorf("runtime_type not found or not a string")
		}
		if runtimeType != expectedType {
			return fmt.Errorf("expected runtime_type %s, got %s", expectedType, runtimeType)
		}
	}

	// Check options if provided
	if len(expectedOptions) > 0 {
		options, ok := runtime["options"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("options not found or not a map[string]interface{}")
		}

		// Use gomega matchers for validation
		for key, expectedValue := range expectedOptions {
			matcher := HaveKeyWithValue(key, expectedValue)
			success, err := matcher.Match(options)
			if err != nil {
				return fmt.Errorf("error matching option %s: %v", key, err)
			}
			if !success {
				return fmt.Errorf("option validation failed: %s", matcher.FailureMessage(options))
			}
		}
	}

	return nil
}
