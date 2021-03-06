// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmdrender

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/GoogleContainerTools/kpt/internal/errors"
	"github.com/GoogleContainerTools/kpt/internal/fnruntime"
	"github.com/GoogleContainerTools/kpt/internal/pkg"
	"github.com/GoogleContainerTools/kpt/internal/printer"
	"github.com/GoogleContainerTools/kpt/internal/types"
	"github.com/GoogleContainerTools/kpt/internal/util/printerutil"
	fnresult "github.com/GoogleContainerTools/kpt/pkg/api/fnresult/v1alpha2"
	kptfilev1alpha2 "github.com/GoogleContainerTools/kpt/pkg/api/kptfile/v1alpha2"
	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/kio/filters"
	"sigs.k8s.io/kustomize/kyaml/kio/kioutil"
	"sigs.k8s.io/kustomize/kyaml/sets"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// Executor hydrates a given pkg.
type Executor struct {
	PkgPath         string
	ResultsDirPath  string
	Output          io.Writer
	ImagePullPolicy fnruntime.ImagePullPolicy

	Printer printer.Printer
}

// Execute runs a pipeline.
func (e *Executor) Execute(ctx context.Context) error {
	const op errors.Op = "fn.render"
	disableCLIOutput := false
	if e.Output != nil {
		// disable output written to stdout in case of out-of-place hydration
		disableCLIOutput = true
	}

	pr := printer.FromContextOrDie(ctx)

	root, err := newPkgNode(e.PkgPath, nil)
	if err != nil {
		return errors.E(op, types.UniquePath(e.PkgPath), err)
	}

	// initialize hydration context
	hctx := &hydrationContext{
		root:             root,
		pkgs:             map[types.UniquePath]*pkgNode{},
		fnResults:        fnresult.NewResultList(),
		imagePullPolicy:  e.ImagePullPolicy,
		disableCLIOutput: disableCLIOutput,
	}

	resources, err := hydrate(ctx, root, hctx)
	if err != nil {
		// Note(droot): ignore the error in function result saving
		// to avoid masking the hydration error.
		// don't disable the CLI output in case of error
		_ = e.saveFnResults(ctx, hctx.fnResults, true, false)
		return errors.E(op, root.pkg.UniquePath, err)
	}

	if err = trackOutputFiles(hctx); err != nil {
		return err
	}

	// format resources before writing
	_, err = filters.FormatFilter{UseSchema: true}.Filter(resources)
	if err != nil {
		return err
	}

	if e.Output == nil {
		// the intent of the user is to modify resources in-place
		pkgWriter := &kio.LocalPackageReadWriter{PackagePath: string(root.pkg.UniquePath)}
		err = pkgWriter.Write(resources)
		if err != nil {
			return fmt.Errorf("failed to save resources: %w", err)
		}

		if err = pruneResources(hctx); err != nil {
			return err
		}
		pr.Printf("Successfully executed %d function(s) in %d package(s).\n", hctx.executedFunctionCnt, len(hctx.pkgs))
	} else {
		// the intent of the user is to write the resources to either stdout|unwrapped|<OUT_DIR>
		// so, write the resources to provided e.Output which will be written to appropriate destination by cobra layer
		writer := &kio.ByteWriter{
			Writer:                e.Output,
			KeepReaderAnnotations: true,
			WrappingAPIVersion:    kio.ResourceListAPIVersion,
			WrappingKind:          kio.ResourceListKind,
		}
		err = writer.Write(resources)
		if err != nil {
			return fmt.Errorf("failed to write resources: %w", err)
		}
	}

	return e.saveFnResults(ctx, hctx.fnResults, false, disableCLIOutput)
}

func (e *Executor) saveFnResults(ctx context.Context, fnResults *fnresult.ResultList, toStdErr, disableCLIOutput bool) error {
	resultsFile, err := fnruntime.SaveResults(e.ResultsDirPath, fnResults)
	if err != nil {
		return fmt.Errorf("failed to save function results: %w", err)
	}

	if disableCLIOutput {
		return nil
	}

	printerutil.PrintFnResultInfo(ctx, resultsFile, false, toStdErr)
	return nil
}

// hydrationContext contains bits to track state of a package hydration.
// This is sort of global state that is available to hydration step at
// each pkg along the hydration walk.
type hydrationContext struct {
	// root points to the root pkg of hydration graph
	root *pkgNode

	// pkgs refers to the packages undergoing hydration. pkgs are key'd by their
	// unique paths.
	pkgs map[types.UniquePath]*pkgNode

	// inputFiles is a set of filepaths containing input resources to the
	// functions across all the packages during hydration.
	// The file paths are relative to the root package.
	inputFiles sets.String

	// outputFiles is a set of filepaths containing output resources. This
	// will be compared with the inputFiles to identify files be pruned.
	outputFiles sets.String

	// executedFunctionCnt is the counter for functions that have been executed.
	executedFunctionCnt int

	// fnResults stores function results gathered
	// during pipeline execution.
	fnResults *fnresult.ResultList

	// imagePullPolicy controls the image pulling behavior.
	imagePullPolicy fnruntime.ImagePullPolicy

	// disableCLIOutput disables the cli output written to stdout
	// stderr is not affected by this
	disableCLIOutput bool
}

//
// pkgNode represents a package being hydrated. Think of it as a node in the hydration DAG.
//
type pkgNode struct {
	pkg *pkg.Pkg

	// state indicates if the pkg is being hydrated or done.
	state hydrationState

	// KRM resources that we have gathered post hydration for this package.
	// These inludes resources at this pkg as well all it's children.
	resources []*yaml.RNode
}

// newPkgNode returns a pkgNode instance given a path or pkg.
func newPkgNode(path string, p *pkg.Pkg) (pn *pkgNode, err error) {
	const op errors.Op = "pkg.read"

	if path == "" && p == nil {
		return pn, fmt.Errorf("missing package path %s or package", path)
	}
	if path != "" {
		p, err = pkg.New(path)
		if err != nil {
			return pn, errors.E(op, p.UniquePath, err)
		}
	}
	// Note: Ensuring the presence of Kptfile can probably be moved
	// to the lower level pkg abstraction, but not sure if that
	// is desired in all the cases. So revisit this.
	kf, err := p.Kptfile()
	if err != nil {
		return pn, errors.E(op, p.UniquePath, err)
	}

	if err := kf.Validate(p.UniquePath); err != nil {
		return pn, errors.E(op, p.UniquePath, err)
	}

	pn = &pkgNode{
		pkg:   p,
		state: Dry, // package starts in dry state
	}
	return pn, nil
}

// hydrationState represent hydration state of a pkg.
type hydrationState int

// constants for all the hydration states
const (
	Dry hydrationState = iota
	Hydrating
	Wet
)

func (s hydrationState) String() string {
	return []string{"Dry", "Hydrating", "Wet"}[s]
}

// hydrate hydrates given pkg and returns wet resources.
func hydrate(ctx context.Context, pn *pkgNode, hctx *hydrationContext) (output []*yaml.RNode, err error) {
	const op errors.Op = "pkg.render"

	curr, found := hctx.pkgs[pn.pkg.UniquePath]
	if found {
		switch curr.state {
		case Hydrating:
			// we detected a cycle
			err = fmt.Errorf("cycle detected in pkg dependencies")
			return output, errors.E(op, curr.pkg.UniquePath, err)
		case Wet:
			output = curr.resources
			return output, nil
		default:
			return output, errors.E(op, curr.pkg.UniquePath,
				fmt.Errorf("package found in invalid state %v", curr.state))
		}
	}
	// add it to the discovered package list
	hctx.pkgs[pn.pkg.UniquePath] = pn
	curr = pn
	// mark the pkg in hydrating
	curr.state = Hydrating

	relPath, err := curr.pkg.RelativePathTo(hctx.root.pkg)
	if err != nil {
		return nil, errors.E(op, curr.pkg.UniquePath, err)
	}

	var input []*yaml.RNode

	// determine sub packages to be hydrated
	subpkgs, err := curr.pkg.DirectSubpackages()
	if err != nil {
		return output, errors.E(op, curr.pkg.UniquePath, err)
	}
	// hydrate recursively and gather hydated transitive resources.
	for _, subpkg := range subpkgs {
		var transitiveResources []*yaml.RNode
		var subPkgNode *pkgNode

		if subPkgNode, err = newPkgNode("", subpkg); err != nil {
			return output, errors.E(op, subpkg.UniquePath, err)
		}

		transitiveResources, err = hydrate(ctx, subPkgNode, hctx)
		if err != nil {
			return output, errors.E(op, subpkg.UniquePath, err)
		}

		input = append(input, transitiveResources...)
	}

	// gather resources present at the current package
	currPkgResources, err := curr.pkg.LocalResources(false)
	if err != nil {
		return output, errors.E(op, curr.pkg.UniquePath, err)
	}

	// ensure input resource's paths are relative to root pkg.
	currPkgResources, err = adjustRelPath(currPkgResources, relPath)
	if err != nil {
		return nil, fmt.Errorf("adjust relative path: %w", err)
	}

	err = trackInputFiles(hctx, currPkgResources)
	if err != nil {
		return nil, err
	}

	// include current package's resources in the input resource list
	input = append(input, currPkgResources...)

	output, err = curr.runPipeline(ctx, hctx, input)
	if err != nil {
		return output, errors.E(op, curr.pkg.UniquePath, err)
	}

	// ensure generated resource's file path are relative to root pkg.
	output, err = adjustRelPath(output, relPath)
	if err != nil {
		return nil, fmt.Errorf("adjust relative path: %w", err)
	}

	// pkg is hydrated, mark the pkg as wet and update the resources
	curr.state = Wet
	curr.resources = output

	return output, err
}

// runPipeline runs the pipeline defined at current pkgNode on given input resources.
func (pn *pkgNode) runPipeline(ctx context.Context, hctx *hydrationContext, input []*yaml.RNode) ([]*yaml.RNode, error) {
	const op errors.Op = "pipeline.run"
	pr := printer.FromContextOrDie(ctx)
	// TODO: the DisplayPath is a relative file path. It cannot represent the
	// package structure. We should have function to get the relative package
	// path here.
	if !hctx.disableCLIOutput {
		pr.OptPrintf(printer.NewOpt().PkgDisplay(pn.pkg.DisplayPath), "\n")
	}

	if len(input) == 0 {
		return nil, nil
	}

	pl, err := pn.pkg.Pipeline()
	if err != nil {
		return nil, err
	}

	if pl.IsEmpty() {
		return input, nil
	}

	// perform runtime validation for pipeline
	if err := pn.pkg.ValidatePipeline(); err != nil {
		return nil, err
	}

	mutatedResources, err := pn.runMutators(ctx, hctx, input)
	if err != nil {
		return nil, errors.E(op, pn.pkg.UniquePath, err)
	}

	if err = pn.runValidators(ctx, hctx, mutatedResources); err != nil {
		return nil, errors.E(op, pn.pkg.UniquePath, err)
	}
	// print a new line after a pipeline running
	if !hctx.disableCLIOutput {
		pr.Printf("\n")
	}
	return mutatedResources, nil
}

// runMutators runs a set of mutators functions on given input resources.
func (pn *pkgNode) runMutators(ctx context.Context, hctx *hydrationContext, input []*yaml.RNode) ([]*yaml.RNode, error) {
	if len(input) == 0 {
		return input, nil
	}

	pl, err := pn.pkg.Pipeline()
	if err != nil {
		return nil, err
	}

	if len(pl.Mutators) == 0 {
		return input, nil
	}

	mutators, err := fnChain(ctx, hctx, pn.pkg.UniquePath, pl.Mutators)
	if err != nil {
		return nil, err
	}

	output := &kio.PackageBuffer{}
	// create a kio pipeline from kyaml library to execute the function chains
	mutation := kio.Pipeline{
		Inputs: []kio.Reader{
			&kio.PackageBuffer{Nodes: input},
		},
		Filters: mutators,
		Outputs: []kio.Writer{output},
	}
	err = mutation.Execute()
	if err != nil {
		return nil, err
	}
	hctx.executedFunctionCnt += len(mutators)
	return output.Nodes, nil
}

// runValidators runs a set of validator functions on input resources.
// We bail out on first validation failure today, but the logic can be
// improved to report multiple failures. Reporting multiple failures
// will require changes to the way we print errors
func (pn *pkgNode) runValidators(ctx context.Context, hctx *hydrationContext, input []*yaml.RNode) error {
	if len(input) == 0 {
		return nil
	}

	pl, err := pn.pkg.Pipeline()
	if err != nil {
		return err
	}

	if len(pl.Validators) == 0 {
		return nil
	}

	for i := range pl.Validators {
		fn := pl.Validators[i]
		validator, err := fnruntime.NewContainerRunner(ctx, &fn, pn.pkg.UniquePath, hctx.fnResults, hctx.imagePullPolicy, hctx.disableCLIOutput)
		if err != nil {
			return err
		}
		// validators are run on a copy of mutated resources to ensure
		// resources are not mutated.
		if _, err = validator.Filter(cloneResources(input)); err != nil {
			return err
		}
		hctx.executedFunctionCnt++
	}
	return nil
}

func cloneResources(input []*yaml.RNode) (output []*yaml.RNode) {
	for _, resource := range input {
		output = append(output, resource.Copy())
	}
	return
}

// path (location) of a KRM resources is tracked in a special key in
// metadata.annotation field. adjustRelPath updates that path annotation by prepending
// the given relPath to the current path annotation if it doesn't exist already.
// Resources are read from local filesystem or generated at a package level, so the
// path annotation in each resource points to path relative to that package.
// But the resources are written to the file system at the root package level, so
// the path annotation in each resources needs to be adjusted to be relative to the rootPkg.
func adjustRelPath(resources []*yaml.RNode, relPath string) ([]*yaml.RNode, error) {
	if relPath == "" {
		return resources, nil
	}
	for _, r := range resources {
		currPath, _, err := kioutil.GetFileAnnotations(r)
		if err != nil {
			return resources, err
		}
		// if currPath is relative to root pkg i.e. already has relPath, skip it
		if !strings.HasPrefix(currPath, relPath+"/") {
			newPath := path.Join(relPath, currPath)
			err = r.PipeE(yaml.SetAnnotation(kioutil.PathAnnotation, newPath))
			if err != nil {
				return resources, err
			}
		}
	}
	return resources, nil
}

// fnChain returns a slice of function runners given a list of functions defined in pipeline.
func fnChain(ctx context.Context, hctx *hydrationContext, pkgPath types.UniquePath, fns []kptfilev1alpha2.Function) ([]kio.Filter, error) {
	var runners []kio.Filter
	for i := range fns {
		fn := fns[i]
		r, err := fnruntime.NewContainerRunner(ctx, &fn, pkgPath, hctx.fnResults, hctx.imagePullPolicy, hctx.disableCLIOutput)
		if err != nil {
			return nil, err
		}
		runners = append(runners, r)
	}
	return runners, nil
}

// trackInputFiles records file paths of input resources in the hydration context.
func trackInputFiles(hctx *hydrationContext, input []*yaml.RNode) error {
	if hctx.inputFiles == nil {
		hctx.inputFiles = sets.String{}
	}
	for _, r := range input {
		path, _, err := kioutil.GetFileAnnotations(r)
		if err != nil {
			return fmt.Errorf("path annotation missing: %w", err)
		}
		hctx.inputFiles.Insert(path)
	}
	return nil
}

// trackOutputfiles records the file paths of output resources in the hydration
// context. It should be invoked post hydration.
func trackOutputFiles(hctx *hydrationContext) error {
	outputSet := sets.String{}

	for _, r := range hctx.root.resources {
		path, _, err := kioutil.GetFileAnnotations(r)
		if err != nil {
			return fmt.Errorf("path annotation missing: %w", err)
		}
		outputSet.Insert(path)
	}
	hctx.outputFiles = outputSet
	return nil
}

// pruneResources compares the input and output of the hydration and prunes
// resources that are no longer present in the output of the hydration.
func pruneResources(hctx *hydrationContext) error {
	filesToBeDeleted := hctx.inputFiles.Difference(hctx.outputFiles)
	for f := range filesToBeDeleted {
		if err := os.Remove(filepath.Join(string(hctx.root.pkg.UniquePath), f)); err != nil {
			return fmt.Errorf("failed to delete file: %w", err)
		}
	}
	return nil
}
