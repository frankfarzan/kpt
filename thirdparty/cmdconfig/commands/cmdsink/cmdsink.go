// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package cmdsink

import (
	"github.com/GoogleContainerTools/kpt/internal/docs/generated/fndocs"
	"github.com/GoogleContainerTools/kpt/internal/pkg"
	"github.com/GoogleContainerTools/kpt/thirdparty/cmdconfig/commands/runner"
	"github.com/spf13/cobra"
	"sigs.k8s.io/kustomize/kyaml/kio"
)

// GetSinkRunner returns a command for Sink.
func GetSinkRunner(name string) *SinkRunner {
	r := &SinkRunner{}
	c := &cobra.Command{
		Use:     "sink DIR [flags]",
		Short:   fndocs.SinkShort,
		Long:    fndocs.SinkShort + "\n" + fndocs.SinkLong,
		Example: fndocs.SinkExamples,
		RunE:    r.runE,
	}
	r.Command = c
	return r
}

func NewCommand(name string) *cobra.Command {
	return GetSinkRunner(name).Command
}

// SinkRunner contains the run function
type SinkRunner struct {
	Command *cobra.Command
}

func (r *SinkRunner) runE(c *cobra.Command, args []string) error {
	dir := pkg.CurDir
	if len(args) > 0 {
		dir = args[0]
	}
	outputs := []kio.Writer{&kio.LocalPackageWriter{PackagePath: dir}}

	err := kio.Pipeline{
		Inputs:  []kio.Reader{&kio.ByteReader{Reader: c.InOrStdin()}},
		Outputs: outputs}.Execute()
	return runner.HandleError(c, err)
}
