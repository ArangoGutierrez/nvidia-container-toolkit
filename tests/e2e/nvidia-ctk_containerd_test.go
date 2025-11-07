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
	"github.com/onsi/gomega/types"
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

// containerdTestEnv defines the test environment for different containerd versions
type containerdTestEnv struct {
	name          string
	image         string
	configVersion int64
	pluginPath    string
}

// Define both containerd versions to test
var containerdEnvs = []containerdTestEnv{
	{
		name:          "containerd-1.7",
		image:         "kindest/node:v1.30.0@sha256:047357ac0cfea04663786a612ba1eaba9702bef25227a794b52890dd8bcd692e",
		configVersion: 2,
		pluginPath:    "io.containerd.grpc.v1.cri",
	},
	{
		name:          "containerd-2.1",
		image:         "docker.io/kindest/base:v20250521-31a79fd4",
		configVersion: 3,
		pluginPath:    "io.containerd.cri.v1.runtime",
	},
}

// Integration tests for containerd drop-in config functionality
var _ = Describe("containerd", Ordered, ContinueOnFailure, Label("container-runtime"), func() {
	// Run all tests for each containerd version
	for _, env := range containerdEnvs {
		Context(env.name, Ordered, func() {
			var (
				nestedContainerRunner Runner
				containerName         = "nvctk-e2e-containerd-tests-" + env.name
			)

			// ensureContainerdRunning starts containerd if not running and waits for it to be ready
			ensureContainerdRunning := func(runner Runner) error {
				_, _, err := runner.Run(`
					if ! systemctl is-active --quiet containerd; then
						systemctl start containerd
						sleep 2
					fi
				`)
				if err != nil {
					return fmt.Errorf("failed to start containerd: %w", err)
				}

				_, _, err = runner.Run(waitForContainerdScript)
				if err != nil {
					return fmt.Errorf("containerd did not become ready")
				}
				return nil
			}

			// restartContainerdAndWait restarts containerd and waits for it to be ready
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
				// TODO: This runner doesn't actually NEED GPU access.
				nestedContainerRunner, err = NewNestedContainerRunner(runner, env.image, false, containerName, localCacheDir, false)
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

				// Ensure containerd is running
				err = ensureContainerdRunning(nestedContainerRunner)
				Expect(err).ToNot(HaveOccurred(), "Failed to ensure containerd is running")

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

			BeforeEach(func(ctx context.Context) {
				// No setup needed - each test starts with the state from the previous test
			})

			AfterEach(func(ctx context.Context) {
				// Step 1: Restore containerd configuration from backup
				_, _, err := nestedContainerRunner.Run(`
			# Restore the original conf.d
			if [ -d /tmp/containerd-conf.d.backup ]; then
				rm -rf /etc/containerd/conf.d
				cp -r /tmp/containerd-conf.d.backup /etc/containerd/conf.d
			else
				# If no backup exists, just clean up
				rm -rf /etc/containerd/conf.d
				mkdir -p /etc/containerd/conf.d
			fi

			# Restore the original config.toml
			if [ -f /tmp/containerd-config.toml.backup ]; then
				cp /tmp/containerd-config.toml.backup /etc/containerd/config.toml
			else
				# No backup means there was no original config file, so remove it
				rm -f /etc/containerd/config.toml
			fi
		`)
				Expect(err).ToNot(HaveOccurred(), "Failed to restore containerd configuration")

				// Step 2: Restart containerd and wait for it to be ready
				err = restartContainerdAndWait(nestedContainerRunner)
				Expect(err).ToNot(HaveOccurred(), "Failed to restart containerd after configuration restore")
			})

			When("configuring containerd", func() {
				It("should add NVIDIA runtime using drop-in config and merge configurations correctly", func(ctx context.Context) {
					// Configure containerd using nvidia-ctk
					_, _, err := nestedContainerRunner.Run(`nvidia-ctk runtime configure --runtime=containerd --set-as-default --cdi.enabled`)
					Expect(err).ToNot(HaveOccurred(), "Failed to add NVIDIA runtime using drop-in config")

					// Verify drop-in config was created
					dropInConfigContents, _, err := nestedContainerRunner.Run("cat /etc/containerd/conf.d/99-nvidia.toml")
					Expect(err).ToNot(HaveOccurred(), "Drop-in config file was not created")
					Expect(dropInConfigContents).ToNot(BeEmpty(), "Drop-in config file is empty")

					// Verify main config has imports directive
					mainConfigContents, _, err := nestedContainerRunner.Run("cat /etc/containerd/config.toml")
					Expect(err).ToNot(HaveOccurred(), "Failed to read main config file")

					mainConfig, err := parseContainerdConfig(mainConfigContents)
					Expect(err).ToNot(HaveOccurred(), "Failed to parse main config file")

					mainConfigImports := mainConfig.Get("imports")
					Expect(mainConfigImports).ToNot(BeNil(), "Main config does not have imports directive")
					mainConfigImportsList, ok := mainConfigImports.([]interface{})
					Expect(ok).To(BeTrue(), "Main config imports is not a list")
					Expect(mainConfigImportsList).To(ContainElement(ContainSubstring("/etc/containerd/conf.d")), "Main config imports does not include conf.d directory")

					// Restart containerd
					err = restartContainerdAndWait(nestedContainerRunner)
					Expect(err).ToNot(HaveOccurred(), "Failed to restart containerd to apply merged configuration")

					// Verify merged config structure
					output, _, err := nestedContainerRunner.Run(`containerd config dump`)
					Expect(err).ToNot(HaveOccurred(), "Failed to dump merged configuration")

					// Parse the TOML output
					config, err := parseContainerdConfig(output)
					Expect(err).ToNot(HaveOccurred(), "Failed to parse merged configuration")

					// Verify config version
					Expect(config.Get("version")).To(Equal(env.configVersion))

					// Verify imports are present in the merged config
					imports := config.Get("imports")
					Expect(imports).ToNot(BeNil(), "Merged config does not have imports")
					importsList, ok := imports.([]interface{})
					Expect(ok).To(BeTrue(), "Merged config imports is not a list")
					Expect(len(importsList)).To(BeNumerically(">=", 1), "Merged config imports list is empty")
					if env.configVersion == 2 {
						Expect(importsList).To(ContainElement(ContainSubstring("/etc/containerd/conf.d/99-nvidia.toml")), "Merged config imports does not include nvidia drop-in config")
					} else {
						Expect(importsList).To(ContainElement(ContainSubstring("/etc/containerd/conf.d/*.toml")), "Merged config imports does not include conf.d directory")
					}

					// Get plugin configuration
					pluginConfig, err := getPluginConfig(config, env.configVersion)
					Expect(err).ToNot(HaveOccurred())

					// Verify CDI is enabled
					cdiEnabled, err := getCDIEnabled(pluginConfig)
					Expect(err).ToNot(HaveOccurred())
					Expect(cdiEnabled).To(BeTrue())

					// Verify default runtime
					defaultRuntime, err := getDefaultRuntime(pluginConfig)
					Expect(err).ToNot(HaveOccurred())
					Expect(defaultRuntime).To(Equal("nvidia"))

					// Verify runtime configuration
					runtimes, err := getRuntimesConfig(pluginConfig)
					Expect(err).ToNot(HaveOccurred())

					// Verify NVIDIA runtime exists with correct configuration
					nvidiaRuntime, exists := runtimes["nvidia"]
					Expect(exists).To(BeTrue())
					err = validateRuntimeConfig(nvidiaRuntime, "io.containerd.runc.v2", map[string]interface{}{
						"BinaryName":    "/usr/bin/nvidia-container-runtime",
						"SystemdCgroup": true,
					})
					Expect(err).ToNot(HaveOccurred())
				})

				It("should preserve runc as default when --set-as-default=false", func(ctx context.Context) {
					// Configure without setting as default
					_, _, err := nestedContainerRunner.Run(`nvidia-ctk runtime configure --runtime=containerd --set-as-default=false --cdi.enabled`)
					Expect(err).ToNot(HaveOccurred(), "Failed to configure containerd")

					// Verify drop-in created
					dropInConfigContents, _, err := nestedContainerRunner.Run("cat /etc/containerd/conf.d/99-nvidia.toml")
					Expect(err).ToNot(HaveOccurred())
					Expect(dropInConfigContents).ToNot(BeEmpty())

					// Restart and verify
					err = restartContainerdAndWait(nestedContainerRunner)
					Expect(err).ToNot(HaveOccurred())

					// Verify runc is still default
					expectedRuntimes := map[string]map[string]interface{}{
						"runc": {
							"runtime_type": "io.containerd.runc.v2",
						},
						"nvidia": {
							"runtime_type": "",
							"BinaryName":   "/usr/bin/nvidia-container-runtime",
						},
					}
					verifyRuntimeConfiguration(nestedContainerRunner, env, "runc", expectedRuntimes)
				})

				It("should set nvidia as default when --set-as-default=true", func(ctx context.Context) {
					// Configure with nvidia as default
					_, _, err := nestedContainerRunner.Run(`nvidia-ctk runtime configure --runtime=containerd --set-as-default --cdi.enabled`)
					Expect(err).ToNot(HaveOccurred(), "Failed to configure containerd")

					// Restart and verify
					err = restartContainerdAndWait(nestedContainerRunner)
					Expect(err).ToNot(HaveOccurred())

					// Verify nvidia is default
					expectedRuntimes := map[string]map[string]interface{}{
						"runc": {
							"runtime_type": "io.containerd.runc.v2",
						},
						"nvidia": {
							"runtime_type": "",
							"BinaryName":   "/usr/bin/nvidia-container-runtime",
						},
					}
					verifyRuntimeConfiguration(nestedContainerRunner, env, "nvidia", expectedRuntimes)
				})

				It("should add imports directive to main config when missing", func(ctx context.Context) {
					// Check if imports exist before
					configBefore, _, err := nestedContainerRunner.Run("cat /etc/containerd/config.toml")
					Expect(err).ToNot(HaveOccurred())
					hasImportsBefore := strings.Contains(configBefore, "imports")

					// Configure containerd
					_, _, err = nestedContainerRunner.Run(`nvidia-ctk runtime configure --runtime=containerd --cdi.enabled`)
					Expect(err).ToNot(HaveOccurred())

					// Verify main config now has imports
					configAfter, _, err := nestedContainerRunner.Run("cat /etc/containerd/config.toml")
					Expect(err).ToNot(HaveOccurred())

					if !hasImportsBefore {
						Expect(configAfter).To(ContainSubstring("imports"))
						Expect(configAfter).To(ContainSubstring("/etc/containerd/conf.d"))
					}

					// Verify imports work by checking config dump includes drop-in content
					err = restartContainerdAndWait(nestedContainerRunner)
					Expect(err).ToNot(HaveOccurred())

					output, _, err := nestedContainerRunner.Run(`containerd config dump`)
					Expect(err).ToNot(HaveOccurred())
					Expect(output).To(ContainSubstring("nvidia"))
				})
			})

			When("configuring containerd with custom runtime options", func() {
				BeforeEach(func(ctx context.Context) {
					// Create config with custom runtime options
					configContent := ""
					if env.configVersion == 2 {
						configContent = `version = 2
[plugins]
  [plugins."io.containerd.grpc.v1.cri"]
    [plugins."io.containerd.grpc.v1.cri".containerd]
      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes]
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
          runtime_type = "io.containerd.runc.v2"
          [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
            BinaryName = "/usr/local/bin/runc"
            Root = "/custom/runc/root"`
					} else {
						configContent = `version = 3
[plugins]
  [plugins."io.containerd.cri.v1.runtime"]
    [plugins."io.containerd.cri.v1.runtime".containerd]
      [plugins."io.containerd.cri.v1.runtime".containerd.runtimes]
        [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc]
          runtime_type = "io.containerd.runc.v2"
          [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc.options]
            BinaryName = "/usr/local/bin/runc"
            Root = "/custom/runc/root"`
					}
					_, _, err := nestedContainerRunner.Run(fmt.Sprintf(`
cat > /etc/containerd/config.toml <<'EOF'
%s
EOF
`, configContent))
					Expect(err).ToNot(HaveOccurred())
					err = restartContainerdAndWait(nestedContainerRunner)
					Expect(err).ToNot(HaveOccurred())
				})

				It("should preserve existing runtime options", func(ctx context.Context) {
					// Configure nvidia runtime
					_, _, err := nestedContainerRunner.Run(`nvidia-ctk runtime configure --runtime=containerd --cdi.enabled`)
					Expect(err).ToNot(HaveOccurred())

					err = restartContainerdAndWait(nestedContainerRunner)
					Expect(err).ToNot(HaveOccurred())

					// Verify custom runc options preserved
					output, _, err := nestedContainerRunner.Run(`containerd config dump`)
					Expect(err).ToNot(HaveOccurred())

					// Parse and verify specific options are preserved
					config, err := parseContainerdConfig(output)
					Expect(err).ToNot(HaveOccurred())

					pluginConfig, err := getPluginConfig(config, env.configVersion)
					Expect(err).ToNot(HaveOccurred())

					runtimes, err := getRuntimesConfig(pluginConfig)
					Expect(err).ToNot(HaveOccurred())

					// Verify runc still has custom options
					runcRuntime, exists := runtimes["runc"]
					Expect(exists).To(BeTrue())
					expectedOptions := map[string]interface{}{
						"BinaryName": "/usr/local/bin/runc",
						"Root":       "/custom/runc/root",
					}
					err = validateRuntimeConfig(runcRuntime, "io.containerd.runc.v2", expectedOptions)
					Expect(err).ToNot(HaveOccurred(), "Custom runc options not preserved")
				})
			})

			When("using containerd with version 3 configuration format", func() {
				It("should correctly add NVIDIA runtime to v3 config structure", func(ctx context.Context) {
					// This test only applies to containerd versions that support v3 config
					if env.configVersion != 3 {
						Skip("This test only applies to containerd with config version 3")
					}
					// Create a v3 containerd config
					_, _, err := nestedContainerRunner.Run(`
cat > /etc/containerd/config.toml <<'EOF'
version = 3

[plugins]
  [plugins."io.containerd.cri.v1.runtime"]
    [plugins."io.containerd.cri.v1.runtime".containerd]
      default_runtime_name = "runc"

      [plugins."io.containerd.cri.v1.runtime".containerd.runtimes]
        [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc]
          runtime_type = "io.containerd.runc.v2"

          [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc.options]
            BinaryName = "/usr/bin/runc"
            SystemdCgroup = true
EOF
`)
					Expect(err).ToNot(HaveOccurred())

					// Configure containerd using nvidia-ctk
					_, _, err = nestedContainerRunner.Run(`nvidia-ctk runtime configure --runtime=containerd --cdi.enabled`)
					Expect(err).ToNot(HaveOccurred(), "Failed to configure containerd")

					// Verify the drop-in config uses v3 format
					output, _, err := nestedContainerRunner.Run(`cat /etc/containerd/conf.d/99-nvidia.toml`)
					Expect(err).ToNot(HaveOccurred())

					// Parse drop-in config to verify it's v3
					dropinConfig, err := parseContainerdConfig(output)
					Expect(err).ToNot(HaveOccurred(), "Failed to parse drop-in config")
					Expect(dropinConfig.Get("version")).To(BeNumerically("==", 3), "Drop-in config should be version 3")

					// Restart containerd
					err = restartContainerdAndWait(nestedContainerRunner)
					Expect(err).ToNot(HaveOccurred(), "Failed to restart containerd")

					// Verify containerd loaded v3 config correctly
					output, _, err = nestedContainerRunner.Run(`containerd config dump`)
					Expect(err).ToNot(HaveOccurred())

					// Parse the TOML output
					config, err := parseContainerdConfig(output)
					Expect(err).ToNot(HaveOccurred(), "Failed to parse containerd config")

					// Verify version
					Expect(config.Get("version")).To(BeNumerically("==", 3), "Config should be version 3")

					// Get plugin configuration
					pluginConfig, err := getPluginConfig(config, env.configVersion)
					Expect(err).ToNot(HaveOccurred(), "Failed to get plugin config")

					// Get runtimes configuration
					runtimes, err := getRuntimesConfig(pluginConfig)
					Expect(err).ToNot(HaveOccurred(), "Failed to get runtimes config")

					// Verify existing runc runtime is preserved with its settings
					runcRuntime, exists := runtimes["runc"]
					Expect(exists).To(BeTrue(), "runc runtime should exist")
					expectedRuncOptions := map[string]interface{}{
						"BinaryName":    "/usr/bin/runc",
						"SystemdCgroup": true,
					}
					err = validateRuntimeConfig(runcRuntime, "io.containerd.runc.v2", expectedRuncOptions)
					Expect(err).ToNot(HaveOccurred(), "runc runtime validation failed")

					// Verify NVIDIA runtime is properly added
					nvidiaRuntime, exists := runtimes["nvidia"]
					Expect(exists).To(BeTrue(), "nvidia runtime should exist")
					expectedNvidiaOptions := map[string]interface{}{
						"BinaryName":    "/usr/bin/nvidia-container-runtime",
						"SystemdCgroup": true,
					}
					err = validateRuntimeConfig(nvidiaRuntime, "", expectedNvidiaOptions)
					Expect(err).ToNot(HaveOccurred(), "NVIDIA runtime validation failed")

					// Verify CDI is enabled
					cdiEnabled, err := getCDIEnabled(pluginConfig)
					Expect(err).ToNot(HaveOccurred(), "Failed to get CDI config")
					Expect(cdiEnabled).To(BeTrue(), "CDI should be enabled")
				})
			})

			When("containerd already uses import directives for modular configuration", func() {
				It("should preserve existing imports when adding NVIDIA drop-in config", func(ctx context.Context) {
					// Create a containerd config with existing imports
					customConfig := ""
					if env.configVersion == 2 {
						// containerd 1.7.x
						customConfig = `version = 2

[plugins]
  [plugins."io.containerd.grpc.v1.cri"]
    [plugins."io.containerd.grpc.v1.cri".cni]
      conf_template = "/etc/cni/net.d/10-containerd.conf"`
					} else {
						// containerd 2.x
						customConfig = `version = 3

[plugins]
  [plugins."io.containerd.cri.v1.images"]
    [plugins."io.containerd.cri.v1.images".registry]
      [plugins."io.containerd.cri.v1.images".registry.mirrors]
        [plugins."io.containerd.cri.v1.images".registry.mirrors."myregistry.io"]
          endpoint = ["https://myregistry.io"]`
					}

					mainConfig := fmt.Sprintf(`imports = ["/etc/containerd/conf.d/*.toml"]
version = %d`, env.configVersion)

					_, _, err := nestedContainerRunner.Run(fmt.Sprintf(`
# Create a custom config that will be imported
cat > /etc/containerd/conf.d/10-custom.toml <<'EOF'
%s
EOF

# Create main config with existing imports
cat > /etc/containerd/config.toml <<'EOF'
%s
EOF
`, customConfig, mainConfig))
					Expect(err).ToNot(HaveOccurred())

					// Configure containerd using nvidia-ctk
					_, _, err = nestedContainerRunner.Run(`nvidia-ctk runtime configure --runtime=containerd`)
					Expect(err).ToNot(HaveOccurred(), "Failed to configure containerd")

					// Restart containerd to load the new configuration
					err = restartContainerdAndWait(nestedContainerRunner)
					Expect(err).ToNot(HaveOccurred(), "Failed to restart containerd")

					// Verify all configs are merged correctly using containerd config dump
					output, _, err := nestedContainerRunner.Run(`containerd config dump`)
					Expect(err).ToNot(HaveOccurred())

					// Parse the TOML output
					config, err := parseContainerdConfig(output)
					Expect(err).ToNot(HaveOccurred(), "Failed to parse containerd config")

					// Verify imports are preserved
					if env.configVersion == 2 {
						// For containerd 1.7, verify the imports are listed
						err = validateImports(config, []string{"/etc/containerd/conf.d/10-custom.toml", "/etc/containerd/conf.d/99-nvidia.toml"}, true)
						Expect(err).ToNot(HaveOccurred(), "Import validation failed")
					} else {
						// For containerd 2.x, verify registry mirror from custom import is preserved
						imagesPlugin := config.GetPath([]string{"plugins", "io.containerd.cri.v1.images"})
						if imagesPlugin != nil {
							if imagesTree, ok := imagesPlugin.(*toml.Tree); ok {
								registry := imagesTree.Get("registry")
								if registry != nil {
									if registryTree, ok := registry.(*toml.Tree); ok {
										mirrors := registryTree.Get("mirrors")
										Expect(mirrors).ToNot(BeNil(), "Registry mirrors should exist")
									}
								}
							}
						}
					}

					// Get plugin configuration
					pluginConfig, err := getPluginConfig(config, env.configVersion)
					Expect(err).ToNot(HaveOccurred(), "Failed to get plugin config")

					// Get runtimes configuration
					runtimes, err := getRuntimesConfig(pluginConfig)
					Expect(err).ToNot(HaveOccurred(), "Failed to get runtimes config")

					// Verify NVIDIA runtime from drop-in is properly loaded
					nvidiaRuntime, exists := runtimes["nvidia"]
					Expect(exists).To(BeTrue(), "nvidia runtime should exist")
					expectedNvidiaOptions := map[string]interface{}{
						"BinaryName": "/usr/bin/nvidia-container-runtime",
					}
					err = validateRuntimeConfig(nvidiaRuntime, "", expectedNvidiaOptions)
					Expect(err).ToNot(HaveOccurred(), "NVIDIA runtime validation failed")
				})
			})
		}) // End Context for containerd version
	} // End for loop over containerd versions
})

// parseContainerdConfig parses the containerd config dump output into a TOML tree
func parseContainerdConfig(output string) (*toml.Tree, error) {
	return toml.Load(output)
}

// getPluginConfig navigates to the appropriate plugin configuration based on containerd version
func getPluginConfig(tree *toml.Tree, version int64) (*toml.Tree, error) {
	var pluginPath []string
	if version == 2 {
		pluginPath = []string{"plugins", "io.containerd.grpc.v1.cri"}
	} else {
		pluginPath = []string{"plugins", "io.containerd.cri.v1.runtime"}
	}

	plugins := tree.Get("plugins")
	if plugins == nil {
		return nil, fmt.Errorf("plugins section not found")
	}

	pluginTree := tree.GetPath(pluginPath)
	if pluginTree == nil {
		return nil, fmt.Errorf("plugin path %v not found", pluginPath)
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

// getDefaultRuntime gets the default runtime name from the containerd configuration
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
func validateRuntimeConfig(runtime interface{}, expectedType string, expectedOptions map[string]interface{}) error {
	runtimeMap, ok := runtime.(map[string]interface{})
	if !ok {
		return fmt.Errorf("runtime is not a map[string]interface{}, got %T", runtime)
	}

	// Check runtime type only if expectedType is specified
	if expectedType != "" {
		runtimeType, ok := runtimeMap["runtime_type"].(string)
		if !ok {
			return fmt.Errorf("runtime_type not found or not a string")
		}
		if runtimeType != expectedType {
			return fmt.Errorf("expected runtime_type %s, got %s", expectedType, runtimeType)
		}
	}

	// Check options if provided
	if len(expectedOptions) > 0 {
		options, ok := runtimeMap["options"].(map[string]interface{})
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

// validateImports validates the imports configuration
func validateImports(tree *toml.Tree, expectedImports []string, partialMatch bool) error {
	imports := tree.Get("imports")
	if imports == nil {
		if len(expectedImports) == 0 {
			return nil
		}
		return fmt.Errorf("imports not found")
	}

	importsList, ok := imports.([]interface{})
	if !ok {
		return fmt.Errorf("imports is not a list")
	}

	// Convert to string slice for easier validation
	importStrings := make([]string, 0, len(importsList))
	for _, imp := range importsList {
		if impStr, ok := imp.(string); ok {
			importStrings = append(importStrings, impStr)
		}
	}

	// Use gomega matchers for validation
	var matcher types.GomegaMatcher
	if partialMatch {
		// Check that all expected imports are present (but there may be more)
		matcher = ContainElements(expectedImports)
	} else {
		// Exact match - same elements regardless of order
		matcher = ConsistOf(expectedImports)
	}

	success, err := matcher.Match(importStrings)
	if err != nil {
		return fmt.Errorf("error matching imports: %v", err)
	}
	if !success {
		return fmt.Errorf("import validation failed: %s", matcher.FailureMessage(importStrings))
	}

	return nil
}

// verifyRuntimeConfiguration verifies the entire runtime configuration including default runtime, runtimes, and CDI
func verifyRuntimeConfiguration(runner Runner, env containerdTestEnv, expectedDefault string, expectedRuntimes map[string]map[string]interface{}) {
	output, _, err := runner.Run(`containerd config dump`)
	Expect(err).ToNot(HaveOccurred())

	config, err := parseContainerdConfig(output)
	Expect(err).ToNot(HaveOccurred())

	pluginConfig, err := getPluginConfig(config, env.configVersion)
	Expect(err).ToNot(HaveOccurred())

	// Verify default runtime
	defaultRuntime, err := getDefaultRuntime(pluginConfig)
	Expect(err).ToNot(HaveOccurred())
	Expect(defaultRuntime).To(Equal(expectedDefault))

	// Verify all expected runtimes
	runtimes, err := getRuntimesConfig(pluginConfig)
	Expect(err).ToNot(HaveOccurred())

	for name, expectedOptions := range expectedRuntimes {
		runtime, exists := runtimes[name]
		Expect(exists).To(BeTrue(), fmt.Sprintf("%s runtime should exist", name))

		runtimeType := expectedOptions["runtime_type"].(string)
		delete(expectedOptions, "runtime_type")

		err = validateRuntimeConfig(runtime, runtimeType, expectedOptions)
		Expect(err).ToNot(HaveOccurred(), fmt.Sprintf("%s runtime validation failed", name))
	}

	// Verify CDI is enabled
	cdiEnabled, err := getCDIEnabled(pluginConfig)
	Expect(err).ToNot(HaveOccurred())
	Expect(cdiEnabled).To(BeTrue())
}
