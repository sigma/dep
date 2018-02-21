// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"github.com/golang/dep"
	"github.com/golang/dep/gps"
	"github.com/golang/dep/gps/pkgtree"
	toml "github.com/pelletier/go-toml"
	"github.com/pkg/errors"
)

const ManifestName = "Gows.toml"

func (cmd *workspaceCommand) Name() string { return "workspace" }
func (cmd *workspaceCommand) Args() string {
	return "[-dry-run] [-v]"
}
func (cmd *workspaceCommand) ShortHelp() string { return "" }
func (cmd *workspaceCommand) LongHelp() string  { return "" }
func (cmd *workspaceCommand) Hidden() bool      { return false }

func (cmd *workspaceCommand) Register(fs *flag.FlagSet) {
	fs.BoolVar(&cmd.dryRun, "dry-run", false, "only report the changes that would be made")
}

type workspaceCommand struct {
	dryRun bool
}

type Manifest struct {
	Packages     []rawPackage
	PruneOptions gps.CascadingPruneOptions
}

func readManifest(r io.Reader) (*Manifest, error) {
	buf := &bytes.Buffer{}
	_, err := buf.ReadFrom(r)
	if err != nil {
		return nil, errors.Wrap(err, "unable to read byte stream")
	}

	raw := rawManifest{}
	err = toml.Unmarshal(buf.Bytes(), &raw)
	if err != nil {
		return nil, errors.Wrap(err, "unable to parse the manifest as TOML")
	}

	m := fromRawManifest(raw)
	if err != nil {
		return nil, err
	}

	return m, nil
}

func fromRawManifest(raw rawManifest) *Manifest {
	return &Manifest{
		Packages: raw.Packages,
		PruneOptions: gps.CascadingPruneOptions{
			DefaultOptions:    (gps.PruneNestedVendorDirs | gps.PruneGoTestFiles | gps.PruneUnusedPackages),
			PerProjectOptions: make(map[gps.ProjectRoot]gps.PruneOptionSet),
		},
	}
}

func NewManifest(root string) *Manifest {
	mp := filepath.Join(root, ManifestName)
	mf, _ := os.Open(mp)
	defer mf.Close()
	m, _ := readManifest(mf)
	return m
}

func NewLock(root string) *dep.Lock {
	mp := filepath.Join(root, dep.LockName)
	mf, err := os.Open(mp)
	if err != nil {
		return nil
	}
	defer mf.Close()
	l, _ := dep.ReadLock(mf)
	return l
}

func (m *Manifest) getProjects(ctx *dep.Ctx) ([]*dep.Project, error) {
	projects := make([]*dep.Project, len(m.Packages))
	// TODO(yhodique) ugh, that working dir dance is disgusting
	wd := ctx.WorkingDir
	for i, pck := range m.Packages {
		ctx.WorkingDir = filepath.Join(wd, pck.Path)
		p, _ := ctx.LoadProject()
		projects[i] = p
	}
	ctx.WorkingDir = wd
	return projects, nil
}

type rawManifest struct {
	Packages []rawPackage `toml:"package,omitempty"`
}

type rawPackage struct {
	Name string `toml:"name"`
	Path string `toml:"path"`
}

type Workspace struct {
	AbsRoot  string
	Lock     *dep.Lock
	Manifest *Manifest
	Projects []*dep.Project
}

func NewWorkspace(ctx *dep.Ctx) (*Workspace, error) {
	m := NewManifest(ctx.WorkingDir)
	l := NewLock(ctx.WorkingDir)
	projects, err := m.getProjects(ctx)

	return &Workspace{
		AbsRoot:  ctx.WorkingDir,
		Lock:     l,
		Manifest: m,
		Projects: projects,
	}, err
}

func (w *Workspace) DependencyConstraints() gps.ProjectConstraints {
	constraints := make(gps.ProjectConstraints)

	for _, p := range w.Projects {
		extra := p.Manifest.DependencyConstraints()
		for root, props := range extra {
			p, ok := constraints[root]
			if ok {
				p.Constraint = p.Constraint.Intersect(props.Constraint)
			} else {
				constraints[root] = props
			}
		}
	}

	return constraints
}

func (w *Workspace) Overrides() gps.ProjectConstraints {
	constraints := make(gps.ProjectConstraints)

	for _, p := range w.Projects {
		extra := p.Manifest.Overrides()
		for root, props := range extra {
			p, ok := constraints[root]
			if ok {
				p.Constraint = p.Constraint.Intersect(props.Constraint)
			} else {
				constraints[root] = props
			}
		}
	}
	return constraints
}

func (w *Workspace) IgnoredPackages() *pkgtree.IgnoredRuleset {
	ignored := make([]string, len(w.Manifest.Packages))
	for i, p := range w.Manifest.Packages {
		ignored[i] = fmt.Sprintf("%s/*", p.Name)
	}
	return pkgtree.NewIgnoredRuleset(ignored)
}

func (w *Workspace) RequiredPackages() map[string]bool {
	required := make(map[string]bool)
	for _, p := range w.Projects {
		for k, v := range p.Manifest.RequiredPackages() {
			required[k] = v
		}
	}
	return required
}

func (w *Workspace) MakeParams() gps.SolveParameters {
	params := gps.SolveParameters{
		RootDir:         w.AbsRoot,
		ProjectAnalyzer: dep.Analyzer{},
	}

	params.Manifest = w

	if w.Lock != nil {
		params.Lock = w.Lock
	}

	return params
}

func (w *Workspace) ParseRootPackageTree() (pkgtree.PackageTree, error) {
	tree := pkgtree.PackageTree{
		ImportRoot: w.AbsRoot,
		Packages:   make(map[string]pkgtree.PackageOrErr),
	}

	for _, p := range w.Projects {
		t, _ := p.ParseRootPackageTree()
		for imp, pack := range t.Packages {
			tree.Packages[imp] = pack
		}
	}
	return tree, nil
}

func (cmd *workspaceCommand) Run(ctx *dep.Ctx, args []string) error {
	w, err := NewWorkspace(ctx)
	if err != nil {
		return err
	}

	sm, err := ctx.SourceManager()
	if err != nil {
		return err
	}
	sm.UseDefaultSignalHandling()
	defer sm.Release()

	for _, p := range w.Projects {
		if err := dep.ValidateProjectRoots(ctx, p.Manifest, sm); err != nil {
			return err
		}

	}

	params := w.MakeParams()
	if ctx.Verbose {
		params.TraceLogger = ctx.Err
	}

	params.RootPackageTree, err = w.ParseRootPackageTree()
	if err != nil {
		return err
	}

	for _, p := range w.Projects {
		if fatal, err := checkErrors(params.RootPackageTree.Packages, p.Manifest.IgnoredPackages()); err != nil {
			if fatal {
				return err
			} else if ctx.Verbose {
				ctx.Out.Println(err)
			}
		}
	}

	// Bare workspace doesn't take any args.
	if len(args) != 0 {
		return errors.New("dep workspace only takes spec arguments with -add or -update")
	}

	if err := ctx.ValidateParams(sm, params); err != nil {
		return err
	}

	solver, err := gps.Prepare(params, sm)
	if err != nil {
		return errors.Wrap(err, "prepare solver")
	}

	solution, err := solver.Solve(context.TODO())
	if err != nil {
		return handleAllTheFailuresOfTheWorld(err)
	}

	sw, err := dep.NewSafeWriter(nil, w.Lock, dep.LockFromSolution(solution), dep.VendorOnChanged, w.Manifest.PruneOptions)
	if err != nil {
		return err
	}
	if cmd.dryRun {
		return sw.PrintPreparedActions(ctx.Out, ctx.Verbose)
	}

	logger := ctx.Err
	if !ctx.Verbose {
		logger = log.New(ioutil.Discard, "", 0)
	}

	err = sw.Write(w.AbsRoot, sm, false, logger)
	if err != nil {
		return errors.Wrap(err, "grouped write of manifest, lock and vendor")
	}

	// TODO(yhodique) maybe do something less horrible?
	vendorPath := filepath.Join(w.AbsRoot, "vendor")
	for _, p := range w.Manifest.Packages {
		projectRoot := filepath.Join(w.AbsRoot, p.Path)
		relVendorPath, _ := filepath.Rel(projectRoot, vendorPath)
		_ = os.Symlink(relVendorPath, filepath.Join(projectRoot, "vendor"))

		vendorProjectPath := filepath.Join(vendorPath, p.Name)
		vendorProjectDirPath := filepath.Dir(vendorProjectPath)
		os.MkdirAll(vendorProjectDirPath, 0755)
		relVendorProjectPath, _ := filepath.Rel(vendorProjectDirPath, projectRoot)
		_ = os.Symlink(relVendorProjectPath, vendorProjectPath)
	}

	return nil
}
