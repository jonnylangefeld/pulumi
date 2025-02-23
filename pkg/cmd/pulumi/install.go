// Copyright 2016-2023, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/logging"

	"github.com/spf13/cobra"

	"github.com/pulumi/pulumi/pkg/v3/backend/display"
	"github.com/pulumi/pulumi/pkg/v3/engine"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/cmdutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
)

func newInstallCmd() *cobra.Command {
	var reinstall bool
	var noPlugins, noDependencies bool
	var useLanguageVersionTools bool

	cmd := &cobra.Command{
		Use:   "install",
		Args:  cmdutil.NoArgs,
		Short: "Install packages and plugins for the current program or policy pack.",
		Long: "Install packages and plugins for the current program or policy pack.\n" +
			"\n" +
			"This command is used to manually install packages and plugins required by your program or policy pack.",
		Run: cmdutil.RunFunc(func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			displayOpts := display.Options{
				Color: cmdutil.GetGlobalColorization(),
			}

			installPolicyPackDeps, err := shouldInstallPolicyPackDependencies()
			if err != nil {
				return err
			}
			if installPolicyPackDeps {
				// No project found, check if we are in a policy pack project and install the policy
				// pack dependencies if so.
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("getting the working directory: %w", err)
				}
				policyPackPath, err := workspace.DetectPolicyPackPathFrom(cwd)
				if err == nil && policyPackPath != "" {
					proj, _, root, err := readPolicyProject(policyPackPath)
					if err != nil {
						return err
					}
					return installPolicyPackDependencies(ctx, root, proj)
				}
			}

			// Load the project
			proj, root, err := readProject()
			if err != nil {
				return err
			}

			span := opentracing.SpanFromContext(ctx)
			projinfo := &engine.Projinfo{Proj: proj, Root: root}
			pwd, main, pctx, err := engine.ProjectInfoContext(
				projinfo,
				nil,
				cmdutil.Diag(),
				cmdutil.Diag(),
				false,
				span,
				nil,
			)
			if err != nil {
				return err
			}

			defer pctx.Close()

			// First make sure the language plugin is present.  We need this to load the required resource plugins.
			// TODO: we need to think about how best to version this.  For now, it always picks the latest.
			runtime := proj.Runtime
			programInfo := plugin.NewProgramInfo(pctx.Root, pwd, main, runtime.Options())
			lang, err := pctx.Host.LanguageRuntime(runtime.Name(), programInfo)
			if err != nil {
				return fmt.Errorf("load language plugin %s: %w", runtime.Name(), err)
			}

			if !noDependencies {
				if err = lang.InstallDependencies(plugin.InstallDependenciesRequest{
					Info:                    programInfo,
					UseLanguageVersionTools: useLanguageVersionTools,
				}); err != nil {
					return fmt.Errorf("installing dependencies: %w", err)
				}
			}

			if !noPlugins {
				// Compute the set of plugins the current project needs.
				installs, err := lang.GetRequiredPlugins(programInfo)
				if err != nil {
					return err
				}

				// Now for each kind, name, version pair, download it from the release website, and install it.
				for _, install := range installs {
					// PluginSpec.String() just returns the name and version, we want the kind too.
					label := fmt.Sprintf("%s plugin %s", install.Kind, install)

					// If the plugin already exists, don't download it unless --reinstall was passed.
					if !reinstall {
						if install.Version != nil {
							if workspace.HasPlugin(install) {
								logging.V(1).Infof("%s skipping install (existing == match)", label)
								continue
							}
						} else {
							if has, _ := workspace.HasPluginGTE(install); has {
								logging.V(1).Infof("%s skipping install (existing >= match)", label)
								continue
							}
						}
					}

					pctx.Diag.Infoerrf(diag.Message("", "%s installing"), label)

					// If we got here, actually try to do the download.
					withProgress := func(stream io.ReadCloser, size int64) io.ReadCloser {
						return workspace.ReadCloserProgressBar(stream, size, "Downloading plugin", displayOpts.Color)
					}
					retry := func(err error, attempt int, limit int, delay time.Duration) {
						pctx.Diag.Warningf(
							diag.Message("", "Error downloading plugin: %s\nWill retry in %v [%d/%d]"), err, delay, attempt, limit)
					}

					r, err := workspace.DownloadToFile(install, withProgress, retry)
					if err != nil {
						return fmt.Errorf("%s downloading from %s: %w", label, install.PluginDownloadURL, err)
					}
					defer func() {
						err := os.Remove(r.Name())
						if err != nil {
							pctx.Diag.Warningf(
								diag.Message("", "Error removing temporary file %s: %s"), r.Name(), err)
						}
					}()

					payload := workspace.TarPlugin(r)

					logging.V(1).Infof("%s installing tarball ...", label)
					if err = install.InstallWithContext(ctx, payload, reinstall); err != nil {
						return fmt.Errorf("installing %s: %w", label, err)
					}
				}
			}

			return nil
		}),
	}

	cmd.PersistentFlags().BoolVar(&reinstall,
		"reinstall", false, "Reinstall a plugin even if it already exists")
	cmd.PersistentFlags().BoolVar(&noPlugins,
		"no-plugins", false, "Skip installing plugins")
	cmd.PersistentFlags().BoolVar(&noDependencies,
		"no-dependencies", false, "Skip installing dependencies")
	cmd.PersistentFlags().BoolVar(&useLanguageVersionTools,
		"use-language-version-tools", false, "Use language version tools to setup and install the language runtime")

	return cmd
}

func shouldInstallPolicyPackDependencies() (bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return false, fmt.Errorf("getting the working directory: %w", err)
	}
	policyPackPath, err := workspace.DetectPolicyPackPathFrom(cwd)
	if err != nil {
		return false, fmt.Errorf("detecting policy pack path: %w", err)
	}
	if policyPackPath != "" {
		// There's a PulumiPolicy.yaml in cwd or a parent folder. The policy pack might be nested
		// within a project, or vice-vera, so we need to check if there's a Pulumi.yaml in a parent
		// folder.
		projectPath, err := workspace.DetectProjectPath()
		if err != nil {
			if errors.Is(err, workspace.ErrProjectNotFound) {
				// No project found, we should install the dependencies for the policy pack.
				return true, nil
			}
			return false, fmt.Errorf("detecting project path: %w", err)
		}
		// We have both a project and a policy pack. If the project path is a parent of the policy
		// pack path, we should install dependencies for the policy pack, otherwise we should
		// install dependencies for the project.
		baseProjectPath := filepath.Dir(projectPath)
		basePolicyPackPath := filepath.Dir(policyPackPath)
		return strings.Contains(basePolicyPackPath, baseProjectPath), nil
	}
	return false, nil
}
