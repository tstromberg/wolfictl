package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing/fstest"

	"chainguard.dev/apko/pkg/build/types"
	"github.com/chainguard-dev/clog"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	ggcrtypes "github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/wolfi-dev/wolfictl/pkg/dag"
	"github.com/wolfi-dev/wolfictl/pkg/internal/bundle"
)

func cmdBundle() *cobra.Command {
	cfg := global{}
	bcfg := bundleConfig{
		base: empty.Index,
	}

	cmd := &cobra.Command{
		Use:           "bundle",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			cfg.donech = make(chan *task, runtime.GOMAXPROCS(0))
			if cfg.signingKey == "" {
				cfg.signingKey = filepath.Join(cfg.dir, "local-melange.rsa")
			}
			if cfg.pipelineDir == "" {
				cfg.pipelineDir = filepath.Join(cfg.dir, "pipelines")
			}
			if cfg.outDir == "" {
				cfg.outDir = filepath.Join(cfg.dir, "packages")
			}

			if bcfg.baseRef != "" {
				ref, err := name.ParseReference(bcfg.baseRef, name.Insecure)
				if err != nil {
					return err
				}
				bcfg.base, err = remote.Index(ref)
				if err != nil {
					return err
				}
			}

			if bcfg.repo != "" {
				pusher, err := remote.NewPusher(remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithUserAgent("jon"))
				if err != nil {
					return err
				}
				bcfg.pusher = pusher

				bcfg.commonFiles, err = commonFS(os.DirFS(cfg.dir))
				if err != nil {
					return err
				}
			}

			return bundleAll(ctx, &cfg, &bcfg, args)
		},
	}

	cmd.Flags().StringVarP(&cfg.dir, "dir", "d", ".", "directory to search for melange configs")
	cmd.Flags().StringVar(&cfg.pipelineDir, "pipeline-dir", "", "directory used to extend defined built-in pipelines")
	cmd.Flags().StringVar(&cfg.runner, "runner", "docker", "which runner to use to enable running commands, default is based on your platform.")
	cmd.Flags().StringSliceVar(&cfg.archs, "arch", []string{"x86_64", "aarch64"}, "arch of package to build")
	cmd.Flags().BoolVar(&cfg.dryrun, "dry-run", false, "print commands instead of executing them")
	cmd.Flags().StringSliceVarP(&cfg.extraKeys, "keyring-append", "k", []string{"https://packages.wolfi.dev/os/wolfi-signing.rsa.pub"}, "path to extra keys to include in the build environment keyring")
	cmd.Flags().StringSliceVarP(&cfg.extraRepos, "repository-append", "r", []string{"https://packages.wolfi.dev/os"}, "path to extra repositories to include in the build environment")
	cmd.Flags().StringVar(&cfg.signingKey, "signing-key", "", "key to use for signing")
	cmd.Flags().StringVar(&cfg.namespace, "namespace", "wolfi", "namespace to use in package URLs in SBOM (eg wolfi, alpine)")
	cmd.Flags().StringVar(&cfg.outDir, "out-dir", "", "directory where packages will be output")
	cmd.Flags().StringVar(&cfg.cacheDir, "cache-dir", "./melange-cache/", "directory used for cached inputs")
	cmd.Flags().StringVar(&cfg.cacheSource, "cache-source", "", "directory or bucket used for preloading the cache")
	cmd.Flags().BoolVar(&cfg.generateIndex, "generate-index", true, "whether to generate APKINDEX.tar.gz")
	cmd.Flags().StringVar(&cfg.dst, "destination-repository", "", "repo where packages will eventually be uploaded, used to skip existing packages (currently only supports http)")
	cmd.Flags().StringVar(&bcfg.baseRef, "bundle-base", "", "base image used for melange build bundles")
	cmd.Flags().StringVar(&bcfg.repo, "bundle-repo", "", "where to push the bundles")

	return cmd
}

type bundleConfig struct {
	baseRef     string
	base        v1.ImageIndex
	repo        string
	commonFiles fs.FS
	pusher      *remote.Pusher
}

func bundleAll(ctx context.Context, cfg *global, bcfg *bundleConfig, args []string) error {
	var eg errgroup.Group

	var stuff *configStuff
	eg.Go(func() error {
		var err error
		stuff, err = walkConfigs(ctx, cfg)
		return err
	})

	var mu sync.Mutex
	cfg.exists = map[string]map[string]struct{}{}

	for _, arch := range cfg.archs {
		arch := arch

		eg.Go(func() error {
			// Logs will go here to mimic the wolfi Makefile.
			archDir := cfg.logdir(arch)
			if err := os.MkdirAll(archDir, os.ModePerm); err != nil {
				return fmt.Errorf("creating buildlogs directory: %w", err)
			}

			return nil
		})

		// If --destination-repository is set, we want to fetch and parse the APKINDEX concurrently with walking all the configs.
		eg.Go(func() error {
			exist, err := fetchIndex(ctx, cfg.dst, arch)
			if err != nil {
				return err
			}

			mu.Lock()
			defer mu.Unlock()

			cfg.exists[types.ParseArchitecture(arch).ToAPK()] = exist

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return err
	}

	newTask := func(pkg string) *task {
		// We should only hit these errors if dag.NewPackages is wrong.
		loadedCfg := stuff.pkgs.Config(pkg, true)
		if len(loadedCfg) == 0 {
			panic(fmt.Sprintf("package does not seem to exist: %s", pkg))
		}
		c := loadedCfg[0]
		if pkg != c.Package.Name {
			panic(fmt.Sprintf("mismatched package, got %q, want %q", c.Package.Name, pkg))
		}

		return &task{
			cfg:    cfg,
			bcfg:   bcfg,
			pkg:    pkg,
			ver:    c.Package.Version,
			epoch:  c.Package.Epoch,
			config: c,
			archs:  filterArchs(cfg.archs, c.Package.TargetArchitecture),
			cond:   sync.NewCond(&sync.Mutex{}),
			deps:   map[string]*task{},
		}
	}

	tasks := map[string]*task{}
	for _, pkg := range stuff.g.Packages() {
		if tasks[pkg] == nil {
			tasks[pkg] = newTask(pkg)
		}
		for k, v := range stuff.m {
			// The package list is in the form of "pkg:version",
			// but we only care about the package name.
			if strings.HasPrefix(k, pkg+":") {
				for _, dep := range v {
					d, _, _ := strings.Cut(dep.Target, ":")

					if tasks[d] == nil {
						tasks[d] = newTask(d)
					}
					tasks[pkg].deps[d] = tasks[d]
				}
			}
		}
	}

	if len(tasks) == 0 {
		return fmt.Errorf("no packages to build")
	}

	todos := tasks
	if len(args) != 0 {
		todos = make(map[string]*task, len(args))

		for _, arg := range args {
			t, ok := tasks[arg]
			if !ok {
				return fmt.Errorf("constraint %q does not exist", arg)
			}
			todos[arg] = t
		}
	}

	// We're ok with Info level from here on.
	log := clog.FromContext(ctx)

	built := map[string]*task{}

	var g errgroup.Group
	g.Go(func() error {
		defer close(cfg.donech)

		for _, todo := range todos {
			log.Infof("bundling %s", todo.pkg)
			if err := todo.bundle(ctx); err != nil {
				return err
			}
		}

		return nil
	})
	g.Go(func() error {
		for t := range cfg.donech {
			delete(todos, t.pkg)

			if t.bundled != nil {
				log.Infof("built %q", t.pkg)
				built[t.pkg] = t
			}
		}

		return nil
	})

	if err := g.Wait(); err != nil {
		return err
	}

	needed, err := stuff.g.Filter(func(pkg dag.Package) bool {
		_, ok := built[pkg.Name()]
		return ok
	})
	if err != nil {
		return err
	}

	m, err := needed.Graph.AdjacencyMap()
	if err != nil {
		return err
	}
	log.Infof("map is %d big", len(m))

	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	layer := static.NewLayer(b, ggcrtypes.MediaType("application/vnd.dev.wolfi.graph+json"))
	img, err := mutate.AppendLayers(mutate.MediaType(empty.Image, ggcrtypes.OCIManifestSchema1), layer)
	if err != nil {
		return err
	}

	// TODO: First one is map.
	addendums := []mutate.IndexAddendum{{Add: img}}

	for pkg, t := range built {
		addendums = append(addendums, mutate.IndexAddendum{
			Add: t.bundled,
			Descriptor: v1.Descriptor{
				Annotations: map[string]string{
					"dev.wolfi.bundle.package": pkg,
					"dev.wolfi.bundle.version": t.config.Package.Version,
					"dev.wolfi.bundle.epoch":   strconv.FormatUint(t.config.Package.Epoch, 10),
				},
			},
		})
	}

	idx := mutate.AppendManifests(empty.Index, addendums...)
	dst, err := name.ParseReference(fmt.Sprintf("%s/%s", bcfg.repo, "index"), name.Insecure)
	if err != nil {
		return err
	}

	log.Infof("pushing bundle to %s", dst.String())

	if err := bcfg.pusher.Push(ctx, dst, idx); err != nil {
		return fmt.Errorf("pushing %s: %w", dst, err)
	}

	digest, err := idx.Digest()
	if err != nil {
		return err
	}

	log.Infof("pushed bundle to %s", dst.Context().Digest(digest.String()).String())
	fmt.Println(dst.Context().Digest(digest.String()).String())

	return nil
}

func (t *task) bundle(ctx context.Context) error {
	log := clog.FromContext(ctx)
	log.Infof("bundle(%q)", t.pkg)

	if t.done {
		return nil
	}

	defer func() {
		// When we finish, wake up any goroutines that are waiting on us.
		t.done = true
		t.cfg.donech <- t
	}()

	for _, dep := range t.deps {
		if dep.done {
			continue
		}
		if err := dep.bundle(ctx); err != nil {
			return fmt.Errorf("bundling dep %q: %w", dep.pkg, err)
		}
	}

	needsBuild := map[string]bool{}
	needsIndex := map[string]bool{}

	for _, arch := range t.archs {
		apkFile := t.pkgver() + ".apk"
		apkPath := filepath.Join(t.cfg.outDir, arch, apkFile)

		// See if we already have the package indexed.
		if _, ok := t.cfg.exists[arch][apkFile]; ok {
			log.Infof("Skipping %s, already indexed", apkFile)
			continue
		}

		needsIndex[arch] = true

		// See if we already have the package built.
		if _, err := os.Stat(apkPath); err == nil {
			log.Infof("Skipping %s, already built", apkPath)
			continue
		}

		needsBuild[arch] = true
	}

	if len(needsBuild) == 0 && len(needsIndex) == 0 {
		t.skipped = true
		return nil
	}

	sdir, err := t.sourceDir()
	if err != nil {
		return err
	}

	if t.bcfg.repo != "" {
		entrypoints := map[types.Architecture]*bundle.Entrypoint{}

		for _, arch := range t.archs {
			flags := []string{
				"--arch=" + arch,
				"--env-file=" + envFile(arch),
				"--runner=" + t.cfg.runner,
				"--namespace=" + t.cfg.namespace,
				"--source-dir=" + sdir,
				"--signing-key=" + t.cfg.signingKey,
				"--pipeline-dir=" + t.cfg.pipelineDir,
			}

			for _, k := range t.cfg.extraKeys {
				flags = append(flags, "--keyring-append="+k)
			}

			for _, r := range t.cfg.extraRepos {
				flags = append(flags, "--repository-append="+r)
			}

			entrypoints[types.ParseArchitecture(arch)] = &bundle.Entrypoint{
				File:  t.config.Path,
				Flags: flags,
			}
		}

		srcfs, err := t.sourceFS(os.DirFS(t.cfg.dir))
		if err != nil {
			return err
		}

		bundled, err := bundle.New(t.bcfg.base, entrypoints, t.bcfg.commonFiles, srcfs)
		if err != nil {
			return err
		}

		t.bundled = bundled

		dst, err := name.ParseReference(fmt.Sprintf("%s/%s", t.bcfg.repo, t.pkgver()), name.Insecure)
		if err != nil {
			return err
		}

		if err := t.bcfg.pusher.Push(ctx, dst, bundled); err != nil {
			return err
		}

		log.Infof("pushed bundle to %s", dst.String())
	}

	return nil
}

func (t *task) sourceFS(dirfs fs.FS) (fs.FS, error) {
	sdir, err := t.sourceDir()
	if err != nil {
		return nil, err
	}

	mapfs := fstest.MapFS{}

	filename := t.config.Path

	data, err := fs.ReadFile(dirfs, filename)
	if err != nil {
		return nil, err
	}
	info, err := fs.Stat(dirfs, filename)
	if err != nil {
		return nil, err
	}

	mapfs[filename] = &fstest.MapFile{
		Data:    data,
		Mode:    info.Mode(),
		ModTime: info.ModTime(),
	}

	if err := fs.WalkDir(dirfs, sdir, func(p string, d fs.DirEntry, _ error) error {
		info, err := d.Info()
		if err != nil {
			return err
		}
		mapf := &fstest.MapFile{
			Mode:    info.Mode(),
			ModTime: info.ModTime(),
		}

		if !info.IsDir() {
			mapf.Data, err = fs.ReadFile(dirfs, p)
			if err != nil {
				return err
			}
		}

		mapfs[p] = mapf

		return nil
	}); err != nil {
		return nil, err
	}
	return mapfs, nil
}

func commonFS(dirfs fs.FS) (fs.FS, error) {
	mapfs := fstest.MapFS{}

	envs, err := fs.Glob(dirfs, "build-*.env")
	if err != nil {
		return nil, err
	}
	for _, name := range envs {
		data, err := fs.ReadFile(dirfs, name)
		if err != nil {
			return nil, err
		}
		info, err := fs.Stat(dirfs, name)
		if err != nil {
			return nil, err
		}

		mapfs[name] = &fstest.MapFile{
			Data:    data,
			Mode:    info.Mode(),
			ModTime: info.ModTime(),
		}
	}

	if err := fs.WalkDir(dirfs, "pipelines", func(p string, d fs.DirEntry, _ error) error {
		info, err := d.Info()
		if err != nil {
			return err
		}
		mapf := &fstest.MapFile{
			Mode:    info.Mode(),
			ModTime: info.ModTime(),
		}

		if !info.IsDir() {
			mapf.Data, err = fs.ReadFile(dirfs, p)
			if err != nil {
				return err
			}
		}

		mapfs[p] = mapf

		return nil
	}); err != nil {
		return nil, err
	}

	return mapfs, nil
}

func envFile(arch string) string {
	return fmt.Sprintf("build-%s.env", arch)
}
