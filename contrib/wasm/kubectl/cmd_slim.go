//go:build js && wasm && kubectl_slim

/*
Copyright 2026 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/kubectl/pkg/cmd/apiresources"
	"k8s.io/kubectl/pkg/cmd/apply"
	"k8s.io/kubectl/pkg/cmd/create"
	"k8s.io/kubectl/pkg/cmd/delete"
	"k8s.io/kubectl/pkg/cmd/describe"
	"k8s.io/kubectl/pkg/cmd/get"
	"k8s.io/kubectl/pkg/cmd/patch"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/cmd/version"
	"k8s.io/kubectl/pkg/cmd/wait"
)

// newKubectlCommand returns the mobile (low-memory) kubectl: only the
// subcommands the browser page and its typical shell use are linked in.
// explain is left out: it renders with text/template, which (like -o
// go-template, see _overlay/slim) turns off the linker's method dead-code
// elimination and costs ~40 MiB.
func newKubectlCommand(streams genericiooptions.IOStreams, _ []string) *cobra.Command {
	cmds := &cobra.Command{
		Use:   "kubectl",
		Short: "kubectl (mobile build: a subset of the upstream commands)",
		// Without Args, cobra reports unknown subcommands itself; with it the
		// root is selected and this explains why the command is missing.
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			var names []string
			for _, c := range cmd.Commands() {
				if c.IsAvailableCommand() {
					names = append(names, c.Name())
				}
			}
			return fmt.Errorf("kubectl %s is not available in the mobile kubectl (low-memory mode; localStorage kcp.lowmem=0 loads the full one). Available: %s",
				args[0], strings.Join(names, ", "))
		},
		Run:          func(cmd *cobra.Command, _ []string) { _ = cmd.Help() },
		SilenceUsage: true,
	}

	flags := cmds.PersistentFlags()
	kubeConfigFlags := genericclioptions.NewConfigFlags(true).
		WithDeprecatedPasswordFlag().
		WithDiscoveryBurst(300).
		WithDiscoveryQPS(50.0).
		WithWarningPrinter(streams)
	kubeConfigFlags.AddFlags(flags)
	matchVersionKubeConfigFlags := cmdutil.NewMatchVersionFlags(kubeConfigFlags)
	matchVersionKubeConfigFlags.AddFlags(flags)
	f := cmdutil.NewFactory(matchVersionKubeConfigFlags)

	cmds.AddCommand(
		get.NewCmdGet("kubectl", f, streams),
		apply.NewCmdApply("kubectl", f, streams),
		create.NewCmdCreate(f, streams),
		patch.NewCmdPatch(f, streams),
		delete.NewCmdDelete(f, streams),
		wait.NewCmdWait(f, streams),
		describe.NewCmdDescribe("kubectl", f, streams),
		version.NewCmdVersion(f, streams),
		apiresources.NewCmdAPIResources(f, streams),
		apiresources.NewCmdAPIVersions(f, streams),
	)
	return cmds
}
