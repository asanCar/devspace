package helper

import (
	"os"
	"path/filepath"

	"github.com/loft-sh/devspace/pkg/devspace/config"

	"github.com/docker/cli/cli/command/image/build"
	"github.com/docker/docker/pkg/archive"
	"github.com/loft-sh/devspace/pkg/devspace/config/generated"
	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	"github.com/loft-sh/devspace/pkg/devspace/kubectl"
	"github.com/loft-sh/devspace/pkg/util/hash"
	"github.com/loft-sh/devspace/pkg/util/log"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

// BuildHelper is the helper class to store common functionality used by both the docker and kaniko builder
type BuildHelper struct {
	ImageConfigName string
	ImageConf       *latest.ImageConfig
	Config          config.Config

	DockerfilePath string
	ContextPath    string

	EngineName string
	ImageName  string
	ImageTags  []string
	Entrypoint []string
	Cmd        []string

	KubeClient kubectl.Client
}

// BuildHelperInterface is the interface the build helper uses to build an image
type BuildHelperInterface interface {
	BuildImage(absoluteContextPath string, absoluteDockerfilePath string, entrypoint []string, cmd []string, log log.Logger) error
}

// NewBuildHelper creates a new build helper for a certain engine
func NewBuildHelper(config config.Config, kubeClient kubectl.Client, engineName string, imageConfigName string, imageConf *latest.ImageConfig, imageTags []string) *BuildHelper {
	var (
		dockerfilePath, contextPath = GetDockerfileAndContext(imageConf)
		imageName                   = imageConf.Image
	)

	// Check if we should overwrite entrypoint
	var (
		entrypoint []string
		cmd        []string
	)

	if imageConf.Entrypoint != nil {
		entrypoint = imageConf.Entrypoint
	}
	if imageConf.Cmd != nil {
		cmd = imageConf.Cmd
	}

	return &BuildHelper{
		ImageConfigName: imageConfigName,
		ImageConf:       imageConf,

		DockerfilePath: dockerfilePath,
		ContextPath:    contextPath,

		ImageName:  imageName,
		ImageTags:  imageTags,
		EngineName: engineName,

		Entrypoint: entrypoint,
		Cmd:        cmd,
		Config:     config,

		KubeClient: kubeClient,
	}
}

// Build builds a new image
func (b *BuildHelper) Build(imageBuilder BuildHelperInterface, log log.Logger) error {
	// Get absolute paths
	absoluteDockerfilePath, err := filepath.Abs(b.DockerfilePath)
	if err != nil {
		return errors.Errorf("Couldn't determine absolute path for %s", b.DockerfilePath)
	}

	absoluteContextPath, err := filepath.Abs(b.ContextPath)
	if err != nil {
		return errors.Errorf("Couldn't determine absolute path for %s", b.ContextPath)
	}

	log.Infof("Building image '%s:%s' with engine '%s'", b.ImageName, b.ImageTags[0], b.EngineName)

	// Build Image
	err = imageBuilder.BuildImage(absoluteContextPath, absoluteDockerfilePath, b.Entrypoint, b.Cmd, log)
	if err != nil {
		return err
	}

	log.Done("Done processing image '" + b.ImageName + "'")
	return nil
}

// ShouldRebuild determines if the image should be rebuilt
func (b *BuildHelper) ShouldRebuild(cache *generated.CacheConfig, forceRebuild bool) (bool, error) {
	// if rebuild strategy is always, we return here
	if b.ImageConf.RebuildStrategy == latest.RebuildStrategyAlways {
		return true, nil
	}

	imageCache := cache.GetImageCache(b.ImageConfigName)

	// Hash dockerfile
	_, err := os.Stat(b.DockerfilePath)
	if err != nil {
		return false, errors.Errorf("Dockerfile %s missing: %v", b.DockerfilePath, err)
	}
	dockerfileHash, err := hash.Directory(b.DockerfilePath)
	if err != nil {
		return false, errors.Wrap(err, "hash dockerfile")
	}

	// Hash image config
	configStr, err := yaml.Marshal(*b.ImageConf)
	if err != nil {
		return false, errors.Wrap(err, "marshal image config")
	}

	imageConfigHash := hash.String(string(configStr))

	// Hash entrypoint
	entrypointHash := ""
	if len(b.Entrypoint) > 0 {
		for _, str := range b.Entrypoint {
			entrypointHash += str
		}
	}
	if len(b.Cmd) > 0 {
		for _, str := range b.Cmd {
			entrypointHash += str
		}
	}
	if entrypointHash != "" {
		entrypointHash = hash.String(entrypointHash)
	}

	// only rebuild Docker image when Dockerfile or context has changed since latest build
	mustRebuild := imageCache.Tag == "" || imageCache.DockerfileHash != dockerfileHash || imageCache.ImageConfigHash != imageConfigHash || imageCache.EntrypointHash != entrypointHash

	// Okay this check verifies if the previous deploy context was local kubernetes context where we didn't push the image and now have a kubernetes context where we probably push
	// or use another docker client (e.g. minikube <-> docker-desktop)
	if b.KubeClient != nil && cache.LastContext != nil && cache.LastContext.Context != b.KubeClient.CurrentContext() && kubectl.IsLocalKubernetes(cache.LastContext.Context) {
		mustRebuild = true
	}

	// Check if should consider context path changes for rebuilding
	if b.ImageConf.RebuildStrategy != latest.RebuildStrategyIgnoreContextChanges {
		// Hash context path
		contextDir, relDockerfile, err := build.GetContextFromLocalDir(b.ContextPath, b.DockerfilePath)
		if err != nil {
			return false, errors.Wrap(err, "get context from local dir")
		}

		relDockerfile = archive.CanonicalTarNameForPath(relDockerfile)
		excludes, err := ReadDockerignore(contextDir, relDockerfile)
		if err != nil {
			return false, errors.Errorf("Error reading .dockerignore: %v", err)
		}

		contextHash, err := hash.DirectoryExcludes(contextDir, excludes, false)
		if err != nil {
			return false, errors.Errorf("Error hashing %s: %v", contextDir, err)
		}

		mustRebuild = mustRebuild || imageCache.ContextHash != contextHash

		// TODO: This is not an ideal solution since there can be the issue that the user runs
		// devspace dev & the generated.yaml is written without ContextHash and on a subsequent
		// devspace deploy the image would be rebuild, because the ContextHash was empty and is
		// now different. However in this case it is probably better to save the context hash computing
		// time during devspace dev instead of always hashing the context path.
		if forceRebuild || mustRebuild {
			imageCache.ContextHash = contextHash
		}
	}

	if forceRebuild || mustRebuild {
		imageCache.DockerfileHash = dockerfileHash
		imageCache.ImageConfigHash = imageConfigHash
		imageCache.EntrypointHash = entrypointHash
	}

	return mustRebuild, nil
}
